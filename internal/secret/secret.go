// Package secret loads secrets from exactly one source: a *_FILE path, a
// default Docker secret file, or a plain env var (PLAN.md §4.3). Values are
// held in a String that never prints.
package secret

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
)

// Redacted is what a String prints as.
const Redacted = "[redacted]"

// String holds a secret. Every formatting path prints Redacted; only Reveal
// returns the value, for setting a request header at request time.
type String struct{ v string }

// New wraps v, for values that come from somewhere other than Load, such as tests.
func New(v string) String { return String{v} }

func (s String) Reveal() string               { return s.v }
func (s String) IsZero() bool                 { return s.v == "" }
func (s String) String() string               { return Redacted }
func (s String) GoString() string             { return Redacted }
func (s String) LogValue() slog.Value         { return slog.StringValue(Redacted) }
func (s String) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }
func (s String) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// Spec names one secret and where it may come from.
type Spec struct {
	Label       string // for messages, e.g. "llm api key"
	Env         string // plain env var; the file var is Env + "_FILE"
	DefaultPath string // default Docker secret file, e.g. /run/secrets/llm_api_key
}

var (
	LLMAPIKey   = Spec{Label: "llm api key", Env: "LLM_API_KEY", DefaultPath: "/run/secrets/llm_api_key"}
	GitHubToken = Spec{Label: "github token", Env: "GITHUB_TOKEN", DefaultPath: "/run/secrets/github_token"}
	NtfyToken   = Spec{Label: "ntfy token", Env: "NTFY_TOKEN", DefaultPath: "/run/secrets/ntfy_token"}
)

// Loaded is a resolved secret and a printable description of its source.
type Loaded struct {
	Value  String
	Source string // e.g. "file /run/secrets/llm_api_key (51 bytes)", "env LLM_API_KEY", "none"
}

// Present reports whether any source supplied the secret.
func (l Loaded) Present() bool { return !l.Value.IsZero() }

// Env is the process environment and file system the resolver reads.
// Tests replace it; the zero value is unusable, use OS.
type Env struct {
	LookupEnv func(string) (string, bool)
	ReadFile  func(string) ([]byte, error)
	Stat      func(string) (fs.FileInfo, error)
}

// OS reads the real environment and file system.
var OS = Env{LookupEnv: os.LookupEnv, ReadFile: os.ReadFile, Stat: os.Stat}

// permHint explains the usual cause of an unreadable Docker secret file.
const permHint = "the container runs as uid 65532: chown 65532 the file with mode 0400, or use mode 0444 inside a 0700 directory"

// Load resolves spec. No source at all is not an error: the result is not
// Present. More than one source, an empty value, or an unreadable file is.
func (e Env) Load(spec Spec) (Loaded, error) {
	fileEnv := spec.Env + "_FILE"
	var sources []string
	plain, hasPlain := e.LookupEnv(spec.Env)
	if hasPlain {
		sources = append(sources, spec.Env)
	}
	filePath, hasFile := e.LookupEnv(fileEnv)
	if hasFile {
		sources = append(sources, fileEnv)
	}
	hasDefault := false
	if spec.DefaultPath != "" {
		_, err := e.Stat(spec.DefaultPath)
		switch {
		case err == nil:
			hasDefault = true
		case errors.Is(err, fs.ErrNotExist):
		default:
			// It exists in some form we can't inspect; count it so a stale env var
			// can't silently shadow it, and report it when it's the only source.
			hasDefault = true
		}
		if hasDefault {
			sources = append(sources, spec.DefaultPath)
		}
	}

	switch len(sources) {
	case 0:
		return Loaded{Source: "none"}, nil
	case 1:
	case 2:
		return Loaded{}, fmt.Errorf("%s: %s are both set; remove one", spec.Label, joinAnd(sources))
	default:
		return Loaded{}, fmt.Errorf("%s: %s are all set; remove all but one", spec.Label, joinAnd(sources))
	}

	switch {
	case hasPlain:
		v := strings.TrimSpace(plain)
		if v == "" {
			return Loaded{}, fmt.Errorf("%s: %s is set but empty", spec.Label, spec.Env)
		}
		return Loaded{Value: String{v}, Source: "env " + spec.Env}, nil
	case hasFile:
		if strings.TrimSpace(filePath) == "" {
			return Loaded{}, fmt.Errorf("%s: %s is set but empty", spec.Label, fileEnv)
		}
		return e.loadFile(spec, filePath)
	default:
		return e.loadFile(spec, spec.DefaultPath)
	}
}

func (e Env) loadFile(spec Spec, path string) (Loaded, error) {
	data, err := e.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrPermission):
		return Loaded{}, fmt.Errorf("%s: %s is not readable (%s)", spec.Label, path, permHint)
	case errors.Is(err, fs.ErrNotExist):
		return Loaded{}, fmt.Errorf("%s: %s does not exist", spec.Label, path)
	case err != nil:
		return Loaded{}, fmt.Errorf("%s: reading %s: %w", spec.Label, path, err)
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return Loaded{}, fmt.Errorf("%s: %s is empty", spec.Label, path)
	}
	return Loaded{Value: String{v}, Source: fmt.Sprintf("file %s (%d bytes)", path, len(v))}, nil
}

func joinAnd(s []string) string {
	if len(s) == 2 {
		return s[0] + " and " + s[1]
	}
	return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
}

// Value is a secret that can be replaced while in use (SIGHUP rotation).
// Clients call Get at request time, never caching the result.
type Value struct{ p atomic.Pointer[Loaded] }

func NewValue(l Loaded) *Value {
	v := &Value{}
	v.Set(l)
	return v
}

func (v *Value) Get() Loaded {
	if l := v.p.Load(); l != nil {
		return *l
	}
	return Loaded{Source: "none"}
}

func (v *Value) Set(l Loaded) { v.p.Store(&l) }
