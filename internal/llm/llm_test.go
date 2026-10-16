package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"release-digest/internal/llm"
	"release-digest/internal/llm/llmtest"
	"release-digest/internal/secret"
)

const key = "sk-or-v1-0123456789abcdef"

func client(f *llmtest.Fake, slept *[]time.Duration) *llm.Client {
	return &llm.Client{
		BaseURL:     f.URL(),
		Model:       "test/model",
		Key:         func() secret.String { return secret.New(key) },
		Headers:     map[string]string{"X-Title": "Release Digest"},
		Temperature: 0.2,
		Timeout:     5 * time.Second,
		Sleep: func(_ context.Context, d time.Duration) bool {
			if slept != nil {
				*slept = append(*slept, d)
			}
			return true
		},
	}
}

var req = llm.Request{
	Messages: []llm.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "notes"}},
	Name:     "summary",
	Schema:   json.RawMessage(`{"type":"object"}`),
}

func TestCompleteSendsTheRequest(t *testing.T) {
	f := llmtest.New(`{"ok":true}`)
	defer f.Close()
	resp, err := client(f, nil).Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != `{"ok":true}` || resp.TokensIn != 100 || resp.TokensOut != 20 {
		t.Errorf("resp = %+v", resp)
	}
	r := f.Requests()[0]
	if r.Model != "test/model" || r.Temperature != 0.2 || r.Format != "json_schema" || len(r.Messages) != 2 {
		t.Errorf("request = %+v", r)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+key {
		t.Errorf("Authorization = %q", got)
	}
	if got := r.Header.Get("X-Title"); got != "Release Digest" {
		t.Errorf("X-Title = %q", got)
	}
}

func TestNoKeyNoAuthorization(t *testing.T) {
	f := llmtest.New(`{}`)
	defer f.Close()
	c := client(f, nil)
	c.Key = func() secret.String { return secret.String{} }
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if h := f.Requests()[0].Header; h.Get("Authorization") != "" {
		t.Errorf("Authorization sent without a key: %q", h.Get("Authorization"))
	}
}

func TestFormatFallbacks(t *testing.T) {
	for _, tc := range []struct {
		accept []string
		want   []string // formats tried by the first call
		prompt bool     // the schema went into the prompt
	}{
		{[]string{"json_schema"}, []string{"json_schema"}, false},
		{[]string{"json_object", ""}, []string{"json_schema", "json_object"}, false},
		{[]string{""}, []string{"json_schema", "json_object", ""}, true},
	} {
		f := llmtest.New(`{}`)
		f.Formats = tc.accept
		c := client(f, nil)
		if _, err := c.Complete(context.Background(), req); err != nil {
			t.Fatalf("%v: %v", tc.accept, err)
		}
		var got []string
		for _, r := range f.Requests() {
			got = append(got, r.Format)
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%v: tried %q, want %q", tc.accept, got, tc.want)
		}
		last := f.Requests()[len(got)-1]
		inPrompt := strings.Contains(last.Messages[len(last.Messages)-1].Content, `{"type":"object"}`)
		if inPrompt != tc.prompt {
			t.Errorf("%v: schema in prompt = %v", tc.accept, inPrompt)
		}
		// The next call starts from the format that worked.
		if _, err := c.Complete(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if n := len(f.Requests()); n != len(got)+1 {
			t.Errorf("%v: second call made %d requests", tc.accept, n-len(got))
		}
		f.Close()
	}
}

func TestOtherBadRequestsAreNotFormatFallbacks(t *testing.T) {
	f := llmtest.New(`{}`)
	defer f.Close()
	f.Queue(llmtest.Reply{Status: 400, Message: "model not found"})
	_, err := client(f, nil).Complete(context.Background(), req)
	var se *llm.StatusError
	if !errors.As(err, &se) || se.Status != 400 {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.Requests()); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

func TestRetries(t *testing.T) {
	f := llmtest.New(`{}`)
	defer f.Close()
	f.Queue(llmtest.Reply{Status: 429, Message: "slow down", RetryAfter: "7"}, llmtest.Reply{Status: 502, Message: "upstream"})
	var slept []time.Duration
	resp, err := client(f, &slept).Complete(context.Background(), req)
	if err != nil || resp.Content != "{}" {
		t.Fatalf("resp, err = %+v, %v", resp, err)
	}
	if len(slept) != 2 || slept[0] < 7*time.Second {
		t.Errorf("slept %v; want two backoffs, the first honoring Retry-After", slept)
	}

	// After Retries retries, the error is returned.
	f.Queue(llmtest.Reply{Status: 500}, llmtest.Reply{Status: 500}, llmtest.Reply{Status: 500}, llmtest.Reply{Status: 500})
	before := len(f.Requests())
	_, err = client(f, nil).Complete(context.Background(), req)
	var se *llm.StatusError
	if !errors.As(err, &se) || se.Status != 500 {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.Requests()) - before; n != llm.Retries+1 {
		t.Errorf("%d requests, want %d", n, llm.Retries+1)
	}
}

func TestTimeout(t *testing.T) {
	block := make(chan struct{})
	f := llmtest.New(`{}`)
	defer f.Close()
	defer close(block)
	f.SetDefault(func(llmtest.Request) llmtest.Reply { <-block; return llmtest.Reply{Content: "{}"} })
	c := client(f, nil)
	c.Timeout = 50 * time.Millisecond
	start := time.Now()
	if _, err := c.Complete(context.Background(), req); err == nil {
		t.Fatal("no error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
}

func TestErrorsNeverCarryTheKey(t *testing.T) {
	f := llmtest.New(`{}`)
	defer f.Close()
	f.Queue(llmtest.Reply{Status: http.StatusUnauthorized, Message: "invalid key " + key})
	_, err := client(f, nil).Complete(context.Background(), req)
	if err == nil {
		t.Fatal("no error")
	}
	for _, s := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		if strings.Contains(s, key) {
			t.Errorf("error leaks the key: %s", s)
		}
	}
}
