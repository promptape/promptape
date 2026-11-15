package cost

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestLookup(t *testing.T) {
	tb := Default()
	cases := []struct {
		model string
		want  string // expected base model, "" = unpriced
	}{
		{"claude-opus-5-5", "claude-opus-5-5"},
		{"gpt-4o-2024-08-06", "gpt-4o"},
		{"gpt-4o-mini-2024-07-18", "gpt-4o-mini"},
		{"gpt-5-mini", "gpt-5-mini"},
		{"openai/gpt-5", "gpt-5"},
		{"anthropic.claude-sonnet-5-5", "claude-sonnet-5-5"},
		{"claude-sonnet-4-20250514", "claude-sonnet-4"},
		{"o3-mini-2025-01-31", "o3-mini"},
		{"gpt-5.6-sol", ""},
		{"llama3", ""},
		{"", ""},
	}
	for _, c := range cases {
		got, ok := tb.Lookup(c.model)
		if c.want == "" {
			if ok {
				t.Errorf("%q: expected unpriced, got %+v", c.model, got)
			}
			continue
		}
		if !ok || got != builtin[c.want] {
			t.Errorf("%q: got %+v ok=%v, want %s", c.model, got, ok, c.want)
		}
	}
}

func TestEstimate(t *testing.T) {
	tb := Default()
	usd, ok := tb.Estimate(Usage{Model: "claude-opus-5-5", Input: 1_000_000, Output: 1_000_000})
	if !ok || math.Abs(usd-24) > 1e-9 {
		t.Fatalf("opus 5.5 1M in + 1M out: got %v %v, want 24", usd, ok)
	}
	usd, _ = tb.Estimate(Usage{Model: "claude-opus-5-5", CacheRead: 1_000_000, CacheWrite: 1_000_000})
	if math.Abs(usd-(0.20+5)) > 1e-9 {
		t.Fatalf("cache pricing: got %v want 5.20", usd)
	}
	if _, ok := tb.Estimate(Usage{Model: "mystery", Input: 10}); ok {
		t.Fatal("unknown model must be unpriced")
	}
}

func TestLoadOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "prices.json")
	os.WriteFile(p, []byte(`{"My-Model":{"input":1,"output":2},"gpt-4o":{"input":9,"output":9}}`), 0o644)
	tb, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := tb.Lookup("my-model-2026"); !ok || got.Output != 2 {
		t.Fatalf("custom model not loaded: %+v %v", got, ok)
	}
	if got, _ := tb.Lookup("gpt-4o"); got.Input != 9 {
		t.Fatalf("override not applied: %+v", got)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestFromBody(t *testing.T) {
	anth := `{"id":"msg_1","model":"claude-opus-5-5","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":50,"cache_creation_input_tokens":10}}`
	u := FromBody(Anthropic, []byte(anth), "")
	if u != (Usage{Model: "claude-opus-5-5", Input: 100, Output: 20, CacheRead: 50, CacheWrite: 10}) {
		t.Fatalf("anthropic: %+v", u)
	}
	chat := `{"model":"gpt-4o-2024-08-06","usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":40}}}`
	u = FromBody(OpenAI, []byte(chat), "")
	if u != (Usage{Model: "gpt-4o-2024-08-06", Input: 60, Output: 7, CacheRead: 40}) {
		t.Fatalf("openai chat: %+v", u)
	}
	resp := `{"model":"gpt-5","usage":{"input_tokens":30,"output_tokens":5,"input_tokens_details":{"cached_tokens":0}}}`
	u = FromBody(OpenAI, []byte(resp), "")
	if u.Input != 30 || u.Output != 5 {
		t.Fatalf("openai responses: %+v", u)
	}
	emb := `{"data":[],"usage":{"prompt_tokens":8,"total_tokens":8}}`
	u = FromBody(OpenAI, []byte(emb), "text-embedding-3-small")
	if u.Model != "text-embedding-3-small" || u.Input != 8 {
		t.Fatalf("embeddings: %+v", u)
	}
	if u := FromBody(OpenAI, []byte("not json"), "m"); !u.IsZero() || u.Model != "m" {
		t.Fatalf("garbage: %+v", u)
	}
}

func TestFromSSE(t *testing.T) {
	anth := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-5-5\",\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":12}}",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}",
	}
	u := FromSSE(Anthropic, anth, "")
	if u != (Usage{Model: "claude-sonnet-5-5", Input: 25, Output: 12}) {
		t.Fatalf("anthropic stream: %+v", u)
	}
	oai := []string{
		`data: {"model":"gpt-5","choices":[{"delta":{"content":"Hi"}}],"usage":null}`,
		`data: {"model":"gpt-5","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":3}}`,
		`data: [DONE]`,
	}
	u = FromSSE(OpenAI, oai, "")
	if u != (Usage{Model: "gpt-5", Input: 9, Output: 3}) {
		t.Fatalf("openai stream: %+v", u)
	}
	respAPI := []string{
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-5\",\"usage\":null}}",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5\",\"usage\":{\"input_tokens\":11,\"output_tokens\":4}}}",
	}
	u = FromSSE(OpenAI, respAPI, "")
	if u.Input != 11 || u.Output != 4 {
		t.Fatalf("responses stream: %+v", u)
	}
}

func TestFormatUSD(t *testing.T) {
	for in, want := range map[float64]string{0: "$0.00", 0.0012: "$0.0012", 1.5: "$1.50", 250: "$250"} {
		if got := FormatUSD(in); got != want {
			t.Errorf("FormatUSD(%v) = %s, want %s", in, got, want)
		}
	}
}
