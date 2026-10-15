// Package ident derives stable item identifiers (PLAN.md §5).
package ident

import (
	"crypto/sha1"
	"fmt"
	"strings"
)

// Namespace is the UUIDv5 namespace for release GUIDs. It is fixed forever:
// changing it changes every item's identity and re-shows every entry.
var Namespace = [16]byte{
	0x6b, 0x0e, 0x4f, 0x3a, 0x9c, 0x21, 0x4d, 0x57,
	0x8a, 0x3e, 0x2f, 0x1d, 0x90, 0xc4, 0x7b, 0x15,
}

// GUID is the UUIDv5 of "{owner}/{repo}@{tag}" with the repo lowercased and
// the tag kept exact, in canonical 8-4-4-4-12 form.
func GUID(repo, tag string) string {
	return uuidV5(Namespace, strings.ToLower(repo)+"@"+tag)
}

// uuidV5 implements RFC 9562 §5.5.
func uuidV5(ns [16]byte, name string) string {
	h := sha1.New()
	h.Write(ns[:])
	h.Write([]byte(name))
	var u [16]byte
	copy(u[:], h.Sum(nil))
	u[6] = u[6]&0x0f | 0x50 // version 5
	u[8] = u[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
