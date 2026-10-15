package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"release-digest/internal/feed"
)

// syncBuffer is a bytes.Buffer safe for the run goroutine's logger.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// cleanEnv clears every env var the config and secret loaders read, so the
// developer's own environment can't leak into a test.
func cleanEnv(t *testing.T) {
	for _, k := range []string{"RELEASE_DIGEST_CONFIG", "RELEASE_DIGEST_REPOS", "RELEASE_DIGEST_PUBLIC_BASE_URL",
		"RELEASE_DIGEST_SCHEDULE_INTERVAL", "RELEASE_DIGEST_LLM_BASE_URL", "RELEASE_DIGEST_LLM_MODEL",
		"LLM_API_KEY", "LLM_API_KEY_FILE", "GITHUB_TOKEN", "GITHUB_TOKEN_FILE", "NTFY_TOKEN", "NTFY_TOKEN_FILE"} {
		t.Setenv(k, "") // restores the original value after the test
		os.Unsetenv(k)
	}
}

func writeConfig(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "release-digest.yaml")
	body := "version: 1\npublic_base_url: https://rss.example.com\nlisten: 127.0.0.1:0\nstore_path: rd.db\nlog: {format: text, level: info}\n" + extra
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestRunServesEmptyFeedAndShutsDown(t *testing.T) {
	cleanEnv(t)
	keyFile := filepath.Join(t.TempDir(), "llm_api_key")
	os.WriteFile(keyFile, []byte("sk-or-v1-secretvalue\n"), 0o400)
	t.Setenv("LLM_API_KEY_FILE", keyFile)
	addrs := make(chan string, 1)
	listening = func(a string) { addrs <- a }
	t.Cleanup(func() { listening = nil })

	cfg := writeConfig(t, "")
	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() { done <- run([]string{"run", "--config", cfg}, &stdout, &stderr) }()
	var base string
	select {
	case a := <-addrs:
		base = "http://" + a + "/"
	case code := <-done:
		t.Fatalf("run exited %d before listening:\n%s", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("run never started listening")
	}

	get := func(path string) (int, []byte) {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}
	// The first tick runs at once on a fresh store; wait for its render.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if code, _ := get("feed.xml"); code == http.StatusOK || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	code, rss := get("feed.xml")
	if code != http.StatusOK {
		t.Fatalf("feed.xml: %d\n%s", code, stderr.String())
	}
	if p := feed.Check(rss); len(p) > 0 {
		t.Errorf("RSS: %v", p)
	}
	code, atom := get("feed.atom")
	if p := feed.CheckAtom(atom); code != http.StatusOK || len(p) > 0 {
		t.Errorf("Atom: %d %v", code, p)
	}
	var out bytes.Buffer
	if code := run([]string{"healthcheck", "--url", base + "healthz"}, &out, &out); code != 0 {
		t.Errorf("healthcheck: exit %d: %s", code, out.String())
	}

	// SIGHUP re-reads the secrets; a bad rotation keeps the old ones.
	if err := os.Chmod(keyFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	waitFor(t, &stderr, "secrets not reloaded")

	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d:\n%s", code, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not shut down")
	}
	logs := stderr.String()
	if !strings.Contains(logs, "source=\"file "+keyFile+" (20 bytes)\"") {
		t.Errorf("secret source not logged:\n%s", logs)
	}
	if strings.Contains(logs, "secretvalue") {
		t.Errorf("secret value logged:\n%s", logs)
	}
}

func waitFor(t *testing.T, buf *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("never logged %q:\n%s", want, buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConfigLintReportsSourcesNotValues(t *testing.T) {
	cleanEnv(t)
	t.Setenv("GITHUB_TOKEN", "ghp_secretvalue")
	t.Setenv("RELEASE_DIGEST_REPOS", "astral-sh/uv")
	var out, errb bytes.Buffer
	if code := run([]string{"config", "lint", "--config", writeConfig(t, "")}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"1 repo(s)", "astral-sh/uv (RELEASE_DIGEST_REPOS)", "github token: env GITHUB_TOKEN",
		"llm api key: none", "warning: llm.base_url https://openrouter.ai/api/v1 is not a local address"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "secretvalue") {
		t.Errorf("secret value printed:\n%s", s)
	}

	t.Setenv("GITHUB_TOKEN_FILE", "/nope")
	out.Reset()
	errb.Reset()
	if code := run([]string{"config", "lint", "--config", writeConfig(t, "")}, &out, &errb); code != 1 ||
		!strings.Contains(errb.String(), "GITHUB_TOKEN and GITHUB_TOKEN_FILE are both set") {
		t.Errorf("conflicting sources: exit %d: %s", code, errb.String())
	}
}

func TestLocalURL(t *testing.T) {
	for u, want := range map[string]bool{
		"http://ollama:11434/v1": true, "http://localhost:11434/v1": true, "http://127.0.0.1/v1": true,
		"http://192.168.1.5/v1": true, "https://openrouter.ai/api/v1": false, "http://8.8.8.8/v1": false,
	} {
		if got := localURL(u); got != want {
			t.Errorf("localURL(%s) = %v", u, got)
		}
	}
}
