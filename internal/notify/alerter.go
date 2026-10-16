package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"release-digest/internal/advisory"
	"release-digest/internal/config"
	"release-digest/internal/github"
	"release-digest/internal/store"
	"release-digest/internal/summarize"
)

// Tuning that is fixed rather than configured (docs/security-alerts.md).
const (
	ScansPerTick    = 200 // releases scanned for advisory IDs per tick
	LookupsPerTick  = 20  // advisory lookups per tick
	RefreshRecent   = 24 * time.Hour
	RefreshVisible  = 7 * 24 * time.Hour
	NotFoundRetry   = time.Hour
	ErrorBackoff    = 5 * time.Minute
	ErrorBackoffMax = time.Hour
)

// stateFingerprint is the scheduler_state key of the urgency config fingerprint.
const stateFingerprint = "urgency_fingerprint"

// Alerter runs the alert stage of a tick: it finds advisory IDs in notes,
// looks them up, and recomputes the urgency of releases whose inputs
// changed, enqueuing or cancelling their alerts (docs/security-alerts.md).
type Alerter struct {
	Store *store.Store
	GH    *github.Client
	Cfg   *config.Config
	Log   *slog.Logger
	Now   func() time.Time // replaceable in tests

	// Enabled is set when an ntfy URL is configured. Without it urgency is
	// still decided, for the feed's marking, but nothing is enqueued.
	Enabled bool
	// Kick, if set, wakes the outbox after an alert is enqueued.
	Kick func()
}

