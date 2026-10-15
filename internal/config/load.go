package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Error is one validation problem, positioned at file:line when the line is known.
type Error struct {
	File string
	Line int
	Path string // dotted key path, e.g. repos[2].tag_pattern
	Msg  string
}

func (e Error) Error() string {
	var b strings.Builder
	b.WriteString(e.File)
	if e.Line > 0 {
		fmt.Fprintf(&b, ":%d", e.Line)
	}
	b.WriteString(": ")
	if e.Path != "" {
		b.WriteString(e.Path + ": ")
	}
	b.WriteString(e.Msg)
	return b.String()
}

// Errors collects every problem found while loading.
type Errors []Error

func (es Errors) Error() string {
	lines := make([]string, len(es))
	for i, e := range es {
		lines[i] = e.Error()
	}
	return strings.Join(lines, "\n")
}

// LookupEnv reads one environment variable; os.LookupEnv in production.
type LookupEnv func(string) (string, bool)

// Load reads the config at path, applies env overrides (§4.1) and validates
// it. On failure the error is an Errors listing every problem found.
func Load(path string, env LookupEnv) (*Config, error) {
	if env == nil {
		env = os.LookupEnv
	}
	c := &Config{File: path}
	root, errs := decodeStrict(path, c)
	if root == nil {
		return nil, errs
	}
	v := &validator{file: path, root: root, errs: errs}
	v.applyEnv(c, env)
	v.validate(c, filepath.Dir(path))
	if len(v.errs) > 0 {
		return nil, v.errs
	}
	return c, nil
}

// secretKeys may never appear in the YAML file, at any depth (§4.3).
var secretKeys = map[string]string{
	"api_key":  "LLM_API_KEY_FILE (or /run/secrets/llm_api_key)",
	"apikey":   "LLM_API_KEY_FILE (or /run/secrets/llm_api_key)",
	"token":    "GITHUB_TOKEN_FILE or NTFY_TOKEN_FILE (or /run/secrets/…)",
	"password": "a Docker secret",
}

// decodeStrict decodes a YAML file into v, rejecting unknown keys and secret
// keys, and returns the node tree so validation can report line numbers.
func decodeStrict(path string, v any) (*yaml.Node, Errors) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, Errors{{File: path, Msg: err.Error()}}
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, yamlErrors(path, err)
	}
	if len(root.Content) == 0 {
		return nil, Errors{{File: path, Msg: "file is empty"}}
	}
	var errs Errors
	secretLines := map[int]bool{}
	walkKeys(&root, nil, func(keyPath []any, key *yaml.Node) {
		if hint, bad := secretKeys[strings.ToLower(key.Value)]; bad {
			secretLines[key.Line] = true
			errs = append(errs, Error{File: path, Line: key.Line, Path: pathString(keyPath),
				Msg: "secrets are never read from the config file; use " + hint + " (see PLAN.md §4.3)"})
		}
	})
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		var te *yaml.TypeError
		if !errors.As(err, &te) {
			return nil, append(errs, yamlErrors(path, err)...)
		}
		// A TypeError (unknown key, wrong type) still decodes everything else,
		// so validation can go on and report the rest of the file's problems.
		// A secret key is already reported, so its "unknown key" is dropped.
		for _, e := range yamlErrors(path, err) {
			if !secretLines[e.Line] {
				errs = append(errs, e)
			}
		}
	}
	// A second document would otherwise be silently ignored.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, append(errs, Error{File: path, Line: extra.Line, Msg: "only one YAML document is allowed (found another after ---)"})
	} else if !errors.Is(err, io.EOF) {
		return nil, append(errs, yamlErrors(path, err)...)
	}
	return &root, errs
}

