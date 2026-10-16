package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"release-digest/internal/config"
	"release-digest/internal/render"
	"release-digest/internal/scheduler"
	"release-digest/internal/secret"
	"release-digest/internal/server"
)

// listening, when set, is told the address run serves on (for tests that listen on port 0).
var listening func(addr string)

// Shutdown takes at most stopTimeout + drainTimeout; compose.yaml's
// stop_grace_period must leave room for both.
const (
	drainTimeout = 10 * time.Second // for open HTTP connections at shutdown
	stopTimeout  = 30 * time.Second // for the tick in flight at shutdown
)

const stateLastTick = "last_tick"

// guardTimeout bounds the token guard run for a SIGHUP rotation.
const guardTimeout = 30 * time.Second

// tick runs one scheduler pass: the visibility sweep and poll (once the
// token guard has passed), the summarize stage, the alert stage (§3.4),
// the gated promotion of due fallbacks, and the render, which always runs (§3). last_tick is recorded only after a fully
// successful pass, so a restart after a failed one retries at once.
func (e *env) tick(ctx context.Context, srv *server.Server) error {
	start := time.Now()
	id, err := e.poller.NextTick(ctx)
	if err != nil {
		return fmt.Errorf("allocating a tick: %w", err)
	}
	var errs []error
	if err := e.checkGuard(ctx); err == nil {
		if err := e.poller.Sweep(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("visibility sweep: %w", err))
		}
		if err := e.poller.Poll(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("poll: %w", err))
		}
	} else if isGuardError(err) {
		errs = append(errs, err)
	}
	if err := e.summarize(ctx, id, srv); err != nil {
		errs = append(errs, fmt.Errorf("summarize: %w", err))
	}
	if err := e.alerts.Pass(ctx, id); err != nil {
		errs = append(errs, fmt.Errorf("alerts: %w", err))
	}
	promoted, err := e.poller.Promote(ctx, id)
	if err != nil {
		errs = append(errs, fmt.Errorf("promotion: %w", err))
	}
	feeds, err := render.Publish(ctx, e.store, e.cfg, generator(), start)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("render: %w", err))...)
	}
	if srv != nil {
		srv.Set(feeds)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if err := e.store.SetStateTime(ctx, stateLastTick, start); err != nil {
		return fmt.Errorf("recording the tick: %w", err)
	}
	e.log.Info("tick done", "tick", id, "published", promoted, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// summarize runs the summarize stage. It always runs after poll, whatever
// poll's outcome; its gate is the tick's confirmations plus a just-in-time
// check (§3.1). Every few summaries it publishes a render, so a long stage
// shows its progress (§3).
func (e *env) summarize(ctx context.Context, tick int64, srv *server.Server) error {
	e.summ.Gate = func(ctx context.Context, repo string) (bool, error) {
		return e.poller.CheckNow(ctx, repo, tick, "summarize")
	}
	e.summ.Progress = func(ctx context.Context) {
		feeds, err := render.Publish(ctx, e.store, e.cfg, generator(), time.Now())
		if err != nil {
			e.log.Warn("render during the summarize stage failed; the tick's render retries", "err", err)
			return
		}
		if srv != nil {
			srv.Set(feeds)
		}
	}
	return e.summ.Pass(ctx, tick)
}

// cmdRun is the long-running mode: the scheduler plus the HTTP server.
func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("run", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	e, code := open(ctx, *cfgPath, stderr)
	if e == nil {
		return code
	}
	defer e.store.Close()
	c := e.cfg
	if err := e.startGuard(ctx); err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}

	// A changed alert config marks every release for an urgency recompute
	// before the outbox starts, so no alert it made stale is sent (§3.4).
	if err := e.alerts.CheckConfig(ctx); err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	if err := e.outbox.Recover(ctx); err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	outboxDone := make(chan struct{})
	if e.alerts.Enabled {
		go func() { defer close(outboxDone); e.outbox.Run(ctx) }()
	} else {
		close(outboxDone)
	}
	// The store closes only after the worker's last write.
	defer func() { <-outboxDone }()

	srv := server.New(c.Feed.Title, c.PublicBaseURL, e.log)
	srv.Health = e.health
	if err := srv.Load(ctx, e.store); err != nil {
		e.log.Warn("no stored feed to serve yet", "err", err)
	}
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()
	e.log.Info("serving", "addr", ln.Addr().String(), "repos", len(c.Repos), "version", version)

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			e.reloadSecrets(secret.OS)
		}
	}()

	// Ticks don't use ctx: on a signal the tick in flight finishes. tickCtx
	// is only cancelled if that takes longer than stopTimeout.
	tickCtx, cancelTicks := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelTicks()
	sched := &scheduler.Scheduler{
		Interval: c.Schedule.Interval.D,
		Jitter:   c.Schedule.Jitter.D,
		LastTick: func(ctx context.Context) (time.Time, error) { return e.store.StateTime(ctx, stateLastTick) },
		Tick: func(context.Context) {
			if err := e.tick(tickCtx, srv); err != nil {
				e.log.Error("tick failed; still serving the last good feed", "err", err)
			}
		},
	}
	scheduled := make(chan error, 1)
	go func() { scheduled <- sched.Loop(ctx) }()
	if listening != nil {
		listening(ln.Addr().String())
	}

	failed, schedDone := false, false
	select {
	case <-ctx.Done():
		e.log.Info("shutting down")
	case err := <-served:
		e.log.Error("http server stopped", "err", err)
		failed = true
		stop()
	case err := <-scheduled:
		schedDone = true
		if err == nil && ctx.Err() != nil {
			e.log.Info("shutting down")
			break
		}
		// Without the scheduler the feed would silently stop refreshing while
		// /healthz stays green; exit so the container is restarted.
		e.log.Error("scheduler stopped; exiting", "err", err)
		failed = true
		stop()
	}
	if !schedDone {
		timer := time.AfterFunc(stopTimeout, cancelTicks)
		if err := <-scheduled; err != nil {
			e.log.Error("scheduler", "err", err)
			failed = true
		}
		timer.Stop()
	}

	dctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := hs.Shutdown(dctx); err != nil {
		e.log.Warn("http drain", "err", err)
	}
	if failed {
		return 1
	}
	return 0
}

