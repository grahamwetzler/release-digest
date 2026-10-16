package feed

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// W3CEndpoint is the W3C Feed Validation Service.
const W3CEndpoint = "https://validator.w3.org/feed/check.cgi"

// W3CMessage is one error or warning from the validator.
type W3CMessage struct {
	Level   string `xml:"level"`
	Type    string `xml:"type"`
	Line    int    `xml:"line"`
	Column  int    `xml:"column"`
	Text    string `xml:"text"`
	Element string `xml:"element"`
}

func (m W3CMessage) String() string {
	return fmt.Sprintf("%s %s (line %d, <%s>): %s", m.Level, m.Type, m.Line, m.Element, m.Text)
}

// W3CResult is the validator's verdict.
type W3CResult struct {
	Valid    bool
	Errors   []W3CMessage
	Warnings []W3CMessage
}

// W3CAllowedWarnings are warnings that are expected and accepted. Each needs a reason.
var W3CAllowedWarnings = map[string]string{
	// The feed is posted as raw data, so there is no location for the self link to match.
	"SelfDoesntMatchLocation": "validated from raw data, not its public URL",
	// Atom only. Tags pushed together (a monorepo's modules) share a commit
	// date, so their entries share an atom:updated value; the validator calls
	// this benign.
	"DuplicateUpdated": "tags released together share a timestamp",
}

// Unexpected returns the errors plus any warning not in W3CAllowedWarnings.
func (r *W3CResult) Unexpected() []W3CMessage {
	out := append([]W3CMessage{}, r.Errors...)
	for _, w := range r.Warnings {
		if _, ok := W3CAllowedWarnings[w.Type]; !ok {
			out = append(out, w)
		}
	}
	return out
}

// ValidateW3C posts a feed to the W3C validator and parses its SOAP 1.2 answer.
func ValidateW3C(ctx context.Context, hc *http.Client, userAgent string, data []byte) (*W3CResult, error) {
	form := url.Values{"rawdata": {string(data)}, "manual": {"1"}, "output": {"soap12"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, W3CEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("W3C validator: HTTP %d: %s", resp.StatusCode, firstLine(body))
	}
	return parseSOAP(body)
}

func parseSOAP(body []byte) (*W3CResult, error) {
	var env struct {
		Body struct {
			Response struct {
				Validity string `xml:"validity"`
				Errors   struct {
					List []W3CMessage `xml:"errorlist>error"`
				} `xml:"errors"`
				Warnings struct {
					List []W3CMessage `xml:"warninglist>warning"`
				} `xml:"warnings"`
			} `xml:"feedvalidationresponse"`
			Fault struct {
				Reason string `xml:"Reason>Text"`
			} `xml:"Fault"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("W3C validator: unreadable response: %v: %s", err, firstLine(body))
	}
	if f := env.Body.Fault.Reason; f != "" {
		return nil, fmt.Errorf("W3C validator fault: %s", f)
	}
	r := env.Body.Response
	if r.Validity == "" {
		return nil, fmt.Errorf("W3C validator: no verdict in response: %s", firstLine(body))
	}
	return &W3CResult{Valid: r.Validity == "true", Errors: r.Errors.List, Warnings: r.Warnings.List}, nil
}

func firstLine(b []byte) string {
	b = bytes.TrimSpace(b)
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}
