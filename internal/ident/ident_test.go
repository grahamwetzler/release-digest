package ident

import (
	"regexp"
	"testing"
)

var canonical = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestUUIDv5KnownVector(t *testing.T) {
	// RFC 9562 Appendix A.4: the DNS namespace and "www.example.com".
	dns := [16]byte{0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	if got, want := uuidV5(dns, "www.example.com"), "2ed6657d-e927-568b-95e1-2665a8aea6a2"; got != want {
		t.Errorf("uuidV5 = %s, want %s", got, want)
	}
}

func TestGUID(t *testing.T) {
	a := GUID("Astral-SH/uv", "0.9.4")
	if a != GUID("astral-sh/uv", "0.9.4") {
		t.Error("repo case changes the GUID")
	}
	if a == GUID("astral-sh/uv", "v0.9.4") {
		t.Error("tag is not kept exact")
	}
	for _, tag := range []string{"v1.0.0", "pkg/a/v1.2.3", "1.0.0+build.5", "版本-1", "a b\"<&>"} {
		if g := GUID("o/r", tag); !canonical.MatchString(g) {
			t.Errorf("GUID for %q is not a canonical UUIDv5: %s", tag, g)
		}
	}
}
