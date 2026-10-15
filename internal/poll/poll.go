// Package poll is the GitHub stage of a tick: the visibility sweep, the
// per-repo scans, reconciliation, the tag metadata queue, deletion checks,
// and the gated pending → fallback promotion at render time (PLAN.md §3, §3.1).
package poll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"release-digest/internal/config"
	"release-digest/internal/github"
	"release-digest/internal/ident"
	"release-digest/internal/store"
	"release-digest/internal/version"
)

// Tuning that the plan fixes rather than configures (§3, §3.1).
const (
	SweepConcurrency = 8
	SweepTimeout     = 10 * time.Second
	MetadataPerTick  = 30
	ReconcileEvery   = 24 * time.Hour
	ReconcileExtra   = 5 // reconcile_pages = max_pages + 5
	MinPublishWindow = 7 * 24 * time.Hour
)

// scheduler_state keys.
const (
	stateTick        = "tick_id"
	stateSweepCursor = "sweep_cursor"
	statePollCursor  = "poll_cursor"
)

// Poller runs the GitHub stages against one store.
type Poller struct {
	GH    *github.Client
	Store *store.Store
	Cfg   *config.Config
	Log   *slog.Logger
	Now   func() time.Time // replaceable in tests
}

func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// NextTick allocates the next tick ID. Confirmations are only valid for the
// tick that made them (§3.1, processing gate).
func (p *Poller) NextTick(ctx context.Context) (int64, error) {
	var id int64
	err := p.Store.Tx(ctx, func(tx *store.Tx) error {
		v, err := tx.State(ctx, stateTick)
		if err != nil {
			return err
		}
		if v != "" {
			if id, err = strconv.ParseInt(v, 10, 64); err != nil {
				return fmt.Errorf("scheduler_state %s: %w", stateTick, err)
			}
		}
		id++
		return tx.SetState(ctx, stateTick, strconv.FormatInt(id, 10))
	})
	return id, err
}

// rotate returns the repos starting after the one named cursor.
func rotate(repos []*config.Repo, cursor string, after bool) []*config.Repo {
	for i, r := range repos {
		if r.Name() == cursor {
			if after {
				i++
			}
			return append(slices.Clone(repos[i%len(repos):]), repos[:i%len(repos)]...)
		}
	}
	return repos
}

// Sweep checks every repo's visibility at the start of a tick, 8 at a time,
// within sweep_budget (§3). Repos it doesn't confirm are unverified for this
// tick. The cursor moves past the last repo attempted, so repos that time
// out rotate to the back.
func (p *Poller) Sweep(ctx context.Context, tick int64) error {
	repos := p.Cfg.Repos
	if len(repos) == 0 {
		return nil
	}
	cursor, err := p.Store.State(ctx, stateSweepCursor)
	if err != nil {
		return err
	}
	order := rotate(repos, cursor, true)
	bctx, cancel := context.WithTimeout(ctx, p.Cfg.Schedule.SweepBudget.D)
	defer cancel()

	type result struct {
		repo string
		vis  github.Visibility
		err  error
	}
	results := make(chan result)
	sem := make(chan struct{}, SweepConcurrency)
	var wg sync.WaitGroup
	last := ""
	go func() {
		defer close(results)
		for _, r := range order {
			select {
			case sem <- struct{}{}:
			case <-bctx.Done():
			}
			if bctx.Err() != nil {
				break
			}
			last = r.Name()
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				defer func() { <-sem }()
				vis, err := p.GH.CheckPublic(bctx, name, SweepTimeout)
				results <- result{name, vis, err}
			}(r.Name())
		}
		wg.Wait()
	}()
	var errs []error
	confirmed := 0
	for res := range results {
		if err := p.recordVisibility(ctx, res.repo, tick, res.vis, res.err, "sweep"); err != nil {
			errs = append(errs, err)
		}
		if res.vis.Public {
			confirmed++
		}
	}
	if last != "" {
		if err := p.Store.SetState(ctx, stateSweepCursor, last); err != nil {
			errs = append(errs, err)
		}
	}
	if confirmed < len(repos) {
		p.Log.Warn("visibility sweep: some repos unverified this tick", "confirmed", confirmed, "repos", len(repos))
	}
	return errors.Join(errs...)
}

