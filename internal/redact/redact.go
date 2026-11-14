// Package redact removes credentials from anything Promptape writes to disk.
//
// Tapes are meant to be committed to git, so they must never contain API
// keys. Request headers are filtered with an allowlist (see the proxy
// package); this package additionally scrubs secret-looking strings that
// appear inside request and response bodies.
package redact

import (
	"bytes"
	"encoding/json"
	"regexp"
)

// Placeholder is what a detected secret is replaced with.
const Placeholder = "[REDACTED]"

// patterns match well-known credential formats. They are intentionally
// specific so normal prompt text is never rewritten.
var patterns = []*regexp.Regexp{
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}`),                       // Anthropic
	regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_\-]{20,}`), // OpenAI
	regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}`),                           // Google
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}`),                       // GitHub
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{40,}`),                     // GitHub fine-grained
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9\-]{10,}`),                    // Slack
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}`),                                 // AWS access key id
	regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}`),                              // Hugging Face
}

// String returns s with every detected secret replaced by Placeholder.
func String(s string) string {
	for _, p := range patterns {
		if p.MatchString(s) {
			s = p.ReplaceAllString(s, Placeholder)
		}
	}
	return s
}

// Contains reports whether s contains a detected secret.
func Contains(s string) bool {
	for _, p := range patterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}

// Value walks a decoded JSON value and redacts every string in place,
// returning the (possibly new) value.
func Value(v any) any {
	switch t := v.(type) {
	case string:
		return String(t)
	case map[string]any:
		for k, e := range t {
			t[k] = Value(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = Value(e)
		}
		return t
	default:
		return v
	}
}

// JSON redacts a raw JSON document. If raw is not valid JSON it is treated
// as text. The result is always safe to write to disk.
func JSON(raw []byte) []byte {
	if !Contains(string(raw)) {
		return raw
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return []byte(String(string(raw)))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(Value(v)); err != nil {
		return []byte(String(string(raw)))
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}
