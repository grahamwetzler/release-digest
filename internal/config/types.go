package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The scalar types below capture the raw YAML value and its position at
// decode time, and are compiled during validation. Decoding never fails on a
// bad value, so validation can report every problem in a file at once.

// scalar is the shared decode step: accept any scalar node, remember where it was.
func scalar(n *yaml.Node, raw *string, line *int) error {
	if n.Kind != yaml.ScalarNode {
		return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: expected a string, got a %s", n.Line, kindName(n.Kind))}}
	}
	*raw, *line = n.Value, n.Line
	return nil
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.MappingNode:
		return "mapping"
	case yaml.SequenceNode:
		return "list"
	case yaml.AliasNode:
		return "alias"
	default:
		return "scalar"
	}
}

// Duration is a Go duration such as "20s" or "1h", or a whole number of
// days such as "7d".
type Duration struct {
	Raw  string
	Line int
	D    time.Duration
}

func (d *Duration) UnmarshalYAML(n *yaml.Node) error { return scalar(n, &d.Raw, &d.Line) }

// Dur returns a Duration already compiled to v, for defaults.
func Dur(v time.Duration) Duration { return Duration{Raw: v.String(), D: v} }

// ParseDuration parses a Go duration or a whole number of days ("30d").
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q (use Go syntax such as 30s, 1h, or whole days such as 7d)", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use Go syntax such as 30s, 1h, or whole days such as 7d)", s)
	}
	return v, nil
}

// compile parses Raw. Zero is allowed only when allowZero is set.
func (d *Duration) compile(allowZero bool) error {
	v, err := ParseDuration(d.Raw)
	if err != nil {
		return err
	}
	if v < 0 || (v == 0 && !allowZero) {
		return fmt.Errorf("duration %q must be positive", d.Raw)
	}
	d.D = v
	return nil
}

// Regex is a Go regular expression.
type Regex struct {
	Raw  string
	Line int
	Re   *regexp.Regexp
}

func (r *Regex) UnmarshalYAML(n *yaml.Node) error { return scalar(n, &r.Raw, &r.Line) }

func (r *Regex) compile() error {
	re, err := regexp.Compile(r.Raw)
	if err != nil {
		return fmt.Errorf("invalid regex %q: %v", r.Raw, err)
	}
	r.Re = re
	return nil
}