// recordVisibility stores a public check's outcome and logs anything but a
// confirmation.
func (p *Poller) recordVisibility(ctx context.Context, repo string, tick int64, vis github.Visibility, err error, during string) error {
	msg := ""
	switch {
	case vis.Gone:
		msg = "repo is private or not found; held: nothing is fetched or published for it"
		p.Log.Error(msg, "repo", repo, "during", during)
	case err != nil:
		msg = "visibility check failed: " + err.Error()
		p.Log.Warn("visibility check failed; repo unverified this tick", "repo", repo, "during", during, "err", err)
	}
	return p.Store.Visibility(ctx, repo, tick, vis.Public, vis.Gone, p.now(), msg)
}

// checkNow is the just-in-time public check before an exposing action
// (§3.1). Any outcome but public invalidates the tick's confirmation.
func (p *Poller) checkNow(ctx context.Context, repo string, tick int64, during string) (bool, error) {
	vis, err := p.GH.CheckPublic(ctx, repo, 0)
	if serr := p.recordVisibility(ctx, repo, tick, vis, err, during); serr != nil {
		return false, serr
	}
	return vis.Public, nil
}

// Poll polls repos round-robin from the persisted cursor within poll_budget,
// then drains the tag metadata queue and runs the rotating deletion checks
// (§3.1). The budget is checked between repos, pages of a reconciliation,
// and queue items; a repo's scan, once started, always finishes. Only store
// failures are returned; GitHub failures are logged and recorded per repo.
func (p *Poller) Poll(ctx context.Context, tick int64) error {
	deadline := p.now().Add(p.Cfg.Schedule.PollBudget.D)
	b := &budget{p: p, deadline: deadline}
	if len(p.Cfg.Repos) > 0 {
		if err := p.pollRepos(ctx, tick, b); err != nil {
			return err
		}
	}
	if b.stopped {
		return nil
	}
	if err := p.drainMetadata(ctx, tick, b); err != nil || b.stopped {
		return err
	}
	return p.verifySweep(ctx, tick, b)
}

type budget struct {
	p        *Poller
	deadline time.Time
	stopped  bool
}

func (b *budget) out() bool {
	if !b.stopped && !b.p.now().Before(b.deadline) {
		b.stop("poll_budget used up")
	}
	return b.stopped
}

func (b *budget) stop(reason string) {
	if !b.stopped {
		b.stopped = true
		b.p.Log.Warn("poll stage ended early; it resumes from here next tick", "reason", reason)
	}
}

func (p *Poller) pollRepos(ctx context.Context, tick int64, b *budget) error {
	cursor, err := p.Store.State(ctx, statePollCursor)
	if err != nil {
		return err
	}
	order := rotate(p.Cfg.Repos, cursor, false)
	for i, r := range order {
		if b.out() {
			return p.Store.SetState(ctx, statePollCursor, r.Name())
		}
		err := p.pollRepo(ctx, r, tick, b)
		if github.IsRateLimit(err) {
			b.stop(err.Error())
			return p.Store.SetState(ctx, statePollCursor, r.Name())
		}
		var se storeError
		if errors.As(err, &se) {
			return se.err
		}
		if err != nil {
			p.Log.Warn("poll failed", "repo", r.Name(), "err", err)
			if serr := p.Store.SetRepoError(ctx, r.Name(), err.Error()); serr != nil {
				return serr
			}
		}
		next := order[(i+1)%len(order)].Name()
		if err := p.Store.SetState(ctx, statePollCursor, next); err != nil {
			return err
		}
	}
	return nil
}

// storeError marks a store failure, which ends the stage, as opposed to a
// GitHub failure, which only fails the repo.
type storeError struct{ err error }

