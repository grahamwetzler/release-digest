// Package config loads release-digest.yaml, applies env overrides and the
// RELEASE_DIGEST_REPOS list, and validates the result (PLAN.md §4).
package config

import "time"

// Config is release-digest.yaml (§4.2). Relative paths resolve against the
// file's directory. Secrets are never read from it (§4.3).
type Config struct {
	Version       int         `yaml:"version"`
	StorePath     string      `yaml:"store_path"`
	Listen        string      `yaml:"listen"`
	PublicBaseURL string      `yaml:"public_base_url"`
	Log           Log         `yaml:"log"`
	Feed          Feed        `yaml:"feed"`
	Schedule      Schedule    `yaml:"schedule"`
	GitHub        GitHub      `yaml:"github"`
	LLM           LLM         `yaml:"llm"`
	Notify        Notify      `yaml:"notify"`
	Defaults      RepoOptions `yaml:"defaults"`
	Repos         []*Repo     `yaml:"repos"`

	File string `yaml:"-"`
}

type Log struct {
	Format string `yaml:"format"` // json | text
	Level  string `yaml:"level"`  // debug | info | warn | error
}

type Feed struct {
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
	MaxItems    int    `yaml:"max_items"`
	TTL         int    `yaml:"ttl"` // minutes
}

type Schedule struct {
	Interval        Duration `yaml:"interval"`
	Jitter          Duration `yaml:"jitter"`
	SweepBudget     Duration `yaml:"sweep_budget"`
	PollBudget      Duration `yaml:"poll_budget"`
	SummarizeBudget Duration `yaml:"summarize_budget"`
}

type GitHub struct {
	APIURL         string   `yaml:"api_url"`
	VerifyPerTick  int      `yaml:"verify_per_tick"`
	RequestTimeout Duration `yaml:"request_timeout"`
}

type LLM struct {
	BaseURL       string            `yaml:"base_url"`
	Model         string            `yaml:"model"`
	Headers       map[string]string `yaml:"headers"`
	Temperature   *float64          `yaml:"temperature"`
	Timeout       Duration          `yaml:"timeout"`
	Concurrency   int               `yaml:"concurrency"`
	MaxPerPass    int               `yaml:"max_per_pass"`
	MaxInputChars int               `yaml:"max_input_chars"`
	PublishAfter  Duration          `yaml:"publish_after"`
	PromptFile    string            `yaml:"prompt_file"`
}

type Notify struct {
	Ntfy               *Ntfy    `yaml:"ntfy"` // nil: alerts are off
	MinSeverity        string   `yaml:"min_severity"`
	MaxAge             Duration `yaml:"max_age"`
	IncludeBackfill    bool     `yaml:"include_backfill"`
	IncludePrereleases bool     `yaml:"include_prereleases"`
}

type Ntfy struct {
	URL string `yaml:"url"` // server + topic; may instead come from NTFY_URL_FILE (§4.3)
}

// RepoOptions are the per-repo settings. In defaults every field is set
// after loading; in a repo entry, nil means "inherit the default".
type RepoOptions struct {
	Sources            []string  `yaml:"sources"`
	IncludePrereleases *bool     `yaml:"include_prereleases"`
	Backfill           *int      `yaml:"backfill"`
	MaxPages           *int      `yaml:"max_pages"`
	Changelog          *string   `yaml:"changelog"` // auto | off | a repo-relative path
	TagPattern         *Regex    `yaml:"tag_pattern"`
	PrereleasePattern  *Regex    `yaml:"prerelease_pattern"`
	Packages           []Package `yaml:"packages"`
}

// Package names a published package for advisory matching (§3.4).
type Package struct {
	Ecosystem string `yaml:"ecosystem"`
	Name      string `yaml:"name"`
}

// Repo is one tracked repository with its effective options: after loading,
// every option field is set, from the entry or else from defaults.
type Repo struct {
	Repo        string `yaml:"repo"` // owner/repo as written; Name() is the lowercased key
	RepoOptions `yaml:",inline"`

	FromEnv bool `yaml:"-"` // listed only in RELEASE_DIGEST_REPOS

	invalid bool // a null entry, already reported; skipped by validation
}

// Defaults (§4.2).
const (
	DefaultStorePath       = "release-digest.db"
	DefaultListen          = ":8080"
	DefaultFeedTitle       = "Release Digest"
	DefaultFeedDescription = "Condensed release notes for the projects I depend on."
	DefaultMaxItems        = 200
	DefaultTTL             = 60
	DefaultGitHubAPI       = "https://api.github.com"
	DefaultVerifyPerTick   = 10
	DefaultLLMBaseURL      = "https://openrouter.ai/api/v1"
	DefaultTemperature     = 0.2
	DefaultConcurrency     = 2
	DefaultMaxPerPass      = 20
	DefaultMaxInputChars   = 24000
	DefaultMinSeverity     = "high"
	DefaultBackfill        = 3
	DefaultMaxPages        = 5
	DefaultChangelog       = "auto"
)

var (
	DefaultInterval        = time.Hour
	DefaultJitter          = 5 * time.Minute
	DefaultSweepBudget     = 2 * time.Minute
	DefaultPollBudget      = 7 * time.Minute
	DefaultSummarizeBudget = 7 * time.Minute
	DefaultRequestTimeout  = 20 * time.Second
	DefaultLLMTimeout      = 120 * time.Second
	DefaultPublishAfter    = 6 * time.Hour
	DefaultMaxAge          = 7 * 24 * time.Hour
	DefaultSources         = []string{"releases", "tags"}
)

// Env vars (§4.1).
const (
	EnvConfig           = "RELEASE_DIGEST_CONFIG"
	EnvRepos            = "RELEASE_DIGEST_REPOS"
	EnvPublicBaseURL    = "RELEASE_DIGEST_PUBLIC_BASE_URL"
	EnvScheduleInterval = "RELEASE_DIGEST_SCHEDULE_INTERVAL"
	EnvLLMBaseURL       = "RELEASE_DIGEST_LLM_BASE_URL"
	EnvLLMModel         = "RELEASE_DIGEST_LLM_MODEL"
)
