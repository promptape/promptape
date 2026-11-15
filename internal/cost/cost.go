// Package cost turns token usage into an estimated dollar figure.
//
// Prices change often. The built-in table is a best-effort snapshot (see
// PricesAsOf) and can be overridden or extended with a JSON file passed to
// --prices. Models that are not in the table are reported as "unpriced"
// rather than guessed.
package cost

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// PricesAsOf is the date the built-in table was last reviewed.
const PricesAsOf = "2026-11"

// Price is the cost of one million tokens, in US dollars.
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// Usage is token usage normalised across providers. Input excludes tokens
// served from the prompt cache, which are counted in CacheRead.
type Usage struct {
	Model      string `json:"model,omitempty"`
	Input      int64  `json:"input_tokens"`
	Output     int64  `json:"output_tokens"`
	CacheRead  int64  `json:"cache_read_tokens,omitempty"`
	CacheWrite int64  `json:"cache_write_tokens,omitempty"`
}

// Total returns all tokens in the usage record.
func (u Usage) Total() int64 { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

// IsZero reports whether no tokens were recorded.
func (u Usage) IsZero() bool { return u.Total() == 0 }

func anthropic(in, out, cacheRead float64) Price {
	if cacheRead == 0 {
		cacheRead = in * 0.1
	}
	return Price{Input: in, Output: out, CacheRead: cacheRead, CacheWrite: in * 1.25}
}

func openai(in, cached, out float64) Price {
	return Price{Input: in, Output: out, CacheRead: cached}
}

// builtin holds list prices per 1M tokens for standard (non-batch) usage.
var builtin = map[string]Price{
	// Anthropic
	"claude-fable-5-2":  anthropic(8, 40, 0.20),
	"claude-mythos-5-2": anthropic(8, 40, 0.20),
	"claude-fable-5-1":  anthropic(10, 50, 0),
	"claude-mythos-5-1": anthropic(10, 50, 0),
	"claude-fable-5":    anthropic(10, 50, 0),
	"claude-opus-5-5":   anthropic(4, 20, 0.20),
	"claude-opus-5":     anthropic(5, 25, 0),
	"claude-opus-4-8":   anthropic(5, 25, 0),
	"claude-opus-4-7":   anthropic(5, 25, 0),
	"claude-opus-4-6":   anthropic(5, 25, 0),
	"claude-opus-4-5":   anthropic(5, 25, 0),
	"claude-opus-4-1":   anthropic(15, 75, 0),
	"claude-sonnet-5-5": anthropic(2, 10, 0.10),
	"claude-sonnet-5":   anthropic(2, 10, 0),
	"claude-sonnet-4-6": anthropic(3, 15, 0),
	"claude-sonnet-4-5": anthropic(3, 15, 0),
	"claude-sonnet-4":   anthropic(3, 15, 0),
	"claude-haiku-5-5":  anthropic(0.10, 0.50, 0),
	"claude-haiku-4-5":  anthropic(1, 5, 0),
	"claude-3-5-haiku":  anthropic(0.80, 4, 0),

	// OpenAI
	"gpt-5.5":                openai(5, 0.50, 30),
	"gpt-5.4":                openai(2.50, 0.25, 15),
	"gpt-5":                  openai(1.25, 0.125, 10),
	"gpt-5-mini":             openai(0.25, 0.025, 2),
	"gpt-5-nano":             openai(0.05, 0.005, 0.40),
	"gpt-4.1":                openai(2, 0.50, 8),
	"gpt-4.1-mini":           openai(0.40, 0.10, 1.60),
	"gpt-4.1-nano":           openai(0.10, 0.025, 0.40),
	"gpt-4o":                 openai(2.50, 1.25, 10),
	"gpt-4o-mini":            openai(0.15, 0.075, 0.60),
	"o3":                     openai(2, 0.50, 8),
	"o3-mini":                openai(1.10, 0.55, 4.40),
	"o4-mini":                openai(1.10, 0.275, 4.40),
	"text-embedding-3-small": openai(0.02, 0, 0),
	"text-embedding-3-large": openai(0.13, 0, 0),
}

// Table resolves model names to prices.
type Table struct {
	prices map[string]Price
}

// Default returns the built-in price table.
func Default() *Table {
	t := &Table{prices: make(map[string]Price, len(builtin))}
	for k, v := range builtin {
		t.prices[k] = v
	}
	return t
}

// Load returns the built-in table merged with overrides from a JSON file of
// the form {"model-id": {"input": 1.0, "output": 2.0, ...}}. An empty path
// returns the defaults.
func Load(path string) (*Table, error) {
	t := Default()
	if path == "" {
		return t, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading prices: %w", err)
	}
	var extra map[string]Price
	if err := json.Unmarshal(raw, &extra); err != nil {
		return nil, fmt.Errorf("parsing prices %s: %w", path, err)
	}
	for k, v := range extra {
		t.prices[strings.ToLower(k)] = v
	}
	return t, nil
}

// normalize strips routing prefixes such as "openai/" (OpenRouter) or
// "anthropic." (Bedrock) so the base model id can be looked up.
func normalize(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	m = strings.TrimPrefix(m, "anthropic.")
	return m
}

// Lookup finds the price for a model. Dated snapshots resolve to their base
// model ("gpt-4o-2024-08-06" -> "gpt-4o"), always preferring the longest
// matching id so "gpt-5-mini" never resolves to "gpt-5".
func (t *Table) Lookup(model string) (Price, bool) {
	m := normalize(model)
	if m == "" {
		return Price{}, false
	}
	if p, ok := t.prices[m]; ok {
		return p, true
	}
	best := ""
	for k := range t.prices {
		if strings.HasPrefix(m, k+"-") && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return Price{}, false
	}
	return t.prices[best], true
}

// Estimate returns the dollar cost of a usage record and whether the model
// was priced.
func (t *Table) Estimate(u Usage) (float64, bool) {
	p, ok := t.Lookup(u.Model)
	if !ok {
		return 0, false
	}
	cacheRead := p.CacheRead
	if cacheRead == 0 {
		cacheRead = p.Input
	}
	cacheWrite := p.CacheWrite
	if cacheWrite == 0 {
		cacheWrite = p.Input
	}
	usd := float64(u.Input)*p.Input +
		float64(u.Output)*p.Output +
		float64(u.CacheRead)*cacheRead +
		float64(u.CacheWrite)*cacheWrite
	return usd / 1_000_000, true
}

// Models returns the ids in the table, sorted.
func (t *Table) Models() []string {
	out := make([]string, 0, len(t.prices))
	for k := range t.prices {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// FormatUSD renders small dollar amounts readably.
func FormatUSD(v float64) string {
	switch {
	case v == 0:
		return "$0.00"
	case v < 0.0001:
		return "<$0.0001"
	case v < 0.01:
		return fmt.Sprintf("$%.4f", v)
	case v < 100:
		return fmt.Sprintf("$%.2f", v)
	default:
		return fmt.Sprintf("$%.0f", v)
	}
}
