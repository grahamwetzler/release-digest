package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"release-digest/internal/secret"
)

// SendTimeout bounds one POST to ntfy (§3.4).
const SendTimeout = 10 * time.Second

// Ntfy publishes alerts to one ntfy topic.
type Ntfy struct {
	// URL is the server and topic. On ntfy.sh the topic acts as a password,
	// so it is never logged or put in an error.
	URL       func() string
	Token     func() secret.String // read at request time; zero: no Authorization
	HTTP      *http.Client
	Timeout   time.Duration
	UserAgent string
}

// SendError is a failed delivery. Ambiguous means ntfy may have accepted
// the message anyway: the request was sent but no response arrived.
type SendError struct {
	Status    int // 0 if no response
	Ambiguous bool
	Msg       string
}

func (e *SendError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("ntfy answered %d %s: %s", e.Status, http.StatusText(e.Status), e.Msg)
	}
	return "ntfy: " + e.Msg
}

// Send publishes p. Header values that aren't ASCII are RFC 2047 encoded,
// which ntfy decodes.
func (n *Ntfy) Send(ctx context.Context, p Payload) error {
	u := n.URL()
	timeout := n.Timeout
	if timeout <= 0 {
		timeout = SendTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(p.Body))
	if err != nil {
		return &SendError{Msg: "the configured URL is invalid"}
	}
	h := req.Header
	h.Set("Title", header(p.Title))
	h.Set("Priority", strconv.Itoa(p.Priority))
	h.Set("Tags", header(strings.Join(p.Tags, ",")))
	if p.Click != "" {
		h.Set("Click", p.Click)
	}
	var actions []string
	for _, a := range p.Actions {
		actions = append(actions, "view, "+a.Label+", "+a.URL)
	}
	if len(actions) > 0 {
		h.Set("Actions", header(strings.Join(actions, "; ")))
	}
	h.Set("Markdown", "yes")
	h.Set("Content-Type", "text/markdown; charset=utf-8")
	if n.UserAgent != "" {
		h.Set("User-Agent", n.UserAgent)
	}
	tok := secret.String{}
	if n.Token != nil {
		tok = n.Token()
	}
	if !tok.IsZero() {
		h.Set("Authorization", "Bearer "+tok.Reveal())
	}
	hc := n.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		// The error names the URL, which holds the topic: describe it instead.
		msg := "the request failed"
		var ue interface{ Timeout() bool }
		switch {
		case errors.As(err, &ue) && ue.Timeout():
			msg = "no response within " + timeout.String()
		case errors.Is(err, context.Canceled):
			msg = "cancelled"
		}
		// Anything after the connection was made may have been delivered.
		var op *net.OpError
		dialed := !(errors.As(err, &op) && op.Op == "dial")
		var dns *net.DNSError
		if errors.As(err, &dns) {
			dialed = false
		}
		return &SendError{Ambiguous: dialed, Msg: msg}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &SendError{Status: resp.StatusCode, Msg: redact(errorText(body), u, tok.Reveal())}
	}
	return nil
}

// header encodes a header value that isn't plain ASCII.
func header(v string) string {
	for i := 0; i < len(v); i++ {
		if v[i] >= utf8.RuneSelf || v[i] < ' ' {
			return mime.BEncoding.Encode("utf-8", v)
		}
	}
	return v
}

// errorText is ntfy's error message from a JSON error body, or the body.
func errorText(body []byte) string {
	s := strings.TrimSpace(string(bytes.ToValidUTF8(body, []byte("?"))))
	if i := strings.Index(s, `"error":"`); i >= 0 {
		rest := s[i+len(`"error":"`):]
		if j := strings.IndexByte(rest, '"'); j >= 0 {
			return rest[:j]
		}
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// redact removes the URL, its topic and the token from s.
func redact(s, u, tok string) string {
	for _, x := range []string{u, topic(u), tok} {
		if len(x) >= 4 {
			s = strings.ReplaceAll(s, x, "[redacted]")
		}
	}
	return s
}

func topic(u string) string {
	u = strings.TrimRight(u, "/")
	if i := strings.LastIndexByte(u, '/'); i >= 0 {
		return u[i+1:]
	}
	return ""
}
