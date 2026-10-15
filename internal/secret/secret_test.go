package secret

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
)

// fakeEnv builds an Env from a map of env vars and an in-memory file system
// rooted at "/". A file mode of 0 makes the file unreadable.
func fakeEnv(env map[string]string, files fstest.MapFS) Env {
	strip := func(p string) string { return strings.TrimPrefix(p, "/") }
	return Env{
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		Stat:      func(p string) (fs.FileInfo, error) { return fs.Stat(files, strip(p)) },
		ReadFile: func(p string) ([]byte, error) {
			if f, ok := files[strip(p)]; ok && f.Mode == 0o000|fs.ModeIrregular {
				return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrPermission}
			}
			return fs.ReadFile(files, strip(p))
		},
	}
}

func TestLoadSources(t *testing.T) {
	keyFile := &fstest.MapFile{Data: []byte("sk-or-v1-abc\n")}
	tests := []struct {
		name    string
		env     map[string]string
		files   fstest.MapFS
		want    string // revealed value
		source  string
		wantErr string
	}{
		{name: "none", source: "none"},
		{name: "plain env", env: map[string]string{"LLM_API_KEY": "k1"}, want: "k1", source: "env LLM_API_KEY"},
		{name: "file env", env: map[string]string{"LLM_API_KEY_FILE": "/keys/llm"}, files: fstest.MapFS{"keys/llm": keyFile},
			want: "sk-or-v1-abc", source: "file /keys/llm (12 bytes)"},
		{name: "default docker secret", files: fstest.MapFS{"run/secrets/llm_api_key": keyFile},
			want: "sk-or-v1-abc", source: "file /run/secrets/llm_api_key (12 bytes)"},
		{name: "env and default file", env: map[string]string{"LLM_API_KEY": "k1"}, files: fstest.MapFS{"run/secrets/llm_api_key": keyFile},
			wantErr: "LLM_API_KEY and /run/secrets/llm_api_key are both set; remove one"},
		{name: "all three", env: map[string]string{"LLM_API_KEY": "k1", "LLM_API_KEY_FILE": "/x"}, files: fstest.MapFS{"run/secrets/llm_api_key": keyFile},
			wantErr: "LLM_API_KEY, LLM_API_KEY_FILE and /run/secrets/llm_api_key are all set"},
		{name: "empty env counts as present", env: map[string]string{"LLM_API_KEY": ""}, wantErr: "LLM_API_KEY is set but empty"},
		{name: "empty env still conflicts", env: map[string]string{"LLM_API_KEY": ""}, files: fstest.MapFS{"run/secrets/llm_api_key": keyFile},
			wantErr: "are both set"},
		{name: "missing _FILE", env: map[string]string{"LLM_API_KEY_FILE": "/nope"}, wantErr: "/nope does not exist"},
		{name: "blank file", files: fstest.MapFS{"run/secrets/llm_api_key": {Data: []byte(" \n")}}, wantErr: "/run/secrets/llm_api_key is empty"},
		{name: "unreadable file", files: fstest.MapFS{"run/secrets/llm_api_key": {Mode: fs.ModeIrregular}}, wantErr: "uid 65532"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, err := fakeEnv(tt.env, tt.files).Load(LLMAPIKey)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if l.Value.Reveal() != tt.want || l.Source != tt.source {
				t.Errorf("got (%q, %q), want (%q, %q)", l.Value.Reveal(), l.Source, tt.want, tt.source)
			}
		})
	}
}

func TestStringNeverPrints(t *testing.T) {
	s := String{"hunter2"}
	var logged strings.Builder
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("x", "key", s)
	j, _ := json.Marshal(struct{ K String }{s})
	for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s %q", s, s, s, s, s), string(j), logged.String()} {
		if strings.Contains(out, "hunter2") {
			t.Errorf("secret leaked: %s", out)
		}
	}
}

func TestValueRotation(t *testing.T) {
	v := NewValue(Loaded{Value: String{"old"}, Source: "env X"})
	v.Set(Loaded{Value: String{"new"}, Source: "env X"})
	if got := v.Get().Value.Reveal(); got != "new" {
		t.Errorf("Get = %q", got)
	}
	if (&Value{}).Get().Present() {
		t.Error("zero Value is present")
	}
}
