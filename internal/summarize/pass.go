package summarize

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"release-digest/internal/config"
	"release-digest/internal/llm"
	"release-digest/internal/store"
)

// ProgressEvery is how many completed summaries trigger a render mid-pass,
// so a long pass publishes as it goes (§3).
const ProgressEvery = 5

// Summarizer runs the summarize stage of a tick.
type Summarizer struct {
	LLM   *llm.Client
	Store *store.Store
	Cfg   *config.Config
	Log   *slog.Logger

	// Gate is the just-in-time public check before an LLM call or a
	// publication (§3.1, processing gate). It returns an error only for a
	// store failure; any outcome but public is false.
	Gate func(ctx context.Context, repo string) (bool, error)
	// Progress, if set, is called after every ProgressEvery completed
	// summaries. Calls never overlap.
	Progress func(ctx context.Context)
	Now      func() time.Time // replaceable in tests
}

func (s *Summarizer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Enabled reports whether an LLM is configured. Without a model the feed
// shows excerpts only.
func (s *Summarizer) Enabled() bool { return s.Cfg.LLM.Model != "" }

// Result is one summary and the request it answers.
type Result struct {
	Key       string
	Summary   *Summary
	Messages  []llm.Message
	TokensIn  int
	TokensOut int
}

// ErrNotPublic means the repo failed a public check between LLM requests,
// so the notes were not sent again. It is not the release's failure.
var ErrNotPublic = errors.New("the repo is no longer confirmed public")

// Summarize asks the LLM to summarize in, allowing one repair retry for an
// invalid reply. It touches neither the store nor the gate: callers check
// visibility before calling it, and recheck, if set, runs before every
// later HTTP request: the client's retries and format fallbacks, and the
// repair request. A call can take minutes (§3.1). A failed recheck returns
// ErrNotPublic.
func (s *Summarizer) Summarize(ctx context.Context, prompt string, in Input, recheck func(context.Context) (bool, error)) (*Result, error) {
	msgs := Messages(prompt, in)
	res := &Result{Key: Key(s.endpoint(), msgs), Messages: msgs}
	req := llm.Request{Messages: msgs, Name: "release_summary", Schema: Schema}
	if recheck != nil {
		req.Before = func(ctx context.Context) error {
			public, err := recheck(ctx)
			if err == nil && !public {
				err = ErrNotPublic
			}
			return err
		}
	}
	resp, err := s.LLM.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	res.TokensIn, res.TokensOut = resp.TokensIn, resp.TokensOut
	sum, perr := Parse(resp.Content)
	if perr != nil {
		if req.Before != nil {
			if err := req.Before(ctx); err != nil {
				return nil, err
			}
		}
		req.Messages = append(append([]llm.Message(nil), msgs...),
			llm.Message{Role: "assistant", Content: resp.Content},
			llm.Message{Role: "user", Content: fmt.Sprintf("That reply was invalid: %v. Reply again with only the corrected JSON object.", perr)})
		if resp, err = s.LLM.Complete(ctx, req); err != nil {
			return nil, err
		}
		res.TokensIn, res.TokensOut = res.TokensIn+resp.TokensIn, res.TokensOut+resp.TokensOut
		if sum, perr = Parse(resp.Content); perr != nil {
			return nil, fmt.Errorf("invalid reply after a repair retry: %w", perr)
		}
	}
	res.Summary = sum
	return res, nil
}

func (s *Summarizer) endpoint() Endpoint {
	return Endpoint{BaseURL: s.LLM.BaseURL, Model: s.LLM.Model, Temperature: s.LLM.Temperature}
}

// Pass summarizes due releases (§3.3) within summarize_budget: at most
// llm.max_per_pass, oldest first, llm.concurrency at a time, only in repos
// confirmed in tick and passing the just-in-time check. The budget is
// checked before each release starts, so the overrun is at most one call.
// A summary already stored under a release's key is reused without a call.
// LLM failures are recorded on the row with a backoff; only store failures
// are returned.
func (s *Summarizer) Pass(ctx context.Context, tick int64) error {
	if !s.Enabled() {
		return nil
	}
	prompt, err := LoadPrompt(s.Cfg.LLM.PromptFile)
	if err != nil {
		return err
	}
	start := s.now()
	deadline := start.Add(s.Cfg.Schedule.SummarizeBudget.D)
	due, err := s.Store.DueSummaries(ctx, tick, start, s.Cfg.LLM.MaxPerPass)
	if err != nil || len(due) == 0 {
		return err
	}

	var (
		mu       sync.Mutex
		errs     []error
		done     int
		progress sync.Mutex // serializes Progress calls
	)
	jobs := make(chan *store.Release)
	var wg sync.WaitGroup
	for range s.Cfg.LLM.Concurrency {
		wg.Go(func() {
			for r := range jobs {
				ok, err := s.one(ctx, tick, prompt, r)
				mu.Lock()
				if err != nil {
					errs = append(errs, err)
				}
				if ok {
					done++
				}
				report := ok && done%ProgressEvery == 0
				mu.Unlock()
				if report && s.Progress != nil {
					progress.Lock()
					s.Progress(ctx)
					progress.Unlock()
				}
			}
		})
	}
	stopped := ""
feed:
	for _, r := range due {
		if !s.now().Before(deadline) {
			stopped = "summarize_budget used up"
			break
		}
		select {
		case jobs <- r:
		case <-ctx.Done():
			stopped = ctx.Err().Error()
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if stopped != "" {
		s.Log.Warn("summarize stage ended early; it resumes next tick", "reason", stopped, "summarized", done)
	}
	return errors.Join(errs...)
}

// one summarizes r. It reports whether r was summarized; the error is a
// store failure.
func (s *Summarizer) one(ctx context.Context, tick int64, prompt string, r *store.Release) (bool, error) {
	// A failed just-in-time check for another of the repo's releases
	// invalidates the tick's confirmation (§3.1).
	if ok, err := s.Store.Confirmed(ctx, r.Repo, tick); err != nil || !ok {
		return false, err
	}
	text, err := s.Store.Notes(ctx, r.NotesHash)
	if err != nil {
		return false, err
	}
	in := Input{Repo: r.Repo, Tag: r.Tag, PrevTag: r.PrevTag, Source: r.NotesSource, Notes: text}
	key := Key(s.endpoint(), Messages(prompt, in))
	if !r.Resummarize.IsZero() {
		// A resummary has its own key, so the published summary, which the
		// feed reads through the row's summary_key, stays as it is until
		// Summarized switches the row after the final gate. One stored by
		// an earlier attempt whose publication was gated is found again
		// under the same key, so it costs no second call.
		key = ResummaryKey(key, r.Resummarize)
	}
	cached, err := s.Store.Summary(ctx, key)
	if err != nil {
		return false, err
	}
	if public, err := s.Gate(ctx, r.Repo); err != nil || !public {
		return false, err
	}
	reused := cached != nil
	if !reused {
		var gateErr error
		res, err := s.Summarize(ctx, prompt, in, func(ctx context.Context) (bool, error) {
			public, err := s.Gate(ctx, r.Repo)
			gateErr = err
			return public, err
		})
		if err != nil {
			if gateErr != nil {
				return false, gateErr // a store failure
			}
			if ctx.Err() != nil || errors.Is(err, ErrNotPublic) {
				return false, nil // shutting down, or gated: not the release's failure
			}
			s.Log.Warn("summary failed; the release keeps its excerpt and is retried later",
				"repo", r.Repo, "tag", r.Tag, "attempts", r.Attempts+1, "err", err)
			return false, s.Store.SummaryFailed(ctx, r.Repo, r.Tag, err.Error(), s.now())
		}
		cached = &store.Summary{Key: key, Model: s.LLM.Model, JSON: res.Summary.Encode(), Created: s.now(),
			TokensIn: res.TokensIn, TokensOut: res.TokensOut}
		if err := s.Store.PutSummary(ctx, cached); err != nil {
			return false, err
		}
		// The call can take minutes: check again before publishing, in case
		// the repo went private meanwhile. The summary stays stored, so the
		// next eligible pass publishes it without another call.
		if public, err := s.Gate(ctx, r.Repo); err != nil || !public {
			return false, err
		}
	}
	// Conditional on the repo's confirmation for this tick, atomically: a
	// concurrent worker's failed check for the same repo stops it (§3.1).
	ok, err := s.Store.Summarized(ctx, r.Repo, r.Tag, r.NotesHash, key, cached.Created, tick, s.now())
	if err == nil && ok {
		s.Log.Info("summarized", "repo", r.Repo, "tag", r.Tag, "reused", reused, "resummarized", !r.Resummarize.IsZero(), "tokens_in", cached.TokensIn, "tokens_out", cached.TokensOut)
	}
	return ok, err
}
