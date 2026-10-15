package main

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"

	"release-digest/internal/config"
	"release-digest/internal/secret"
)

// cmdConfigLint validates the merged config and the secrets' sources. It
// reports where each secret comes from, never its value (§4.3). Checks that
// need the network (repos reachable, LLM answering) come in later milestones.
func cmdConfigLint(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("config lint", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, err := config.Load(*cfgPath, os.LookupEnv)
	if err != nil {
		return reportErrors(err, stderr)
	}
	loaded, err := loadSecrets(secret.OS)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "ok: %s, %d repo(s)\n", *cfgPath, len(c.Repos))
	for _, r := range c.Repos {
		from := "yaml"
		if r.FromEnv {
			from = config.EnvRepos
		}
		fmt.Fprintf(stdout, "  %s (%s)\n", r.Name(), from)
	}
	for i, l := range loaded {
		fmt.Fprintf(stdout, "%s: %s\n", secretSpecs[i].Label, l.Source)
	}
	if !loaded[1].Present() {
		fmt.Fprintln(stdout, "warning: no GitHub token; requests are limited to 60 per hour")
	}
	if !loaded[0].Present() && !localURL(c.LLM.BaseURL) {
		fmt.Fprintf(stdout, "warning: llm.base_url %s is not a local address and there is no llm api key\n", c.LLM.BaseURL)
	}
	return 0
}

// localURL reports whether u's host is loopback, private, or a bare
// single-label name such as a compose service ("ollama").
func localURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	host := p.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	return host == "localhost" || !strings.Contains(host, ".")
}