// reloadSecrets re-reads every secret on SIGHUP (§4.3). If any fails, every
// secret keeps its current value: a half-applied rotation is worse than none.
func (e *env) reloadSecrets(env secret.Env) {
	loaded, err := loadSecrets(env)
	if err != nil {
		e.log.Error("SIGHUP: secrets not reloaded; keeping the current ones", "err", err)
		return
	}
	// A replacement GitHub token must pass the token guard before it is
	// activated; if it fails or can't be checked, nothing is reloaded (§3.1).
	newTok := loaded[secGitHub].Value
	if newTok.Reveal() != e.secrets.GitHub.Get().Value.Reveal() {
		ctx, cancel := context.WithTimeout(context.Background(), guardTimeout)
		defer cancel()
		if err := e.gh.Guard(ctx, newTok); err != nil {
			e.log.Error("SIGHUP: the new GitHub token was not accepted; keeping the current secrets", "err", err)
			return
		}
		e.guardOK.Store(true)
	}
	// Alerts were turned on or off at startup; a reload can't change that.
	if err := checkNtfyURL(e.cfg, loaded[secNtfyURL]); err != nil || loaded[secNtfyURL].Present() != e.secrets.NtfyURL.Get().Present() {
		if err == nil {
			err = errors.New("the ntfy url secret was added or removed; restart to turn alerts on or off")
		}
		e.log.Error("SIGHUP: secrets not reloaded; keeping the current ones", "err", err)
		return
	}
	e.secrets.LLM.Set(loaded[secLLM])
	e.secrets.GitHub.Set(loaded[secGitHub])
	e.secrets.Ntfy.Set(loaded[secNtfy])
	e.secrets.NtfyURL.Set(loaded[secNtfyURL])
	logSecrets(e.log, loaded)
	e.log.Info("SIGHUP: secrets reloaded")
}

// health is the /healthz status: degraded while an expired alert awaits
// acknowledgement (§3.4).
func (e *env) health(ctx context.Context) error {
	n, err := e.store.ExpiredUnacked(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("degraded: %d security alert(s) expired undelivered; see the log, then run release-digest notify ack", n)
	}
	return nil
}

// cmdHealthcheck probes /healthz on the local server, for Docker's
// HEALTHCHECK in an image with no shell or curl.
func cmdHealthcheck(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("healthcheck", stderr)
	url := fs.String("url", "", "URL to probe (default: /healthz on the configured listen address)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *url == "" {
		c, err := config.Load(*cfgPath, os.LookupEnv)
		if err != nil {
			return reportErrors(err, stderr)
		}
		if *url, err = healthURL(c.Listen); err != nil {
			fmt.Fprintf(stderr, "release-digest: %v\n", err)
			return 2
		}
	}
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Get(*url)
	if err != nil {
		fmt.Fprintf(stderr, "unhealthy: %v\n", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "unhealthy: %s answered %s\n", *url, resp.Status)
		return 1
	}
	fmt.Fprintln(stdout, "ok")
	return 0
}

// healthURL is the loopback /healthz URL for a listen address such as ":8080".
func healthURL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("listen address %q: %w", listen, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}
