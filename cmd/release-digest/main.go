// Command release-digest turns GitHub releases into a condensed RSS digest.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"release-digest/internal/config"
	"release-digest/internal/github"
	"release-digest/internal/llm"
	"release-digest/internal/notify"
	"release-digest/internal/poll"
	"release-digest/internal/secret"
	"release-digest/internal/store"
	"release-digest/internal/summarize"
)

// llmSleep, when set, replaces the LLM client's retry backoff (for tests).
var llmSleep func(ctx context.Context, d time.Duration) bool

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Release Digest condenses GitHub release notes into one RSS feed.

Usage:
  release-digest run                 long-running: scheduler + HTTP server
  release-digest build               one pass, then exit
  release-digest summarize owner/repo@tag
                                     dry run: resolve notes, call the LLM, print the result; writes nothing
  release-digest notify test [--release owner/repo@tag]
                                     send a test alert; with --release, print the alert decision for a release instead
  release-digest notify ack          acknowledge expired alerts (clears /healthz's degraded state)
  release-digest validate [--w3c]    check the stored feed (optionally with the W3C validator)
  release-digest config lint         validate the config and secrets; exits non-zero on error
  release-digest healthcheck         probe /healthz (for Docker HEALTHCHECK)
  release-digest version

Every command accepts --config (default: release-digest.yaml, or $RELEASE_DIGEST_CONFIG).
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "config":
		if len(args) == 0 || args[0] != "lint" {
			fmt.Fprintln(stderr, "usage: release-digest config lint")
			return 2
		}
		return cmdConfigLint(args[1:], stdout, stderr)
	case "build":
		return cmdBuild(args, stdout, stderr)
	case "summarize":
		return cmdSummarize(args, stdout, stderr)
	case "notify":
		return cmdNotify(args, stdout, stderr)
	case "validate":
		return cmdValidate(args, stdout, stderr)
	case "run":
		return cmdRun(args, stdout, stderr)
	case "healthcheck":
		return cmdHealthcheck(args, stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "release-digest", version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "release-digest: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

// newFlags returns a flag set with the shared --config flag.
func newFlags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	def := os.Getenv(config.EnvConfig)
	if def == "" {
		def = "release-digest.yaml"
	}
	return fs, fs.String("config", def, "path to the config file")
}

func reportErrors(err error, stderr io.Writer) int {
	var errs config.Errors
	if errors.As(err, &errs) {
		for _, e := range errs {
			fmt.Fprintln(stderr, e.Error())
		}
		fmt.Fprintf(stderr, "%d error(s)\n", len(errs))
	} else {
		fmt.Fprintln(stderr, err)
	}
	return 1
}

func generator() string { return "release-digest/" + version }

// secrets are the loaded credentials (§4.3). Clients read them at request
// time, so a SIGHUP reload takes effect on the next request.
type secrets struct {
	LLM, GitHub, Ntfy, NtfyURL *secret.Value
}

// secretSpecs, in the order loadSecrets returns them.
var secretSpecs = []secret.Spec{secret.LLMAPIKey, secret.GitHubToken, secret.NtfyToken, secret.NtfyURL}

// Indexes into loadSecrets' result.
const (
	secLLM = iota
	secGitHub
	secNtfy
	secNtfyURL
)

// loadSecrets resolves every secret, returning all problems at once.
func loadSecrets(env secret.Env) ([]secret.Loaded, error) {
	var errs []error
	out := make([]secret.Loaded, len(secretSpecs))
	for i, spec := range secretSpecs {
		l, err := env.Load(spec)
		if err != nil {
			errs = append(errs, err)
		}
		out[i] = l
	}
	return out, errors.Join(errs...)
}

// env is everything a command needs once the config, secrets and store are open.
type env struct {
	cfg     *config.Config
	secrets secrets
	store   *store.Store
	log     *slog.Logger
	gh      *github.Client
	poller  *poll.Poller
	summ    *summarize.Summarizer
	alerts  *notify.Alerter
	outbox  *notify.Outbox

	// guardOK is set once the active GitHub token has passed the token guard
	// (§3.1). Until then no tick polls GitHub.
	guardOK atomic.Bool
}

// open loads the config and secrets and opens the store. On failure it
// reports to stderr and returns nil and the exit code.
func open(ctx context.Context, cfgPath string, stderr io.Writer) (*env, int) {
	cfg, err := config.Load(cfgPath, os.LookupEnv)
	if err != nil {
		return nil, reportErrors(err, stderr)
	}
	log := newLogger(cfg.Log, stderr)
	loaded, err := loadSecrets(secret.OS)
	if err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return nil, 1
	}
	if err := checkNtfyURL(cfg, loaded[secNtfyURL]); err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return nil, 1
	}
	e := &env{cfg: cfg, log: log, secrets: secrets{
		LLM: secret.NewValue(loaded[secLLM]), GitHub: secret.NewValue(loaded[secGitHub]),
		Ntfy: secret.NewValue(loaded[secNtfy]), NtfyURL: secret.NewValue(loaded[secNtfyURL]),
	}}
	logSecrets(log, loaded)
	if e.store, err = store.Open(ctx, cfg.StorePath); err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return nil, 1
	}
	e.gh = &github.Client{
		BaseURL:   cfg.GitHub.APIURL,
		Token:     func() secret.String { return e.secrets.GitHub.Get().Value },
		HTTP:      &http.Client{},
		Cache:     e.store,
		Timeout:   cfg.GitHub.RequestTimeout.D,
		UserAgent: generator(),
	}
	e.poller = &poll.Poller{GH: e.gh, Store: e.store, Cfg: cfg, Log: log}
	e.summ = &summarize.Summarizer{Store: e.store, Cfg: cfg, Log: log, LLM: &llm.Client{
		BaseURL:     cfg.LLM.BaseURL,
		Model:       cfg.LLM.Model,
		Key:         func() secret.String { return e.secrets.LLM.Get().Value },
		Headers:     cfg.LLM.Headers,
		Temperature: *cfg.LLM.Temperature,
		Timeout:     cfg.LLM.Timeout.D,
		HTTP:        &http.Client{},
		UserAgent:   generator(),
		Sleep:       llmSleep,
	}}
	if !e.summ.Enabled() {
		log.Warn("no llm.model configured: releases are published as excerpts, without summaries")
	}
	e.outbox = &notify.Outbox{Store: e.store, Log: log,
		Check: func(ctx context.Context, repo string) (github.Visibility, error) {
			return e.gh.CheckPublic(ctx, repo, 0)
		},
		Ntfy: &notify.Ntfy{
			URL:       e.ntfyURL,
			Token:     func() secret.String { return e.secrets.Ntfy.Get().Value },
			HTTP:      &http.Client{},
			UserAgent: generator(),
		}}
	e.alerts = &notify.Alerter{Store: e.store, GH: e.gh, Cfg: cfg, Log: log, Enabled: e.ntfyURL() != "", Kick: e.outbox.Kick}
	if !e.alerts.Enabled {
		log.Info("no ntfy URL configured: urgent security releases are marked in the feed but not alerted")
	}
	return e, 0
}

