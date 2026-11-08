package secsignal

import (
	"reflect"
	"testing"
)

func TestScan(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{"empty", "", nil},
		{"ordinary notes", "Faster startup and documentation", nil},
		{"IDs", "cve-2024-1234 CVE-2024-123456789 ghsa-2345-6789-CFGH", []string{"CVE-2024-1234", "CVE-2024-123456789", "GHSA-2345-6789-cfgh"}},
		{"invalid IDs", "XCVE-2024-12345 CVE-2024-123456x CVE-2024-123 CVE-024-1234 GHSA-1234-6789-cfgh GHSA-23456-6789-cfgh GHSA-2345-6789-cfghx", nil},
		{"Unicode boundaries", "éRCE RCE界 ٢XSS XSS٣ éCVE-2024-1234 CVE-2024-1234界 GHSA-2345-6789-cfghé", nil},
		{"word boundaries", "force vulnerabilityish insecuresecurity fixes sql injections xssed sssrf auth bypassed", nil},
		{"punctuation boundaries", "_RCE_ (**XSS**) [CVE-2024-1234]", []string{"remote code execution", "cross-site scripting", "CVE-2024-1234"}},
		{"all phrases", "VULNERABILITIES; security fixes; security advisories; security issues; remote code execution; privilege escalation; cross-site scripting; SQL injection; directory traversal; server-side request forgery; auth bypass", []string{"vulnerability", "security fix", "security advisory", "security issue", "remote code execution", "privilege escalation", "cross-site scripting", "sql injection", "path traversal", "server-side request forgery", "authentication bypass"}},
		{"singular and aliases", "vulnerability security fix security advisory security issue RCE XSS path traversal SSRF authentication bypass", []string{"vulnerability", "security fix", "security advisory", "security issue", "remote code execution", "cross-site scripting", "path traversal", "server-side request forgery", "authentication bypass"}},
		{"whitespace", "remote\tcode\nexecution security\r\nfixes cross \t site\nscripting server\nside\trequest forgery auth\u00a0bypass sql\u0085injection path\vtraversal privilege\fescalation security\u2028issue security\u2029advisory", []string{"remote code execution", "security fix", "cross-site scripting", "server-side request forgery", "authentication bypass", "sql injection", "path traversal", "privilege escalation", "security issue", "security advisory"}},
		{"invalid separators", "security-fix remotecode execution cross--site scripting server- side request forgery authentication_bypass", nil},
		{"appearance and dedupe", "SSRF cve-2024-1234 RCE server-side request forgery CVE-2024-1234 remote code execution ghsa-2345-6789-CFGH GHSA-2345-6789-cfgh vulnerabilities vulnerability", []string{"server-side request forgery", "CVE-2024-1234", "remote code execution", "GHSA-2345-6789-cfgh", "vulnerability"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Scan(tc.text); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Scan(%q) = %#v, want %#v", tc.text, got, tc.want)
			}
		})
	}
}
