// Package llm is a minimal OpenAI-compatible Chat Completions client
// (docs/summaries.md): structured output with fallbacks for servers that don't
// support it, and retries on 429 and 5xx. Errors never carry the API key.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"release-digest/internal/secret"
)

// Message is one chat message.
type Message struct {
	Role    string `json:"role"` // system | user | assistant
	Content string `json:"content"`
}

// Request is one completion request. Schema is the JSON Schema the reply
// must match; Name names it for servers that want one.
type Request struct {
	Messages []Message
	Name     string
	Schema   json.RawMessage
	// Before, if set, runs before every HTTP request of the call but the
	// first: each retry and each response-format fallback. An error stops
	// the call and is returned as is. The caller checks before calling.
	Before func(context.Context) error
}

// Response is a completion's text and token usage, if reported.
type Response struct {
	Content   string
	TokensIn  int
	TokensOut int
}

// Response formats, from most to least structured (docs/summaries.md). A server that
// rejects one is asked again with the next.
const (
	FormatJSONSchema = iota
	FormatJSONObject
	FormatPrompt
)

// Retries is how many times one call retries a 429 or 5xx.
const Retries = 3

// MaxRetryAfter is the longest Retry-After a call waits out itself.
const MaxRetryAfter = time.Minute

// maxBody bounds a response read.
const maxBody = 4 << 20

// Client talks to one OpenAI-compatible endpoint.
type Client struct {
	BaseURL     string               // e.g. https://openrouter.ai/api/v1
	Model       string               //
	Key         func() secret.String // read at request time, so a SIGHUP rotation applies at once; nil or empty: no Authorization
	Headers     map[string]string    // extra, non-secret request headers
	Temperature float64
	Timeout     time.Duration // per request
	HTTP        *http.Client
	UserAgent   string

	// Sleep is replaceable in tests. It returns false if ctx ended first.
	Sleep func(ctx context.Context, d time.Duration) bool

	format atomic.Int32 // the most structured format the server has accepted
}

// StatusError is a non-2xx answer.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("llm: HTTP %d", e.Status)
	}
	return fmt.Sprintf("llm: HTTP %d: %s", e.Status, e.Message)
}

// Complete sends req. If the server rejects the response format, it retries
// with the next one, and later calls start from the format that worked.
func (c *Client) Complete(ctx context.Context, req Request) (*Response, error) {
	sent := 0
	for f := int(c.format.Load()); ; f++ {
		resp, err := c.complete(ctx, req, f, &sent)
		var se *StatusError
		if f < FormatPrompt && errors.As(err, &se) && formatRejected(se) {
			c.format.CompareAndSwap(int32(f), int32(f+1))
			continue
		}
		return resp, err
	}
}

// Format names the most structured response format the server has
// accepted so far: json_schema, json_object or prompt.
func (c *Client) Format() string {
	return [...]string{"json_schema", "json_object", "prompt"}[c.format.Load()]
}

// formatRejected reports whether an error answer is a rejection of the
// response format rather than of the request as a whole. Servers say so in
// different ways; a 400 or 422 that mentions it is taken as one.
func formatRejected(e *StatusError) bool {
	if e.Status != http.StatusBadRequest && e.Status != http.StatusUnprocessableEntity {
		return false
	}
	m := strings.ToLower(e.Message)
	return strings.Contains(m, "response_format") || strings.Contains(m, "json_schema") ||
		strings.Contains(m, "json_object") || strings.Contains(m, "structured")
}

// complete makes one call in format f, retrying 429 and 5xx answers and
// network errors with jittered exponential backoff.
// sent counts the HTTP requests of the whole call, for req.Before.
func (c *Client) complete(ctx context.Context, req Request, f int, sent *int) (*Response, error) {
	body, err := c.body(req, f)
	if err != nil {
		return nil, err
	}
	for try := 0; ; try++ {
		if *sent > 0 && req.Before != nil {
			if err := req.Before(ctx); err != nil {
				return nil, err
			}
		}
		*sent++
		resp, wait, err := c.post(ctx, body)
		if err == nil || try >= Retries || wait < 0 || wait > MaxRetryAfter || ctx.Err() != nil {
			// A server that asks for a longer wait than MaxRetryAfter fails
			// the attempt, and the summarize backoff (docs/summaries.md) takes over.
			return resp, err
		}
		backoff := time.Second << try
		backoff += rand.N(backoff) // jitter
		backoff = max(backoff, wait)
		if !c.sleep(ctx, backoff) {
			return nil, err
		}
	}
}

