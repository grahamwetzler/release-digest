// Package llmtest is a fake OpenAI-compatible Chat Completions server for
// tests.
package llmtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"

	"release-digest/internal/llm"
)

// Request is one request the fake received.
type Request struct {
	Header      http.Header
	Model       string
	Messages    []llm.Message
	Format      string // response_format.type, or "" for none
	Temperature float64
}

// Reply is one answer: an error status with a message, or (Status 0 or
// 200) a completion with Content.
type Reply struct {
	Status     int
	Message    string // error message for a non-2xx status
	Content    string
	RetryAfter string
}

// Fake answers /chat/completions. Queued replies are used first, in order;
// then Default. A request whose response format isn't in Formats gets the
// 400 a server that doesn't support it would send.
type Fake struct {
	*httptest.Server

	mu       sync.Mutex
	requests []Request
	queue    []Reply
	Formats  []string // accepted response_format types ("" for none); nil accepts all
	Default  func(Request) Reply
}

// New starts a fake that answers every request with content.
func New(content string) *Fake {
	f := &Fake{Default: func(Request) Reply { return Reply{Content: content} }}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Queue adds replies to send before falling back to Default.
func (f *Fake) Queue(rs ...Reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, rs...)
}

// SetDefault replaces the default reply.
func (f *Fake) SetDefault(fn func(Request) Reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Default = fn
}

// Requests returns every request so far.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// URL is the base URL to configure, ending in /v1.
func (f *Fake) URL() string { return f.Server.URL + "/v1" }

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Model          string        `json:"model"`
		Messages       []llm.Message `json:"messages"`
		Temperature    float64       `json:"temperature"`
		ResponseFormat *struct {
			Type string `json:"type"`
		} `json:"response_format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	req := Request{Header: r.Header.Clone(), Model: body.Model, Messages: body.Messages, Temperature: body.Temperature}
	if body.ResponseFormat != nil {
		req.Format = body.ResponseFormat.Type
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	accepted := f.Formats == nil || slices.Contains(f.Formats, req.Format)
	var rep Reply
	if accepted {
		if len(f.queue) > 0 {
			rep, f.queue = f.queue[0], f.queue[1:]
		} else {
			rep = f.Default(req)
		}
	}
	f.mu.Unlock()
	if !accepted {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("response_format type %q is not supported", req.Format))
		return
	}
	if rep.RetryAfter != "" {
		w.Header().Set("Retry-After", rep.RetryAfter)
	}
	if rep.Status != 0 && rep.Status != http.StatusOK {
		writeError(w, rep.Status, rep.Message)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": rep.Content}}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 20},
	})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg}})
}
