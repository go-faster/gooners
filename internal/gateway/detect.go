// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
//
// Redaction uses simple substring replacement for configured patterns and
// bounded Shannon-entropy detection for high-entropy strings when enabled.
// Entropy detection is intentionally conservative (len>=20) and may produce
// false negatives on short secrets or false positives on random-looking data.
package gateway

import (
	"math"
	"regexp"
	"strings"
)

// Redactor replaces sensitive substrings in text output.
type Redactor struct {
	patterns   []*regexp.Regexp
	minEntropy float64
	builtins   []*regexp.Regexp
}

// NewRedactor compiles custom patterns and seeds built-in secret detectors.
func NewRedactor(patterns []string, minEntropy float64) (*Redactor, error) {
	r := &Redactor{minEntropy: minEntropy}
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, err
		}
		r.patterns = append(r.patterns, re)
	}
	// Built-ins keep the label and replace the secret value.
	r.builtins = append(r.builtins,
		regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|bearer|authorization)\s*[:=]\s*\S+`),
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	)
	return r, nil
}

// Redact replaces matches; label-preserving for built-ins.
func (r *Redactor) Redact(s string) string {
	for _, re := range r.patterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	for _, re := range r.builtins {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			// keep prefix up to : or = or space before the value
			if idx := strings.LastIndexAny(m, "=: "); idx >= 0 && idx < len(m)-1 {
				return m[:idx+1] + "[REDACTED]"
			}
			return "[REDACTED]"
		})
	}
	if r.minEntropy > 0 {
		s = redactHighEntropy(s, r.minEntropy)
	}
	return s
}

func redactHighEntropy(s string, minEntropy float64) string {
	// very simple: scan for >=20 char substrings with entropy >= min
	for i := 0; i+20 <= len(s); i++ {
		sub := s[i : i+20]
		if shannon(sub) >= minEntropy {
			s = s[:i] + "[REDACTED]" + s[i+20:]
			i += 8 // rough skip
		}
	}
	return s
}

func shannon(s string) float64 {
	if s == "" {
		return 0
	}
	counts := map[rune]int{}
	for _, r := range s {
		counts[r]++
	}
	var e float64
	n := float64(len(s))
	for _, c := range counts {
		p := float64(c) / n
		e -= p * math.Log2(p)
	}
	return e
}
