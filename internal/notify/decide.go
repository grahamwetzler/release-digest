// Package notify decides which releases are urgent security releases and
// alerts them through ntfy (docs/security-alerts.md): the urgency decision, the alert
// stage of a tick, the ntfy client and the outbox worker that delivers.
package notify

import (
	"regexp"
	"slices"

	"release-digest/internal/advisory"
	"release-digest/internal/config"
	"release-digest/internal/store"
	"release-digest/internal/summarize"
)

// Inputs are everything an urgency decision reads.
type Inputs struct {
	Release     *store.Release
	Notes       string
	Summary     *summarize.Summary // nil unless summarized
	Links       []store.Link
	Packages    []config.Package
	Prerelease  *regexp.Regexp // the repo's prerelease_pattern, or nil
	MinSeverity string
}

// LinkResult is the decision for one advisory mentioned in the notes.
type LinkResult struct {
	ID       string
	Matched  bool   // it belongs to the repo
	Fixes    bool   // this release fixes it
	Evidence string // version | text | "" (none)
	Fires    bool   // it makes the release urgent
	Severity string
	Note     string              // why it doesn't fire, for logs and notify test
	Matching []advisory.Advisory // the advisories under the ID that belong to the repo
}

// Decision is a release's urgency and the signals behind it.
type Decision struct {
	Urgency  string // none | advisory | urgent
	Source   string // advisory:GHSA-… | llm | ""
	Priority int    // ntfy priority: 5 for a critical advisory or LLM urgent, else 4
	Links    []LinkResult
	LLM      string // the summary's urgency after the keyword check; "" without a summary
	// Downgraded is set when the LLM said urgent but the summary lists no
	// security change or the notes have no security keyword (docs/security-alerts.md).
	Downgraded bool
}

// Firing returns the links that make the release urgent, highest severity first.
func (d *Decision) Firing() []LinkResult {
	var out []LinkResult
	for _, l := range d.Links {
		if l.Fires {
			out = append(out, l)
		}
	}
	slices.SortStableFunc(out, func(a, b LinkResult) int { return advisory.Rank(b.Severity) - advisory.Rank(a.Severity) })
	return out
}

// securityKeyword is what the notes must contain for an LLM "urgent" to count.
var securityKeyword = regexp.MustCompile(`(?i)security|vulnerab|CVE-|GHSA-|exploit`)

// Decide computes a release's urgency as the maximum of the advisory
// signal and the LLM signal (docs/security-alerts.md).
func Decide(in Inputs) Decision {
	d := Decision{Urgency: store.UrgencyNone}
	r := in.Release
	advisoryLevel := false
	for _, l := range in.Links {
		res := LinkResult{ID: l.AdvisoryID}
		a := l.Advisory
		switch {
		case a == nil || a.FetchedAt.IsZero():
			res.Note = "not looked up yet"
		case !a.Found:
			res.Note = "not in GitHub's advisory database"
		}
		if res.Note != "" {
			d.Links = append(d.Links, res)
			continue
		}
		advs, err := advisory.Parse(a.JSON)
		if err != nil {
			res.Note = err.Error()
			d.Links = append(d.Links, res)
			continue
		}
		withdrawn := true
		for _, adv := range advs {
			ok, vulns := adv.Matches(r.Repo, in.Packages)
			if !ok {
				continue
			}
			res.Matched = true
			res.Matching = append(res.Matching, adv)
			fixes, how := false, ""
			switch advisory.CheckVersions(r.Tag, r.PrevTag, vulns, in.Prerelease) {
			case advisory.Fixes:
				fixes, how = true, "version"
			case advisory.Unavailable:
				if advisory.TextFixes(in.Notes, l.AdvisoryID) {
					fixes, how = true, "text"
				}
			}
			if !fixes {
				continue
			}
			res.Fixes, res.Evidence = true, how
			if adv.Withdrawn() {
				continue
			}
			withdrawn = false
			if advisory.Rank(adv.Severity) > advisory.Rank(res.Severity) || res.Severity == "" {
				res.Severity = adv.Severity
			}
		}
		switch {
		case !res.Matched:
			res.Note = "unmatched: it isn't for this repo or its configured packages"
		case !res.Fixes:
			res.Note = "no evidence that this release fixes it"
		case withdrawn:
			res.Note = "withdrawn"
		case !advisory.AtLeast(res.Severity, in.MinSeverity):
			res.Note = "severity " + res.Severity + " is below notify.min_severity " + in.MinSeverity
			advisoryLevel = true
		default:
			res.Fires = true
		}
		d.Links = append(d.Links, res)
	}

	if s := in.Summary; s != nil {
		d.LLM = s.Urgency()
		if d.LLM == summarize.UrgencyUrgent && (len(s.Security) == 0 || !securityKeyword.MatchString(in.Notes)) {
			d.LLM, d.Downgraded = summarize.UrgencyAdvisory, true
		}
	}

	firing := d.Firing()
	switch {
	case len(firing) > 0:
		d.Urgency, d.Source = store.UrgencyUrgent, "advisory:"+firing[0].ID
		if len(firing[0].Matching) > 0 && firing[0].Matching[0].GHSA != "" && !advisory.IsGHSA(firing[0].ID) {
			d.Source = "advisory:" + firing[0].Matching[0].ID()
		}
	case d.LLM == summarize.UrgencyUrgent:
		d.Urgency, d.Source = store.UrgencyUrgent, "llm"
	case advisoryLevel:
		d.Urgency, d.Source = store.UrgencyAdvisory, "advisory"
	case d.LLM == summarize.UrgencyAdvisory:
		d.Urgency, d.Source = store.UrgencyAdvisory, "llm"
	}
	if d.Urgency == store.UrgencyUrgent {
		d.Priority = 4
		if d.LLM == summarize.UrgencyUrgent || (len(firing) > 0 && advisory.Rank(firing[0].Severity) == 4) {
			d.Priority = 5
		}
	}
	return d
}