func (a *Alerter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Pass is the alert stage. It runs only for repos confirmed in tick, like
// every stage that changes what the feed shows (docs/polling.md). Advisory lookups
// name only an advisory ID, never a repo. Only store failures are returned.
func (a *Alerter) Pass(ctx context.Context, tick int64) error {
	due, err := a.Store.AdvisoryScansDue(ctx, tick, ScansPerTick)
	if err != nil {
		return err
	}
	for _, r := range due {
		notes, err := a.Store.Notes(ctx, r.NotesHash)
		if err != nil {
			return err
		}
		if err := a.Store.SetAdvisoryLinks(ctx, r.Repo, r.Tag, r.NotesHash, advisory.Extract(notes)); err != nil {
			return err
		}
	}
	if err := a.lookups(ctx); err != nil {
		return err
	}
	dirty, err := a.Store.UrgencyDirty(ctx, tick)
	if err != nil {
		return err
	}
	for _, r := range dirty {
		if err := a.Recompute(ctx, r.Repo, r.Tag); err != nil {
			return err
		}
	}
	return nil
}

// lookups refreshes up to LookupsPerTick due advisories (docs/security-alerts.md).
// A rate limit or shutdown ends the batch; it resumes next tick.
func (a *Alerter) lookups(ctx context.Context) error {
	now := a.now()
	ids, err := a.Store.AdvisoriesDue(ctx, now, LookupsPerTick)
	if err != nil {
		return err
	}
	for _, id := range ids {
		prev, err := a.Store.Advisory(ctx, id)
		if err != nil {
			return err
		}
		if prev == nil {
			prev = &store.Advisory{ID: id}
		}
		data, lerr := a.GH.Advisories(ctx, id)
		now := a.now()
		next := *prev
		switch {
		case lerr == nil:
			found, err := parsed(id, data, now)
			if err != nil {
				lerr = err
				break
			}
			next = found
			recent, err := a.Store.AdvisoryRecent(ctx, id, now.Add(-a.Cfg.Notify.MaxAge.D))
			if err != nil {
				return err
			}
			next.NextFetchAt = now.Add(RefreshVisible)
			if recent {
				next.NextFetchAt = now.Add(RefreshRecent)
			}
		case errors.Is(lerr, github.ErrNotFound):
			// Not (yet) in the global database: a brand-new GHSA often isn't.
			next = store.Advisory{ID: id, FetchedAt: now, NextFetchAt: now.Add(NotFoundRetry)}
			if prev.Found {
				a.Log.Warn("advisory no longer found", "advisory", id)
			}
			lerr = nil
		case ctx.Err() != nil || github.IsRateLimit(lerr):
			a.Log.Warn("advisory lookups stopped; they resume next tick", "err", lerr)
			return nil
		}
		if lerr != nil {
			next.Errors = prev.Errors + 1
			next.NextFetchAt = now.Add(min(ErrorBackoff<<min(prev.Errors, 8), ErrorBackoffMax))
			a.Log.Warn("advisory lookup failed; retrying later", "advisory", id, "retry_at", next.NextFetchAt, "err", lerr)
		}
		if err := a.Store.PutAdvisory(ctx, &next); err != nil {
			return err
		}
	}
	return nil
}

// parsed is the cache row for a found advisory lookup.
func parsed(id string, data json.RawMessage, now time.Time) (store.Advisory, error) {
	advs, err := advisory.Parse(string(data))
	if err != nil {
		return store.Advisory{}, err
	}
	out := store.Advisory{ID: id, Found: true, JSON: string(data), FetchedAt: now}
	withdrawn := len(advs) > 0
	for _, adv := range advs {
		if advisory.Rank(adv.Severity) > advisory.Rank(out.Severity) || out.Severity == "" {
			out.Severity = adv.Severity
		}
		withdrawn = withdrawn && adv.Withdrawn()
	}
	if withdrawn {
		out.WithdrawnAt = now
		if t, err := time.Parse(time.RFC3339, *advs[0].WithdrawnAt); err == nil {
			out.WithdrawnAt = t
		}
	}
	return out, nil
}

// Recompute decides a release's urgency from its current inputs and, in
// the same transaction, records it and enqueues, rebuilds or cancels its
// alert (docs/security-alerts.md).
func (a *Alerter) Recompute(ctx context.Context, repo, tag string) error {
	var enqueued bool
	var d Decision
	var old string
	err := a.Store.Tx(ctx, func(tx *store.Tx) error {
		enqueued = false
		r, err := tx.Release(ctx, repo, tag)
		if err != nil || r == nil {
			return err
		}
		old = r.Urgency
		in, err := a.inputs(ctx, tx, r)
		if err != nil {
			return err
		}
		d = Decide(in)
		for _, l := range d.Links {
			if err := tx.SetLink(ctx, repo, tag, l.ID, l.Matched, l.Fixes); err != nil {
				return err
			}
		}
		now := a.now()
		if _, err := tx.SetUrgency(ctx, repo, tag, d.Urgency, d.Source, now); err != nil {
			return err
		}
		enqueued, err = a.outbox(ctx, tx, r, in.Summary, d, now)
		return err
	})
	if err != nil {
		return err
	}
	for _, l := range d.Links {
		if !l.Fires && l.Note != "" {
			a.Log.Debug("advisory doesn't fire", "repo", repo, "tag", tag, "advisory", l.ID, "why", l.Note)
		}
	}
	if d.Downgraded {
		a.Log.Info("LLM urgent downgraded to advisory: no security change listed or no security keyword in the notes",
			"repo", repo, "tag", tag)
	}
	if old != d.Urgency {
		a.Log.Info("urgency changed", "repo", repo, "tag", tag, "from", old, "to", d.Urgency, "source", d.Source)
	}
	if enqueued && a.Kick != nil {
		a.Kick()
	}
	return nil
}

// inputs gathers the decision inputs for r.
func (a *Alerter) inputs(ctx context.Context, tx *store.Tx, r *store.Release) (Inputs, error) {
	opts := RepoOptions(a.Cfg, r.Repo)
	in := Inputs{Release: r, Packages: opts.Packages, MinSeverity: a.Cfg.Notify.MinSeverity}
	if opts.PrereleasePattern != nil {
		in.Prerelease = opts.PrereleasePattern.Re
	}
	var err error
	if in.Notes, err = tx.Notes(ctx, r.NotesHash); err != nil {
		return in, err
	}
	if r.State == store.StateSummarized {
		sum, err := tx.Summary(ctx, r.SummaryKey)
		if err != nil {
			return in, err
		}
		if sum != nil {
			if s, err := summarize.Decode(sum.JSON); err == nil {
				in.Summary = s
			}
		}
	}
	in.Links, err = tx.Links(ctx, r.Repo, r.Tag)
	return in, err
}

// outbox applies a decision to the release's alert. An urgent release
// that may alert is enqueued once, or has its unsent alert rebuilt; any
// other release has its unsent alert cancelled. It reports whether an
// alert is now queued.
func (a *Alerter) outbox(ctx context.Context, tx *store.Tx, r *store.Release, sum *summarize.Summary, d Decision, now time.Time) (bool, error) {
	n, err := tx.Notification(ctx, r.GUID)
	if err != nil {
		return false, err
	}
	eligible, why, err := a.eligible(ctx, tx, r, now)
	if err != nil {
		return false, err
	}
	if d.Urgency != store.UrgencyUrgent || !eligible {
		if n != nil && (n.State == store.AlertQueued || n.State == store.AlertSending) {
			// Past the window, a queued alert expires loudly rather than
			// being cancelled.
			if d.Urgency == store.UrgencyUrgent && why == whyTooOld {
				return false, nil
			}
			a.Log.Info("alert cancelled", "repo", r.Repo, "tag", r.Tag, "urgency", d.Urgency, "why", why)
			return false, tx.Cancel(ctx, r.GUID)
		}
		if d.Urgency == store.UrgencyUrgent && n == nil {
			a.Log.Info("urgent release not alerted", "repo", r.Repo, "tag", r.Tag, "why", why)
		}
		return false, nil
	}
	p := BuildPayload(r, sum, d).Encode()
	expires := r.Published.Add(a.Cfg.Notify.MaxAge.D)
	if n == nil {
		a.Log.Info("alert enqueued", "repo", r.Repo, "tag", r.Tag, "reason", d.Source)
		return tx.Enqueue(ctx, &store.Notification{GUID: r.GUID, Repo: r.Repo, Tag: r.Tag, Channel: "ntfy",
			Reason: d.Source, Payload: p, Created: now, Expires: expires})
	}
	if n.Payload == p && n.Reason == d.Source && n.Expires.Equal(expires) && n.State != store.AlertCancelled && !n.CancelRequested {
		return false, nil
	}
	ok, err := tx.Rebuild(ctx, r.GUID, d.Source, p, expires, now)
	if ok {
		a.Log.Info("alert rebuilt", "repo", r.Repo, "tag", r.Tag, "reason", d.Source, "was", n.State)
	}
	return ok, err
}

const whyTooOld = "published longer ago than notify.max_age"

// eligible reports whether r may alert (docs/security-alerts.md): alerts are on, it was
// published after tracking began (unless include_backfill) and within
// max_age, and it is no pre-release (unless include_prereleases).
func (a *Alerter) eligible(ctx context.Context, tx *store.Tx, r *store.Release, now time.Time) (bool, string, error) {
	n := a.Cfg.Notify
	switch {
	case !a.Enabled:
		return false, "no ntfy URL is configured", nil
	case r.State == store.StateSkipped:
		return false, "skipped", nil
	case !r.MissingSince.IsZero():
		return false, "deleted on GitHub", nil
	case r.Published.IsZero():
		return false, "its date is unknown", nil
	case r.Prerelease && !n.IncludePrereleases:
		return false, "pre-release (notify.include_prereleases is off)", nil
	case now.Sub(r.Published) >= n.MaxAge.D:
		return false, whyTooOld, nil
	}
	if !n.IncludeBackfill {
		rs, err := tx.RepoState(ctx, r.Repo)
		if err != nil {
			return false, "", err
		}
		if !r.Published.After(rs.InitializedAt) {
			return false, "published before tracking began (notify.include_backfill is off)", nil
		}
	}
	return true, "", nil
}

// RepoOptions are the effective options of a repo, or the defaults for a
// repo no longer configured.
func RepoOptions(c *config.Config, repo string) config.RepoOptions {
	for _, r := range c.Repos {
		if r.Name() == repo {
			return r.RepoOptions
		}
	}
	return c.Defaults
}

// Fingerprint identifies the config that urgency decisions read (docs/security-alerts.md).
func Fingerprint(c *config.Config, enabled bool) string {
	type repo struct {
		Packages   []config.Package `json:"packages"`
		Prerelease string           `json:"prerelease_pattern"`
	}
	doc := struct {
		MinSeverity        string          `json:"min_severity"`
		MaxAge             time.Duration   `json:"max_age"`
		IncludeBackfill    bool            `json:"include_backfill"`
		IncludePrereleases bool            `json:"include_prereleases"`
		Enabled            bool            `json:"enabled"`
		Repos              map[string]repo `json:"repos"`
		Defaults           repo            `json:"defaults"`
	}{c.Notify.MinSeverity, c.Notify.MaxAge.D, c.Notify.IncludeBackfill, c.Notify.IncludePrereleases, enabled,
		map[string]repo{}, repo{c.Defaults.Packages, pattern(c.Defaults.PrereleasePattern)}}
	for _, r := range c.Repos {
		doc.Repos[r.Name()] = repo{r.Packages, pattern(r.PrereleasePattern)}
	}
	b, _ := json.Marshal(doc) // map keys marshal sorted
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func pattern(r *config.Regex) string {
	if r == nil {
		return ""
	}
	return r.Raw
}

// CheckConfig compares the urgency config with the one stored and, if it
// changed, marks every release for an urgency recompute (docs/security-alerts.md). It runs at
// startup, before the outbox worker: an alert waits while its release is
// marked, so one made stale by the change is cancelled before it can be sent.
func (a *Alerter) CheckConfig(ctx context.Context) error {
	fp := Fingerprint(a.Cfg, a.Enabled)
	return a.Store.Tx(ctx, func(tx *store.Tx) error {
		old, err := tx.State(ctx, stateFingerprint)
		if err != nil || old == fp {
			return err
		}
		n, err := tx.MarkAllUrgencyDirty(ctx)
		if err != nil {
			return err
		}
		if old != "" {
			a.Log.Info("alert settings changed; urgency is recomputed for every release", "releases", n)
		}
		return tx.SetState(ctx, stateFingerprint, fp)
	})
}
