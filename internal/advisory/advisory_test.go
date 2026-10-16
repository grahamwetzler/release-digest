package advisory

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"release-digest/internal/config"
)

func TestExtract(t *testing.T) {
	notes := `## Security
- Fixes ghsa-CfGh-2345-mpqr (cve-2026-12345)
- See also CVE-2026-12345 and GHSA-cfgh-2345-mpqr again
- Not an ID: GHSA-aaaa-bbbb (too short), CVE-26-1, XCVE-2026-1111
`
	got := Extract(notes)
	want := []string{"GHSA-cfgh-2345-mpqr", "CVE-2026-12345"}
	if !slices.Equal(got, want) {
		t.Errorf("Extract = %q, want %q", got, want)
	}
	if got := Extract("Fixes _CVE-2026-4321_ and *GHSA-cfgh-2345-mpqr*"); !slices.Equal(got, []string{"CVE-2026-4321", "GHSA-cfgh-2345-mpqr"}) {
		t.Errorf("Extract in emphasis = %q", got)
	}
	var many string
	for i := range MaxPerRelease + 5 {
		many += fmt.Sprintf("CVE-2026-%05d\n", i)
	}
	if n := len(Extract(many)); n != MaxPerRelease {
		t.Errorf("Extract kept %d IDs, want %d", n, MaxPerRelease)
	}
}

func TestTextFixes(t *testing.T) {
	const id = "CVE-2026-1234"
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"- Fix CVE-2026-1234 in parser", true},
		{"Security: Fixes CVE-2026-1234 (#123)", true},
		{"* **Security:** Patches CVE-2026-1234", true},
		{"[security] resolves CVE-2026-1234", true},
		{"CVE-2026-1234 is patched", true},
		{"1. Addressed CVE-2026-1234 by validating input", true},

		{"We do not fix CVE-2026-1234", false},
		{"We will fix CVE-2026-1234 in the next release", false},
		{"A fix is planned for CVE-2026-1234", false},
		{"fix planned for CVE-2026-1234", false},
		{"CVE-2026-1234 isn't fixed yet", false},
		{"CVE-2026-1234 wasn’t fully patched", false},
		{"CVE-2026-1234 wasnt fixed", false},
		{"CVE-2026-1234 is partially mitigated", false},
		{"upstream patch for CVE-2026-1234 pending", false},
		{"this release is about CVE-2026-1234", false},
		{"Bump foo to address CVE-2026-1234", false}, // the verb doesn't start the line
		{"CVE-2026-1234 is not yet fixed", false},
		{"Known issue: CVE-2026-1234 (fixed in 2.0)", false},
		{"Fix CVE-2026-9999", false},                // another advisory
		{"Fix _CVE-2026-1234_ in the parser", true}, // Markdown emphasis
		{"Fix **CVE-2026-1234**", true},
		{"Fix CVE-2026-12345", false},                       // a longer ID that starts the same
		{"- Fix cve-2026-1234 (also CVE-2026-12345)", true}, // case-insensitive, whole ID
	} {
		if got := TextFixes("intro\n"+tc.line+"\nmore", id); got != tc.want {
			t.Errorf("%q: %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestAtLeast(t *testing.T) {
	for _, tc := range []struct {
		sev, min string
		want     bool
	}{
		{"critical", "high", true}, {"high", "high", true}, {"medium", "high", false},
		{"medium", "medium", true}, {"low", "medium", false}, {"unknown", "medium", false},
		{"", "medium", false}, {"HIGH", "critical", false}, {"CRITICAL", "critical", true},
	} {
		if got := AtLeast(tc.sev, tc.min); got != tc.want {
			t.Errorf("AtLeast(%q, %q) = %v", tc.sev, tc.min, got)
		}
	}
}

func advisory(t *testing.T, s string) *Advisory {
	t.Helper()
	var a Advisory
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		t.Fatal(err)
	}
	return &a
}

func TestMatches(t *testing.T) {
	a := advisory(t, `{"ghsa_id":"GHSA-aaaa-bbbb-cccc","severity":"high","source_code_location":"https://github.com/Foo/Bar.git",
		"vulnerabilities":[
			{"package":{"ecosystem":"pip","name":"Foo_Core"},"vulnerable_version_range":"< 1.2.3","first_patched_version":"1.2.3"},
			{"package":{"ecosystem":"npm","name":"@foo/web"},"vulnerable_version_range":"< 2.0.0","first_patched_version":"2.0.0"}]}`)
	if ok, vulns := a.Matches("foo/bar", nil); !ok || len(vulns) != 2 {
		t.Errorf("source_code_location: %v, %d vulnerabilities", ok, len(vulns))
	}
	if ok, vulns := a.Matches("other/repo", []config.Package{{Ecosystem: "pip", Name: "foo-core"}}); !ok || len(vulns) != 1 ||
		vulns[0].Package.Name != "Foo_Core" {
		t.Errorf("package (pip-normalized name): %v, %+v", ok, vulns)
	}
	// A monorepo lists two of its packages.
	if ok, vulns := a.Matches("other/mono", []config.Package{{Ecosystem: "npm", Name: "@foo/web"}, {Ecosystem: "pip", Name: "foo.core"}}); !ok || len(vulns) != 2 {
		t.Errorf("two packages: %v, %d", ok, len(vulns))
	}
	if ok, _ := a.Matches("other/repo", []config.Package{{Ecosystem: "npm", Name: "foo-core"}}); ok {
		t.Error("matched a package of another ecosystem")
	}
	if ok, _ := a.Matches("foo/barbaz", nil); ok {
		t.Error("matched another repo")
	}
}

func TestCheckVersions(t *testing.T) {
	a := advisory(t, `{"vulnerabilities":[
		{"package":{"ecosystem":"go","name":"x"},"vulnerable_version_range":">= 1.0.0, < 1.2.3","first_patched_version":"1.2.3"},
		{"package":{"ecosystem":"go","name":"x"},"vulnerable_version_range":">= 2.0.0, < 2.0.5","first_patched_version":{"identifier":"2.0.5"}}]}`)
	for _, tc := range []struct {
		tag, prev string
		want      VersionEvidence
	}{
		{"v1.2.3", "v1.2.2", Fixes},
		{"v1.2.4", "v1.2.2", Fixes}, // skipped the patched version's tag
		{"v2.0.5", "v2.0.4", Fixes},
		{"svc/v2.0.5", "svc/v2.0.0-rc.1", NotFixes}, // 2.0.0-rc.1 < 2.0.0: not in range
		{"v1.2.4", "v1.2.3", NotFixes},              // already patched before
		{"v1.2.2", "v1.2.1", NotFixes},              // still vulnerable
		{"v1.2.3", "", Unavailable},                 // first tracked version
		{"latest", "v1.2.2", Unavailable},
	} {
		if got := CheckVersions(tc.tag, tc.prev, a.Vulnerabilities, nil); got != tc.want {
			t.Errorf("%s after %s: %v, want %v", tc.tag, tc.prev, got, tc.want)
		}
	}
	none := advisory(t, `{"vulnerabilities":[{"package":{"ecosystem":"go","name":"x"},"vulnerable_version_range":"< 9.0","first_patched_version":null}]}`)
	if got := CheckVersions("v1.0.1", "v1.0.0", none.Vulnerabilities, nil); got != Unavailable {
		t.Errorf("no patched version: %v", got)
	}
}