// walkKeys calls fn for every mapping key in the tree with its key path.
func walkKeys(n *yaml.Node, at []any, fn func(path []any, key *yaml.Node)) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			walkKeys(c, at, fn)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			p := append(slices.Clip(at), n.Content[i].Value)
			fn(p, n.Content[i])
			walkKeys(n.Content[i+1], p, fn)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			walkKeys(c, append(slices.Clip(at), i), fn)
		}
	}
}

var unknownField = regexp.MustCompile(`^field (\S+) not found in type \S+$`)

var yamlLinePrefix = regexp.MustCompile(`^(?:yaml: )?line (\d+): `)

// yamlErrors splits a yaml.v3 error into positioned Errors.
func yamlErrors(path string, err error) Errors {
	var msgs []string
	var te *yaml.TypeError
	if errors.As(err, &te) {
		msgs = te.Errors
	} else {
		msgs = []string{err.Error()}
	}
	var out Errors
	for _, m := range msgs {
		e := Error{File: path, Msg: m}
		if sub := yamlLinePrefix.FindStringSubmatch(m); sub != nil {
			e.Line, _ = strconv.Atoi(sub[1])
			e.Msg = m[len(sub[0]):]
		}
		if sub := unknownField.FindStringSubmatch(e.Msg); sub != nil {
			e.Msg = fmt.Sprintf("unknown key %q", sub[1])
		}
		out = append(out, e)
	}
	return out
}

// lineOf finds the line of a key path (string keys, int list indices) in a
// YAML tree. If the full path is absent, it returns the deepest part that
// exists, so "missing key" errors point at the enclosing block.
func lineOf(root *yaml.Node, path ...any) int {
	if root == nil {
		return 0
	}
	n := root
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	line := 0
	for _, p := range path {
		switch k := p.(type) {
		case string:
			if n.Kind != yaml.MappingNode {
				return line
			}
			found := false
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == k {
					line, n, found = n.Content[i].Line, n.Content[i+1], true
					break
				}
			}
			if !found {
				return line
			}
		case int:
			if n.Kind != yaml.SequenceNode || k >= len(n.Content) {
				return line
			}
			n = n.Content[k]
			line = n.Line
		}
	}
	return line
}