func (e storeError) Error() string { return e.err.Error() }
func (e storeError) Unwrap() error { return e.err }

func storeErr(err error) error {
	if err == nil {
		return nil
	}
	return storeError{err}
}

// entry is one list entry, from either the releases list or the tag refs.
type entry struct {
	Tag, Name, Body, HTMLURL string
	Kind                     string // release | tag
	Prerelease               bool
	Published                time.Time // zero for tags
	RefSHA, RefType          string    // tags only
}

// mode is which list a repo is read from this poll.
type mode string

const (
	modeReleases mode = "release"
	modeTags     mode = "tag"
)

// releasesPage reads one page of the releases list, newest first.
func (p *Poller) releasesPage(ctx context.Context, r *config.Repo, page int) ([]entry, error) {
	rels, err := p.GH.Releases(ctx, r.Name(), page)
	if err != nil {
		return nil, err
	}
	var out []entry
	for _, rel := range rels {
		if rel.Draft || rel.TagName == "" || rel.PublishedAt == nil {
			continue // drafts are always excluded and never recorded: they may be published later
		}
		e := entry{Tag: rel.TagName, Name: rel.Name, Body: rel.Body, Kind: "release", HTMLURL: rel.HTMLURL,
			Prerelease: rel.Prerelease, Published: rel.PublishedAt.UTC()}
		if e.HTMLURL == "" {
			e.HTMLURL = p.tagURL(r.Name(), rel.TagName)
		}
		out = append(out, e)
	}
	return out, nil
}

// tagEntries reads the repo's complete tag list.
func (p *Poller) tagEntries(ctx context.Context, r *config.Repo) ([]entry, error) {
	refs, err := p.GH.TagRefs(ctx, r.Name())
	if err != nil {
		return nil, err
	}
	out := make([]entry, len(refs))
	for i, t := range refs {
		out[i] = entry{Tag: t.Name, Kind: "tag", HTMLURL: p.tagURL(r.Name(), t.Name), RefSHA: t.SHA, RefType: t.Type}
	}
	return out, nil
}

// webBase is the web host for the API: github.com for api.github.com, and
// the host itself for GitHub Enterprise Server (https://host/api/v3).
func webBase(api string) string {
	u, err := url.Parse(api)
	if err != nil {
		return "https://github.com"
	}
	if u.Host == "api.github.com" {
		return "https://github.com"
	}
	return u.Scheme + "://" + u.Host
}

