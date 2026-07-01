// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
//
// Redaction uses simple substring replacement for configured patterns and
// bounded Shannon-entropy detection for high-entropy strings when enabled.
// Entropy detection is intentionally conservative (len>=20) and may produce
// false negatives on short secrets or false positives on random-looking data.
//
// Window-size math: a 20-rune window caps maximum entropy at log2(20) ≈ 4.32 bits.
// Set minEntropy in the 3.5–3.8 range for practical API key / base64 detection.
package gateway

import (
	"math"
	"regexp"
	"strings"
)

// redactedRunes is the replacement sequence appended on every high-entropy hit.
// Declared once at package level to avoid a new []rune allocation per redaction.
var redactedRunes = []rune("[REDACTED]")

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

// redactHighEntropy scans s in 20-rune sliding windows. Each window whose
// Shannon entropy meets or exceeds minEntropy is replaced with [REDACTED] and
// the scan advances past the full window. Non-overlapping windows are used
// after a hit to avoid re-examining already-redacted material.
//
// Complexity: O(n·windowSize) time, O(n) space. The inner entropy calculation
// uses a stack-allocated [256]float64 frequency array rather than a map to
// eliminate GC pressure when scanning long strings. Non-ASCII runes in a window
// are skipped in the frequency count; entropy may be underestimated for non-Latin
// text, but this is correct for typical secrets (base64, hex, alphanumeric).
func redactHighEntropy(s string, minEntropy float64) string {
	const windowSize = 20
	runes := []rune(s)
	n := len(runes)
	if n < windowSize {
		return s // strings shorter than the window can never trigger detection
	}

	var out []rune
	pos := 0
	for pos+windowSize <= n {
		win := runes[pos : pos+windowSize]
		if shannonRunes(win) >= minEntropy {
			if out == nil {
				out = make([]rune, 0, n)
				out = append(out, runes[:pos]...)
			}
			out = append(out, redactedRunes...)
			pos += windowSize
			continue
		}
		if out != nil {
			out = append(out, runes[pos])
		}
		pos++
	}

	if out == nil {
		return s // fast path: nothing was redacted
	}
	// append any tail runes that fell outside the last full window
	out = append(out, runes[pos:]...)
	return string(out)
}

// shannonRunes computes the Shannon entropy of the rune window in bits.
// Only runes in the Latin-1 range (0–255) are counted; higher code points are
// skipped. This is deliberate: API keys, tokens, and base64 strings are
// composed entirely of ASCII, so ignoring non-ASCII avoids inflating counts.
// A stack-allocated [256]float64 array is used instead of a map to avoid
// heap allocation on every call.
func shannonRunes(win []rune) float64 {
	var counts [256]float64
	counted := 0
	for _, r := range win {
		if r < 256 {
			counts[r]++
			counted++
		}
	}
	if counted == 0 {
		return 0
	}
	var e float64
	n := float64(counted)
	for _, c := range &counts {
		if c > 0 {
			p := c / n
			e -= p * math.Log2(p)
		}
	}
	return e
}
