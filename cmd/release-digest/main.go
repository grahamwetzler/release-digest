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
	"os"
	"sync/atomic"

	"release-digest/internal/config"
	"release-digest/internal/github"
	"release-digest/internal/poll"
	"release-digest/internal/secret"
	"release-digest/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Release Digest condenses GitHub release notes into one RSS feed.

Usage:
  release-digest run                 long-running: scheduler + HTTP server
  release-digest build               one pass, then exit
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
	LLM, GitHub, Ntfy *secret.Value
}

var secretSpecs = []secret.Spec{secret.LLMAPIKey, secret.GitHubToken, secret.NtfyToken}

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
	e := &env{cfg: cfg, log: log, secrets: secrets{
		LLM: secret.NewValue(loaded[0]), GitHub: secret.NewValue(loaded[1]), Ntfy: secret.NewValue(loaded[2]),
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
	return e, 0
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
	if !loaded[1].Present() {
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