func (p *Poller) tagURL(repo, tag string) string {
	parts := strings.Split(tag, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return webBase(p.Cfg.GitHub.APIURL) + "/" + repo + "/releases/tag/" + strings.Join(parts, "/")
}

// pollRepo is one repo's unit of poll work: the scan, the window deletion
// checks and, when due, reconciliation. It runs only for a repo this tick's
// sweep confirmed public (§3.1 step 0, processing gate); fetching content
// exposes nothing, so it needs no just-in-time check of its own.
func (p *Poller) pollRepo(ctx context.Context, r *config.Repo, tick int64, b *budget) error {
	ok, err := p.Store.Confirmed(ctx, r.Name(), tick)
	if err != nil || !ok {
		return storeErr(err)
	}
	var page1 []entry
	if slices.Contains(r.Sources, "releases") {
		if page1, err = p.releasesPage(ctx, r, 1); err != nil {
			return err
		}
	}
	if len(page1) == 0 && slices.Contains(r.Sources, "tags") {
		// No releases (or tags only): the tag list is complete in one
		// request, so there is no boundary, catch-up or reconciliation.
		tags, err := p.scanTags(ctx, r)
		if err != nil {
			return err
		}
		return p.windowChecks(ctx, r, modeTags, tags, true)
	}
	if err := p.scanReleases(ctx, r, page1); err != nil {
		return err
	}
	if err := p.windowChecks(ctx, r, modeReleases, page1, false); err != nil {
		return err
	}
	return p.reconcile(ctx, r, b)
}

// fresh reports whether the repo's list m is being read for the first time:
// the repo's first scan, or the first since it switched between the
// releases and tags lists. Either way only a backfill is queued, so a
// switch can't flood the feed with old versions.
func fresh(rs *store.RepoState, m mode) bool {
	return rs.InitializedAt.IsZero() || (rs.ScanMode != "" && rs.ScanMode != string(m))
}

// finishScan records a completed scan in the repo's state.
func (p *Poller) finishScan(rs *store.RepoState, m mode, start time.Time) {
	if rs.InitializedAt.IsZero() {
		rs.InitializedAt, rs.LastReconciledAt = start, start
	}
	rs.ScanMode = string(m)
	rs.LastSuccessAt, rs.LastError = p.now(), ""
}

// scanReleases takes page 1 of the releases list and, when the scan
// boundary isn't on it, reads catch-up pages up to max_pages, then commits
// everything in one transaction (§3.1 step 3). On any error nothing is
// written and the boundary stays put.
func (p *Poller) scanReleases(ctx context.Context, r *config.Repo, page1 []entry) error {
	name := r.Name()
	start := p.now()
	rs, err := p.Store.RepoState(ctx, name)
	if err != nil {
		return storeErr(err)
	}
	pages := [][]entry{page1}
	first := fresh(rs, modeReleases)
	if !first && rs.ScanBoundary != "" && !hasTag(page1, rs.ScanBoundary) {
		last := page1
		for pg := 2; pg <= *r.MaxPages && len(last) >= github.PerPage; pg++ {
			if last, err = p.releasesPage(ctx, r, pg); err != nil {
				return err
			}
			pages = append(pages, last)
			if hasTag(last, rs.ScanBoundary) {
				break
			}
			if pg == *r.MaxPages && len(last) >= github.PerPage {
				p.Log.Warn("catch-up reached max_pages without finding the last scan's newest version; older new versions are omitted",
					"repo", name, "pages", pg, "boundary", rs.ScanBoundary)
			}
		}
	}

	inserted := 0
	err = p.Store.Tx(ctx, func(tx *store.Tx) error {
		rs, err := tx.RepoState(ctx, name)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		backfilled := 0
		var newRows []*store.Release
		for pi, pg := range pages {
			for _, e := range pg {
				if seen[e.Tag] {
					continue
				}
				seen[e.Tag] = true
				row := p.newRow(r, e, start)
				filtered := p.filtered(r, row)
				switch {
				case first && e.Published.After(start):
					row.Origin = store.OriginLive // released while this first scan ran
				case first:
					row.Origin = store.OriginBackfill
					if !filtered && backfilled < *r.Backfill {
						backfilled++
					} else {
						filtered = true
					}
				case pi == 0:
					row.Origin = store.OriginLive
				default:
					row.Origin = store.OriginCatchup
				}
				row.State = store.StatePending
				if filtered {
					row.State = store.StateSkipped
				}
				ok, err := p.insert(ctx, tx, row, e)
				if err != nil {
					return err
				}
				if ok {
					newRows = append(newRows, row)
				}
			}
		}
		inserted = len(newRows)
		if err := setPredecessors(ctx, tx, newRows); err != nil {
			return err
		}
		if err := tx.Seen(ctx, name, slices.Collect(maps.Keys(seen))); err != nil {
			return err
		}
		if len(page1) > 0 {
			rs.ScanBoundary = page1[0].Tag
		}
		p.finishScan(rs, modeReleases, start)
		return tx.PutRepoState(ctx, rs)
	})
	if err != nil {
		return storeErr(err)
	}
	if inserted > 0 || first {
		p.Log.Info("polled", "repo", name, "list", "releases", "pages", len(pages), "new", inserted, "first_sight", first)
	}
	return nil
}

// scanTags reads the repo's complete tag list and records every tag not yet
// stored (§3.1 step 2). On first sight the highest `backfill` versions are
// pending and the rest skipped; after that every new tag is live. Tag rows
// get their date later, from the metadata queue.
func (p *Poller) scanTags(ctx context.Context, r *config.Repo) ([]entry, error) {
	name := r.Name()
	start := p.now()
	tags, err := p.tagEntries(ctx, r)
	if err != nil {
		return nil, err
	}
	inserted, first := 0, false
	err = p.Store.Tx(ctx, func(tx *store.Tx) error {
		rs, err := tx.RepoState(ctx, name)
		if err != nil {
			return err
		}
		first = fresh(rs, modeTags)
		stored, err := tx.RepoTags(ctx, name)
		if err != nil {
			return err
		}
		var rows []*store.Release
		for _, e := range tags {
			if stored[e.Tag] {
				continue
			}
			stored[e.Tag] = true
			row := p.newRow(r, e, start)
			row.Origin, row.State = store.OriginLive, store.StatePending
			if first {
				row.Origin = store.OriginBackfill
			}
			if p.filtered(r, row) {
				row.State = store.StateSkipped
			}
			rows = append(rows, row)
		}
		if first {
			backfillHighest(rows, *r.Backfill)
		}
		var newRows []*store.Release
		for _, row := range rows {
			ok, err := tx.InsertRelease(ctx, row)
			if err != nil {
				return err
			}
			if ok {
				newRows = append(newRows, row)
			}
		}
		inserted = len(newRows)
		if err := setPredecessors(ctx, tx, newRows); err != nil {
			return err
		}
		names := make([]string, len(tags))
		for i, e := range tags {
			names[i] = e.Tag
		}
		if err := tx.Seen(ctx, name, names); err != nil {
			return err
		}
		p.finishScan(rs, modeTags, start)
		return tx.PutRepoState(ctx, rs)
	})
	if err != nil {
		return nil, storeErr(err)
	}
	if inserted > 0 || first {
		p.Log.Info("polled", "repo", name, "list", "tags", "tags", len(tags), "new", inserted, "first_sight", first)
	}
	return tags, nil
}

// backfillHighest keeps the n highest pending versions pending and skips
// the rest. The tag list has no dates, so "newest" means highest version;
// date-ordered tags can't be ranked and are skipped.
func backfillHighest(rows []*store.Release, n int) {
	var pending []*store.Release
	for _, r := range rows {
		switch {
		case r.State != store.StatePending:
		case r.Version == "":
			r.State = store.StateSkipped
		default:
			pending = append(pending, r)
		}
	}
	slices.SortStableFunc(pending, func(a, b *store.Release) int {
		return version.Compare(version.Parse(b.Version, nil), version.Parse(a.Version, nil))
	})
	for i, r := range pending {
		if i >= n {
			r.State = store.StateSkipped
		}
	}
}

func hasTag(es []entry, tag string) bool {
	return slices.ContainsFunc(es, func(e entry) bool { return e.Tag == tag })
}

// newRow classifies an entry into a release row (state and origin unset).
func (p *Poller) newRow(r *config.Repo, e entry, now time.Time) *store.Release {
	v := version.Parse(e.Tag, patternOf(r.PrereleasePattern))
	return &store.Release{
		Repo: r.Name(), Tag: e.Tag, GUID: ident.GUID(r.Name(), e.Tag), Kind: e.Kind, Name: e.Name,
		HTMLURL: e.HTMLURL, Stream: v.Stream, Version: v.String(), Prerelease: e.Prerelease || v.Prerelease,
		Published: e.Published, FirstSeen: now, RefSHA: e.RefSHA, RefType: e.RefType,
	}
}

func patternOf(r *config.Regex) *regexp.Regexp {
	if r == nil {
		return nil
	}
	return r.Re
}

// filtered reports whether a row is recorded as skipped (§3.1 step 4):
// unrecognized tags (unless tag_pattern matches them), tag_pattern misses,
// exclude_pattern matches and unwanted pre-releases.
func (p *Poller) filtered(r *config.Repo, row *store.Release) bool {
	tp, xp := patternOf(r.TagPattern), patternOf(r.ExcludePattern)
	switch {
	case row.Version == "" && (tp == nil || !tp.MatchString(row.Tag)):
		return true
	case tp != nil && !tp.MatchString(row.Tag):
		return true
	case xp != nil && xp.MatchString(row.Tag):
		return true
	case row.Prerelease && !*r.IncludePrereleases:
		return true
	}
	return false
}

// insert stores a new row with its notes. For a row already stored and not
// yet visible, it refreshes the notes from an edited release body.
func (p *Poller) insert(ctx context.Context, tx *store.Tx, row *store.Release, e entry) (bool, error) {
	body := strings.TrimSpace(e.Body)
	if body != "" {
		h, err := tx.PutNotes(ctx, body)
		if err != nil {
			return false, err
		}
		row.NotesSource, row.NotesHash = "release_body", h
	}
	ok, err := tx.InsertRelease(ctx, row)
	if err != nil || ok || body == "" {
		return ok, err
	}
	return false, tx.SetPendingNotes(ctx, row.Repo, row.Tag, row.NotesSource, row.NotesHash)
}

// setPredecessors records prev_tag for newly inserted, non-skipped rows from
// every stored row in their stream (§3.2.1).
func setPredecessors(ctx context.Context, tx *store.Tx, rows []*store.Release) error {
	streams := map[[2]string][]version.Candidate{}
	for _, r := range rows {
		if r.State == store.StateSkipped {
			continue
		}
		key := [2]string{r.Repo, r.Stream}
		cands, ok := streams[key]
		if !ok {
			stored, err := tx.StreamReleases(ctx, r.Repo, r.Stream)
			if err != nil {
				return err
			}
			for _, s := range stored {
				cands = append(cands, candidate(s))
			}
			streams[key] = cands
		}
		if prev := version.Predecessor(candidate(r), cands); prev != "" {
			if err := tx.SetPrevTag(ctx, r.Repo, r.Tag, prev); err != nil {
				return err
			}
		}
	}
	return nil
}

func candidate(r *store.Release) version.Candidate {
	return version.Candidate{Tag: r.Tag, Stream: r.Stream, Version: r.Version, Prerelease: r.Prerelease, Published: r.Published}
}

// windowChecks runs existence checks on stored visible rows that should have
// been in the list but weren't (§3.1, deletion detection). With a complete
// list (tags), that is every visible tag row missing from it. The releases
// list is newest first, so a stored release newer than page 1's oldest
// entry belongs on it; an empty page 1 makes every visible row a candidate.
// At most a page's worth are checked per poll, and rows already marked
// missing are skipped: a list appearance clears them.
func (p *Poller) windowChecks(ctx context.Context, r *config.Repo, m mode, list []entry, complete bool) error {
	visible, err := p.Store.RepoVisible(ctx, r.Name())
	if err != nil {
		return storeErr(err)
	}
	var oldest time.Time
	for _, e := range list {
		if oldest.IsZero() || e.Published.Before(oldest) {
			oldest = e.Published
		}
	}
	checked := 0
	for _, v := range visible {
		if checked >= github.PerPage {
			break
		}
		if !v.MissingSince.IsZero() || hasTag(list, v.Tag) {
			continue
		}
		switch {
		case len(list) == 0:
		case v.Kind != string(m):
			continue
		case !complete && !v.Published.After(oldest):
			continue
		}
		checked++
		if err := p.verify(ctx, v); err != nil {
			return err
		}
	}
	return nil
}

// verify runs one existence check and records the outcome. A rate limit is
// returned so the stage stops; other failures only record the attempt.
func (p *Poller) verify(ctx context.Context, r *store.Release) error {
	ok, err := p.GH.Exists(ctx, r.Repo, r.Kind, r.Tag)
	outcome := store.VerifyFound
	switch {
	case err != nil:
		outcome = store.VerifyFailed
		p.Log.Warn("existence check failed", "repo", r.Repo, "tag", r.Tag, "err", err)
	case !ok:
		outcome = store.VerifyNotFound
		p.Log.Warn("release not found; it leaves the feed after the grace period unless it reappears",
			"repo", r.Repo, "tag", r.Tag, "grace", store.MissingGrace)
	}
	if serr := p.Store.RecordVerify(ctx, r.Repo, r.Tag, outcome, p.now()); serr != nil {
		return storeErr(serr)
	}
	if github.IsRateLimit(err) {
		return err
	}
	return nil
}

// publishWindow is how old a reconciled release may be and still be
// published: max(7d, notify.max_age) (§3.1).
func (p *Poller) publishWindow() time.Duration {
	return max(MinPublishWindow, p.Cfg.Notify.MaxAge.D)
}

// reconcilePending reports whether a reconciled release with a known date is
// pending rather than skipped: published within the publish window and
// after the repo was first scanned (§3.1).
func (p *Poller) reconcilePending(published, initialized time.Time) bool {
	return published.After(initialized) && p.now().Sub(published) <= p.publishWindow()
}

// reconcile runs the daily reconciliation of the releases list when due,
// page by page, resuming at reconcile_next_page (§3.1). It only inserts
// missing rows and never moves the scan boundary. Tag-only repos need none:
// their list is read in full on every poll.
func (p *Poller) reconcile(ctx context.Context, r *config.Repo, b *budget) error {
	name := r.Name()
	rs, err := p.Store.RepoState(ctx, name)
	if err != nil {
		return storeErr(err)
	}
	if rs.InitializedAt.IsZero() || (rs.ReconcileNextPage == 0 && p.now().Sub(rs.LastReconciledAt) < ReconcileEvery) {
		return nil
	}
	pages := *r.MaxPages + ReconcileExtra
	page := max(rs.ReconcileNextPage, 1)
	added := 0
	for ; page <= pages; page++ {
		if b.out() {
			return storeErr(p.setReconcilePage(ctx, name, page))
		}
		entries, err := p.releasesPage(ctx, r, page)
		if err != nil {
			if serr := p.setReconcilePage(ctx, name, page); serr != nil {
				return storeErr(serr)
			}
			return err
		}
		err = p.Store.Tx(ctx, func(tx *store.Tx) error {
			var newRows []*store.Release
			var tags []string
			for _, e := range entries {
				tags = append(tags, e.Tag)
				row := p.newRow(r, e, p.now())
				row.Origin, row.State = store.OriginReconcile, store.StatePending
				if p.filtered(r, row) || (!row.Published.IsZero() && !p.reconcilePending(row.Published, rs.InitializedAt)) {
					row.State = store.StateSkipped
				}
				ok, err := p.insert(ctx, tx, row, e)
				if err != nil {
					return err
				}
				if ok {
					newRows = append(newRows, row)
				}
			}
			added += len(newRows)
			if err := setPredecessors(ctx, tx, newRows); err != nil {
				return err
			}
			if err := tx.Seen(ctx, name, tags); err != nil {
				return err
			}
			st, err := tx.RepoState(ctx, name)
			if err != nil {
				return err
			}
			st.ReconcileNextPage = page + 1
			return tx.PutRepoState(ctx, st)
		})
		if err != nil {
			return storeErr(err)
		}
		if len(entries) < github.PerPage {
			break
		}
	}
	err = p.Store.Tx(ctx, func(tx *store.Tx) error {
		st, err := tx.RepoState(ctx, name)
		if err != nil {
			return err
		}
		st.ReconcileNextPage, st.LastReconciledAt = 0, p.now()
		return tx.PutRepoState(ctx, st)
	})
	if added > 0 {
		p.Log.Info("reconciliation found versions the scans missed", "repo", name, "added", added)
	}
	return storeErr(err)
}

func (p *Poller) setReconcilePage(ctx context.Context, repo string, page int) error {
	return p.Store.Tx(ctx, func(tx *store.Tx) error {
		st, err := tx.RepoState(ctx, repo)
		if err != nil {
			return err
		}
		st.ReconcileNextPage = page
		return tx.PutRepoState(ctx, st)
	})
}

// drainMetadata resolves tag dates from the metadata queue, at most
// MetadataPerTick per tick (§3.1 step 2). Dates are frozen once set.
func (p *Poller) drainMetadata(ctx context.Context, tick int64, b *budget) error {
	rows, err := p.Store.UndatedTags(ctx, tick, MetadataPerTick)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if b.out() {
			return nil
		}
		date, err := p.GH.TagDate(ctx, r.Repo, r.RefSHA, r.RefType)
		if serr := p.Store.MetaAttempted(ctx, r.Repo, r.Tag, p.now()); serr != nil {
			return serr
		}
		switch {
		case github.IsRateLimit(err):
			b.stop(err.Error())
			return nil
		case errors.Is(err, github.ErrNotFound):
			p.Log.Warn("tag target not found; the tag stays unpublished", "repo", r.Repo, "tag", r.Tag)
			continue
		case err != nil:
			p.Log.Warn("resolving a tag date failed", "repo", r.Repo, "tag", r.Tag, "err", err)
			continue
		}
		err = p.Store.Tx(ctx, func(tx *store.Tx) error {
			if err := tx.SetPublished(ctx, r.Repo, r.Tag, date.UTC()); err != nil {
				return err
			}
			if r.Origin == store.OriginReconcile && r.State == store.StatePending {
				rs, err := tx.RepoState(ctx, r.Repo)
				if err != nil {
					return err
				}
				if !p.reconcilePending(date, rs.InitializedAt) {
					if err := tx.Skip(ctx, r.Repo, r.Tag); err != nil {
						return err
					}
				}
			}
			if r.Version == "" && r.State != store.StateSkipped { // date-ordered: the predecessor needs the date
				r.Published = date.UTC()
				return setPredecessors(ctx, tx, []*store.Release{r})
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// verifySweep runs the rotating existence checks: up to verify_per_tick
// visible releases, least recently attempted first (§3.1).
func (p *Poller) verifySweep(ctx context.Context, tick int64, b *budget) error {
	rows, err := p.Store.VerifyDue(ctx, tick, p.Cfg.GitHub.VerifyPerTick)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if b.out() {
			return nil
		}
		err := p.verify(ctx, r)
		var se storeError
		if errors.As(err, &se) {
			return se.err
		}
		if github.IsRateLimit(err) {
			b.stop(err.Error())
			return nil
		}
	}
	return nil
}

// Promote publishes pending releases older than llm.publish_after as
// fallbacks (§3.3). Each repo must be confirmed this tick and pass a
// just-in-time public check first; one check covers a repo's promotions in
// this pass (§3.1, processing gate). It returns how many were promoted.
func (p *Poller) Promote(ctx context.Context, tick int64) (int, error) {
	now := p.now()
	due, err := p.Store.DuePromotions(ctx, now.Add(-p.Cfg.LLM.PublishAfter.D))
	if err != nil {
		return 0, err
	}
	byRepo := map[string][]*store.Release{}
	var repos []string
	for _, r := range due {
		if _, ok := byRepo[r.Repo]; !ok {
			repos = append(repos, r.Repo)
		}
		byRepo[r.Repo] = append(byRepo[r.Repo], r)
	}
	promoted := 0
	for _, repo := range repos {
		ok, err := p.Store.Confirmed(ctx, repo, tick)
		if err != nil {
			return promoted, err
		}
		if !ok {
			continue
		}
		public, err := p.checkNow(ctx, repo, tick, "promotion")
		if err != nil {
			return promoted, err
		}
		if !public {
			continue
		}
		err = p.Store.Tx(ctx, func(tx *store.Tx) error {
			for _, r := range byRepo[repo] {
				if err := tx.Promote(ctx, r.Repo, r.Tag, now); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return promoted, err
		}
		promoted += len(byRepo[repo])
	}
	return promoted, nil
}
