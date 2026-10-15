package version

import (
	"regexp"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		tag, stream, version string
		pre                  bool
	}{
		{"v2.0.0-rc.1", "", "2.0.0-rc.1", true},
		{"v1.2.3", "", "1.2.3", false},
		{"1.2", "", "1.2", false},
		{"go1.28rc1", "go", "1.28-rc1", true},
		{"go1.28.1", "go", "1.28.1", false},
		{"1.0.0beta2", "", "1.0.0-beta2", true},
		{"1.0.0.beta.1", "", "1.0.0-beta.1", true},
		{"service/api/v1.4.0", "service/api/", "1.4.0", false},
		{"service/api/v1.5.0-alpha.1", "service/api/", "1.5.0-alpha.1", true},
		{"release-1.4", "release-", "1.4", false},
		{"dev1.0", "dev", "1.0", false}, // "v" ends a word here, so it stays in the stream
		{"v1.2.3+build.5", "", "1.2.3+build.5", false},
		{"v1.2.3_final", "", "1.2.3+_final", false},
		{"1.2.3.4.5", "", "1.2.3.4+.5", false},
		{"latest", "", "", false}, // date-ordered tags share the empty stream
		{"stable", "", "", false},
	} {
		v := Parse(tc.tag, nil)
		if v.Stream != tc.stream || v.String() != tc.version || v.Prerelease != tc.pre {
			t.Errorf("Parse(%q) = stream %q version %q pre %v; want %q %q %v", tc.tag, v.Stream, v.String(), v.Prerelease, tc.stream, tc.version, tc.pre)
		}
	}
	// A custom pattern replaces the default one.
	custom := regexp.MustCompile(`^-?next`)
	if v := Parse("1.0.0next", custom); !v.Prerelease {
		t.Error("custom pattern not applied")
	}
	if v := Parse("1.0.0beta", custom); v.Prerelease {
		t.Error("default pattern still applied")
	}
}

func TestCompare(t *testing.T) {
	ordered := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc2", "1.0.0-rc10", "1.0.0", "1.0.1", "1.2", "1.10.0", "2.0.0"}
	for i := 0; i+1 < len(ordered); i++ {
		a, b := Parse(ordered[i], nil), Parse(ordered[i+1], nil)
		if Compare(a, b) >= 0 || Compare(b, a) <= 0 {
			t.Errorf("%s should sort before %s", ordered[i], ordered[i+1])
		}
	}
	if Compare(Parse("1.28", nil), Parse("1.28.0", nil)) != 0 {
		t.Error("missing components should count as 0")
	}
}

func TestPredecessor(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	cand := func(tag string, pub time.Time, prerelease bool) Candidate {
		v := Parse(tag, nil)
		return Candidate{Tag: tag, Stream: v.Stream, Version: v.String(), Prerelease: prerelease || v.Prerelease, Published: pub}
	}
	stored := []Candidate{
		cand("v1.2.2", day(1), false),
		cand("v1.2.3", day(2), false),
		cand("v2.0.0-rc.1", day(3), false),
		cand("v2.0.0", day(4), false),
		cand("v1.2.4", day(5), false), // maintenance release after 2.0.0
		cand("v1.3.0-rc.1", day(6), false),
		cand("v1.3.0-rc.2", day(7), false),
		cand("v1.3.0", day(8), false),
		cand("api/v0.9.0", day(2), false),
		cand("api/v1.0.0", day(9), false),
		cand("nightly", day(1), false), // date-ordered: never a SemVer predecessor
	}
	for _, tc := range []struct{ tag, want string }{
		{"v1.2.4", "v1.2.3"},           // not 2.0.0, which shipped in between
		{"v1.3.0", "v1.2.4"},           // a stable release skips its RCs
		{"v1.3.0-rc.2", "v1.3.0-rc.1"}, // a pre-release considers everything
		{"v2.0.0-rc.1", "v1.3.0"},
		{"v1.2.2", ""},               // the repo's first version
		{"api/v1.0.0", "api/v0.9.0"}, // streams don't cross
		{"api/v0.9.0", ""},
	} {
		var this Candidate
		for _, c := range stored {
			if c.Tag == tc.tag {
				this = c
			}
		}
		if got := Predecessor(this, stored); got != tc.want {
			t.Errorf("Predecessor(%s) = %q, want %q", tc.tag, got, tc.want)
		}
	}

	dated := []Candidate{
		cand("stable", day(1), false),
		cand("latest", day(3), false),
		cand("edge", day(2), false),
		cand("v9.0.0", day(2), false), // parsed: never a date-ordered predecessor
		cand("snap-undated", time.Time{}, false),
	}
	if got := Predecessor(dated[1], dated); got != "edge" {
		t.Errorf("date-ordered predecessor = %q, want edge", got)
	}
	if got := Predecessor(dated[4], dated); got != "" {
		t.Errorf("undated tag predecessor = %q, want none", got)
	}
}