// ntfyURL is the ntfy server and topic, from the config or the ntfy url
// secret (§4.3); "" if alerts are off.
func (e *env) ntfyURL() string {
	if n := e.cfg.Notify.Ntfy; n != nil && n.URL != "" {
		return n.URL
	}
	return e.secrets.NtfyURL.Get().Value.Reveal()
}

// checkNtfyURL rejects an ntfy URL set in both the config and a secret, or
// a secret that isn't an http(s) URL. The value is never echoed: it holds
// the topic.
func checkNtfyURL(cfg *config.Config, l secret.Loaded) error {
	if !l.Present() {
		return nil
	}
	if n := cfg.Notify.Ntfy; n != nil && n.URL != "" {
		return fmt.Errorf("notify.ntfy.url and the ntfy url secret (%s) are both set; remove one", l.Source)
	}
	u, err := url.Parse(l.Value.Reveal())
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return fmt.Errorf("the ntfy url secret (%s) must be an http(s) URL with a topic, such as https://ntfy.sh/my-topic", l.Source)
	}
	return nil
}

// startGuard runs the token guard at startup (§3.1). A token that fails it
// is fatal. If the check can't run (GitHub unreachable), the service starts
// anyway, and each tick retries the guard before polling.
func (e *env) startGuard(ctx context.Context) error {
	err := e.checkGuard(ctx)
	var ge *github.GuardError
	if errors.As(err, &ge) {
		return err
	}
	return nil
}

// checkGuard runs the token guard on the active token unless it already
// passed, recording a pass.
func (e *env) checkGuard(ctx context.Context) error {
	if e.guardOK.Load() {
		return nil
	}
	tok := e.secrets.GitHub.Get()
	err := e.gh.Guard(ctx, tok.Value)
	switch {
	case err == nil:
		e.guardOK.Store(true)
		if tok.Present() {
			e.log.Info("github token checked for obvious private-repo access only (best effort): passed")
		}
	case isGuardError(err):
		e.log.Error("github token failed the token guard; GitHub is not polled", "err", err)
	default:
		e.log.Warn("github token guard could not run; GitHub is not polled until it does", "err", err)
	}
	return err
}

func isGuardError(err error) bool {
	var ge *github.GuardError
	return errors.As(err, &ge)
}

// logSecrets reports where each secret came from, never its value.
func logSecrets(log *slog.Logger, loaded []secret.Loaded) {
	for i, l := range loaded {
		log.Info("secret", "name", secretSpecs[i].Label, "source", l.Source)
	}
	if !loaded[secGitHub].Present() {
		log.Warn("no GitHub token: requests are unauthenticated and limited to 60 per hour")
	}
}

func newLogger(c config.Log, w io.Writer) *slog.Logger {
	var level slog.Level
	level.UnmarshalText([]byte(c.Level)) // validated by config
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