// has reports whether a key path exists in the YAML tree.
func has(root *yaml.Node, path ...any) bool {
	if root == nil {
		return false
	}
	n := root
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	for _, part := range path {
		found := false
		switch k := part.(type) {
		case string:
			for i := 0; n.Kind == yaml.MappingNode && i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == k {
					n, found = n.Content[i+1], true
					break
				}
			}
		case int:
			if n.Kind == yaml.SequenceNode && k < len(n.Content) {
				n, found = n.Content[k], true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func pathString(path []any) string {
	var b strings.Builder
	for _, part := range path {
		switch k := part.(type) {
		case string:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(k)
		case int:
			fmt.Fprintf(&b, "[%d]", k)
		}
	}
	return b.String()
}

// validator accumulates errors for one file.
type validator struct {
	file string
	root *yaml.Node
	errs Errors
}

func p(parts ...any) []any { return parts }

func (v *validator) err(path []any, format string, args ...any) {
	v.errs = append(v.errs, Error{File: v.file, Line: lineOf(v.root, path...), Path: pathString(path), Msg: fmt.Sprintf(format, args...)})
}

// envErr reports a problem with an env var rather than a file position.
func (v *validator) envErr(name, format string, args ...any) {
	v.errs = append(v.errs, Error{File: "env " + name, Msg: fmt.Sprintf(format, args...)})
}

// absURL validates an absolute http(s) URL.
func (v *validator) absURL(path []any, raw string) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		v.err(path, "must be an absolute http(s) URL, got %q", raw)
	}
}

func (v *validator) duration(path []any, d *Duration, def string, allowZero bool) {
	if d.Raw == "" {
		d.Raw = def
	}
	if err := d.compile(allowZero); err != nil {
		v.err(path, "%v", err)
	}
}

func (v *validator) atLeast(path []any, n *int, def, min int) {
	if !has(v.root, path...) {
		*n = def // absent; an explicit 0 is validated as written
	}
	if *n < min {
		v.err(path, "must be at least %d, got %d", min, *n)
	}
}

// applyEnv applies the env overrides of §4.1 before validation.
func (v *validator) applyEnv(c *Config, env LookupEnv) {
	if s, ok := env(EnvPublicBaseURL); ok && s != "" {
		c.PublicBaseURL = s
	}
	if s, ok := env(EnvScheduleInterval); ok && s != "" {
		c.Schedule.Interval = Duration{Raw: s}
	}
	if s, ok := env(EnvLLMBaseURL); ok && s != "" {
		c.LLM.BaseURL = s
	}
	if s, ok := env(EnvLLMModel); ok && s != "" {
		c.LLM.Model = s
	}
	if s, ok := env(EnvRepos); ok {
		v.mergeEnvRepos(c, s)
	}
}

// repoName is owner/repo as GitHub allows it.
var repoName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?/[A-Za-z0-9._-]+$`)

// mergeEnvRepos merges RELEASE_DIGEST_REPOS into the YAML list (§4.1). A
// repo listed in both keeps its YAML options. "+pre" includes pre-releases.
func (v *validator) mergeEnvRepos(c *Config, list string) {
	seen := map[string]bool{}
	for _, r := range c.Repos {
		seen[strings.ToLower(r.Repo)] = true
	}
	for _, f := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		f = strings.TrimSpace(f)
		if f == "" || strings.HasPrefix(f, "#") {
			continue
		}
		name, flags, _ := strings.Cut(f, "+")
		r := &Repo{Repo: name, FromEnv: true}
		for _, flag := range strings.Split(flags, "+") {
			switch flag {
			case "":
			case "pre":
				t := true
				r.IncludePrereleases = &t
			default:
				v.envErr(EnvRepos, "%q: unknown flag +%s (only +pre is supported)", f, flag)
			}
		}
		if !repoName.MatchString(name) {
			v.envErr(EnvRepos, "%q is not owner/repo", f)
			continue
		}
		if seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		c.Repos = append(c.Repos, r)
	}
}

var (
	ecosystems  = []string{"actions", "composer", "erlang", "go", "maven", "npm", "nuget", "other", "pip", "pub", "rubygems", "rust", "swift"}
	severities  = []string{"critical", "high", "medium"}
	sourceKinds = []string{"releases", "tags"}
)

func (v *validator) validate(c *Config, dir string) {
	if c.Version != 1 {
		v.err(p("version"), "must be 1, got %d", c.Version)
	}
	c.StorePath = resolve(dir, orDefault(c.StorePath, DefaultStorePath))
	c.Listen = orDefault(c.Listen, DefaultListen)
	if _, port, err := net.SplitHostPort(c.Listen); err != nil || port == "" {
		v.err(p("listen"), "must be host:port or :port, got %q", c.Listen)
	}
	c.PublicBaseURL = strings.TrimRight(c.PublicBaseURL, "/")
	if c.PublicBaseURL == "" {
		v.err(p("public_base_url"), "is required (or set %s); it is used for the feeds' rel=self links", EnvPublicBaseURL)
	} else {
		v.absURL(p("public_base_url"), c.PublicBaseURL)
	}

	c.Log.Format = orDefault(c.Log.Format, "json")
	if c.Log.Format != "json" && c.Log.Format != "text" {
		v.err(p("log", "format"), "must be json or text, got %q", c.Log.Format)
	}
	c.Log.Level = orDefault(c.Log.Level, "info")
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, c.Log.Level) {
		v.err(p("log", "level"), "must be debug, info, warn or error, got %q", c.Log.Level)
	}

	c.Feed.Title = orDefault(c.Feed.Title, DefaultFeedTitle)
	c.Feed.Description = orDefault(c.Feed.Description, DefaultFeedDescription)
	v.atLeast(p("feed", "max_items"), &c.Feed.MaxItems, DefaultMaxItems, 1)
	v.atLeast(p("feed", "ttl"), &c.Feed.TTL, DefaultTTL, 0)

	s := &c.Schedule
	v.duration(p("schedule", "interval"), &s.Interval, DefaultInterval.String(), false)
	v.duration(p("schedule", "jitter"), &s.Jitter, DefaultJitter.String(), true)
	v.duration(p("schedule", "sweep_budget"), &s.SweepBudget, DefaultSweepBudget.String(), false)
	v.duration(p("schedule", "poll_budget"), &s.PollBudget, DefaultPollBudget.String(), false)
	v.duration(p("schedule", "summarize_budget"), &s.SummarizeBudget, DefaultSummarizeBudget.String(), false)
	if s.Jitter.D > 0 && s.Interval.D > 0 && s.Jitter.D >= s.Interval.D {
		v.err(p("schedule", "jitter"), "must be less than schedule.interval (%s)", s.Interval.Raw)
	}

	g := &c.GitHub
	g.APIURL = strings.TrimRight(orDefault(g.APIURL, DefaultGitHubAPI), "/")
	v.absURL(p("github", "api_url"), g.APIURL)
	v.atLeast(p("github", "verify_per_tick"), &g.VerifyPerTick, DefaultVerifyPerTick, 0)
	v.duration(p("github", "request_timeout"), &g.RequestTimeout, DefaultRequestTimeout.String(), false)

	l := &c.LLM
	l.BaseURL = strings.TrimRight(orDefault(l.BaseURL, DefaultLLMBaseURL), "/")
	v.absURL(p("llm", "base_url"), l.BaseURL)
	for name := range l.Headers {
		switch strings.ToLower(name) {
		case "authorization", "proxy-authorization", "x-api-key", "api-key", "cookie":
			v.err(p("llm", "headers", name), "carries a credential; set the key with LLM_API_KEY_FILE instead (see PLAN.md §4.3)")
		}
	}
	if l.Temperature == nil {
		t := DefaultTemperature
		l.Temperature = &t
	} else if *l.Temperature < 0 || *l.Temperature > 2 {
		v.err(p("llm", "temperature"), "must be between 0 and 2, got %g", *l.Temperature)
	}
	v.duration(p("llm", "timeout"), &l.Timeout, DefaultLLMTimeout.String(), false)
	v.atLeast(p("llm", "concurrency"), &l.Concurrency, DefaultConcurrency, 1)
	v.atLeast(p("llm", "max_per_pass"), &l.MaxPerPass, DefaultMaxPerPass, 1)
	v.atLeast(p("llm", "max_input_chars"), &l.MaxInputChars, DefaultMaxInputChars, 1000)
	v.duration(p("llm", "publish_after"), &l.PublishAfter, DefaultPublishAfter.String(), false)
	if l.PromptFile != "" {
		l.PromptFile = resolve(dir, l.PromptFile)
	}

	n := &c.Notify
	n.MinSeverity = orDefault(n.MinSeverity, DefaultMinSeverity)
	if !slices.Contains(severities, n.MinSeverity) {
		v.err(p("notify", "min_severity"), "must be critical, high or medium, got %q", n.MinSeverity)
	}
	v.duration(p("notify", "max_age"), &n.MaxAge, DefaultMaxAge.String(), false)
	if n.Ntfy != nil && n.Ntfy.URL != "" {
		v.absURL(p("notify", "ntfy", "url"), n.Ntfy.URL)
	}

	v.defaults(&c.Defaults)
	seen := map[string]int{}
	for i, r := range c.Repos {
		at := p("repos", i)
		if r.FromEnv {
			at = nil // positioned at the env var, already validated there
		}
		if !r.FromEnv {
			if r.Repo == "" {
				v.err(append(at, "repo"), "is required")
			} else if !repoName.MatchString(r.Repo) {
				v.err(append(at, "repo"), "must be owner/repo, got %q", r.Repo)
			}
			if j, dup := seen[r.Name()]; dup {
				v.err(append(at, "repo"), "duplicate repo %q (also repos[%d])", r.Repo, j)
			}
			seen[r.Name()] = i
		}
		v.repoOptions(at, &r.RepoOptions, &c.Defaults)
	}
}

// defaults fills and validates the defaults block, which every repo inherits.
func (v *validator) defaults(d *RepoOptions) {
	at := p("defaults")
	if d.Sources == nil {
		d.Sources = slices.Clone(DefaultSources)
	}
	if d.IncludePrereleases == nil {
		f := false
		d.IncludePrereleases = &f
	}
	if d.Backfill == nil {
		n := DefaultBackfill
		d.Backfill = &n
	}
	if d.MaxPages == nil {
		n := DefaultMaxPages
		d.MaxPages = &n
	}
	if d.Changelog == nil {
		s := DefaultChangelog
		d.Changelog = &s
	}
	v.repoOptions(at, d, nil)
}

// repoOptions validates o and, when def is non-nil, fills unset fields from it.
func (v *validator) repoOptions(at []any, o *RepoOptions, def *RepoOptions) {
	if def != nil {
		if o.Sources == nil {
			o.Sources = def.Sources
		}
		if o.IncludePrereleases == nil {
			o.IncludePrereleases = def.IncludePrereleases
		}
		if o.Backfill == nil {
			o.Backfill = def.Backfill
		}
		if o.MaxPages == nil {
			o.MaxPages = def.MaxPages
		}
		if o.Changelog == nil {
			o.Changelog = def.Changelog
		}
		if o.TagPattern == nil {
			o.TagPattern = def.TagPattern
		}
		if o.PrereleasePattern == nil {
			o.PrereleasePattern = def.PrereleasePattern
		}
		// packages are per repo and never inherited: they name what one repo publishes.
	}
	if at == nil {
		return // an env-only repo: everything came from the validated defaults
	}
	if len(o.Sources) == 0 {
		v.err(append(at, "sources"), "must list at least one of releases, tags")
	}
	for i, s := range o.Sources {
		if !slices.Contains(sourceKinds, s) {
			v.err(append(at, "sources", i), "must be releases or tags, got %q", s)
		}
	}
	if *o.Backfill < 0 {
		v.err(append(at, "backfill"), "must be at least 0, got %d", *o.Backfill)
	}
	if *o.MaxPages < 1 {
		v.err(append(at, "max_pages"), "must be at least 1, got %d", *o.MaxPages)
	}
	if cl := *o.Changelog; cl != "auto" && cl != "off" {
		if clean := path.Clean(cl); cl == "" || path.IsAbs(cl) || clean == ".." || strings.HasPrefix(clean, "../") {
			v.err(append(at, "changelog"), "must be auto, off or a path inside the repo, got %q", cl)
		}
	}
	for _, re := range []struct {
		key string
		r   *Regex
	}{{"tag_pattern", o.TagPattern}, {"prerelease_pattern", o.PrereleasePattern}} {
		if re.r != nil && re.r.Re == nil {
			if err := re.r.compile(); err != nil {
				v.err(append(at, re.key), "%v", err)
			}
		}
	}
	for i, pk := range o.Packages {
		if !slices.Contains(ecosystems, pk.Ecosystem) {
			v.err(append(at, "packages", i, "ecosystem"), "must be one of %s, got %q", strings.Join(ecosystems, ", "), pk.Ecosystem)
		}
		if strings.TrimSpace(pk.Name) == "" {
			v.err(append(at, "packages", i, "name"), "is required")
		}
	}
}

// Name is the repo's key: owner/repo, lowercased (§5).
func (r *Repo) Name() string { return strings.ToLower(r.Repo) }

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// resolve makes p relative to dir unless it's absolute.
func resolve(dir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}