// body is the request JSON for format f.
func (c *Client) body(req Request, f int) ([]byte, error) {
	msgs := req.Messages
	if f == FormatPrompt && len(req.Schema) > 0 {
		// Without response_format the schema has to be in the prompt.
		msgs = append(append([]Message(nil), msgs...), Message{Role: "user",
			Content: "Reply with only a JSON object, no prose and no code fence, matching this JSON Schema:\n" + string(req.Schema)})
	}
	v := map[string]any{
		"model":       c.Model,
		"messages":    msgs,
		"temperature": c.Temperature,
	}
	switch {
	case len(req.Schema) == 0 || f == FormatPrompt:
	case f == FormatJSONSchema:
		name := req.Name
		if name == "" {
			name = "response"
		}
		v["response_format"] = map[string]any{"type": "json_schema",
			"json_schema": map[string]any{"name": name, "strict": true, "schema": req.Schema}}
	case f == FormatJSONObject:
		v["response_format"] = map[string]any{"type": "json_object"}
	}
	return json.Marshal(v)
}

// post sends one request. wait is how long to back off before a retry:
// from Retry-After when given, 0 for the default backoff, or negative when
// the error is not worth retrying.
func (c *Client) post(ctx context.Context, body []byte) (*Response, time.Duration, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	u := c.BaseURL + "/chat/completions"
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, -1, err
	}
	for k, v := range c.Headers {
		hr.Header.Set(k, v)
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		hr.Header.Set("User-Agent", c.UserAgent)
	}
	key := c.key()
	if !key.IsZero() {
		hr.Header.Set("Authorization", "Bearer "+key.Reveal())
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(hr)
	if err != nil {
		// The *url.Error names the URL and the cause, never the headers.
		return nil, 0, redact(key, fmt.Errorf("llm: POST %s: %w", u, unwrapURL(err)))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, 0, redact(key, fmt.Errorf("llm: reading the response: %w", err))
	}
	if resp.StatusCode/100 != 2 {
		se := &StatusError{Status: resp.StatusCode, Message: redactString(key, errorMessage(data))}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return nil, retryAfter(resp.Header.Get("Retry-After"), time.Now()), se
		}
		return nil, -1, se
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, -1, fmt.Errorf("llm: decoding the response: %w", err)
	}
	if out.Error != nil {
		// OpenRouter reports some upstream failures inside a 200.
		return nil, 0, &StatusError{Status: resp.StatusCode, Message: redactString(key, out.Error.Message)}
	}
	if len(out.Choices) == 0 {
		return nil, -1, errors.New("llm: the response has no choices")
	}
	return &Response{Content: out.Choices[0].Message.Content,
		TokensIn: out.Usage.PromptTokens, TokensOut: out.Usage.CompletionTokens}, 0, nil
}

func (c *Client) key() secret.String {
	if c.Key == nil {
		return secret.String{}
	}
	return c.Key()
}

// redact removes the request's key from an error, in case a server echoes
// it back. It is the key the request was sent with, which a SIGHUP may have
// replaced meanwhile.
func redact(key secret.String, err error) error {
	if k := key.Reveal(); k != "" && strings.Contains(err.Error(), k) {
		return errors.New(strings.ReplaceAll(err.Error(), k, secret.Redacted))
	}
	return err
}

func redactString(key secret.String, s string) string {
	if k := key.Reveal(); k != "" {
		return strings.ReplaceAll(s, k, secret.Redacted)
	}
	return s
}

func unwrapURL(err error) error {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if inner := ue.Unwrap(); inner != nil {
			return inner
		}
	}
	return err
}

// errorMessage is the error message of an OpenAI-style error body, or the
// start of the body.
func errorMessage(body []byte) string {
	var v struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &v) == nil && len(v.Error) > 0 {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(v.Error, &e) == nil && e.Message != "" {
			return e.Message
		}
		var s string
		if json.Unmarshal(v.Error, &s) == nil && s != "" {
			return s
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = strings.ToValidUTF8(s[:300], "") + "…"
	}
	return s
}

// retryAfter parses a Retry-After header, in seconds or as an HTTP date;
// 0 if absent, unparseable or already past.
func retryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	if n, err := strconv.Atoi(h); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

func (c *Client) sleep(ctx context.Context, d time.Duration) bool {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
