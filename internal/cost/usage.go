package cost

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Provider identifies the API dialect, which changes how usage is counted.
type Provider string

const (
	OpenAI    Provider = "openai"
	Anthropic Provider = "anthropic"
)

// FromBody extracts usage from a complete (non-streaming) JSON response.
// fallbackModel is used when the response does not name the model.
func FromBody(p Provider, body []byte, fallbackModel string) Usage {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return Usage{Model: fallbackModel}
	}
	acc := accumulator{provider: p}
	acc.absorb(obj)
	return acc.result(fallbackModel)
}

// FromSSE extracts usage from a recorded server-sent-event stream. Each
// element is one raw event ("event: ...\ndata: {...}").
func FromSSE(p Provider, events []string, fallbackModel string) Usage {
	acc := accumulator{provider: p}
	for _, ev := range events {
		for _, line := range strings.Split(ev, "\n") {
			data, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			data = strings.TrimSpace(data)
			if data == "" || data == "[DONE]" || data[0] != '{' {
				continue
			}
			var obj map[string]any
			dec := json.NewDecoder(bytes.NewReader([]byte(data)))
			dec.UseNumber()
			if err := dec.Decode(&obj); err != nil {
				continue
			}
			acc.absorb(obj)
		}
	}
	return acc.result(fallbackModel)
}

// accumulator merges usage reported across one or more JSON objects. Later
// non-zero values win, which matches how both providers report running
// totals while streaming.
type accumulator struct {
	provider                       Provider
	model                          string
	in, out, cacheRead, cacheWrite int64
	sawUsage                       bool
}

func (a *accumulator) absorb(obj map[string]any) {
	a.takeModel(obj)
	a.takeUsage(obj["usage"])
	// Anthropic stream: {"type":"message_start","message":{...,"usage":{}}}
	// OpenAI Responses stream: {"type":"response.completed","response":{...}}
	for _, k := range []string{"message", "response"} {
		if inner, ok := obj[k].(map[string]any); ok {
			a.takeModel(inner)
			a.takeUsage(inner["usage"])
		}
	}
}

func (a *accumulator) takeModel(obj map[string]any) {
	if a.model != "" {
		return
	}
	if m, ok := obj["model"].(string); ok && m != "" {
		a.model = m
	}
}

func (a *accumulator) takeUsage(v any) {
	u, ok := v.(map[string]any)
	if !ok {
		return
	}
	a.sawUsage = true
	set := func(dst *int64, n int64) {
		if n > 0 {
			*dst = n
		}
	}
	// Input: Anthropic + OpenAI Responses use input_tokens; Chat Completions
	// and embeddings use prompt_tokens.
	if n := num(u["input_tokens"]); n > 0 {
		set(&a.in, n)
	} else {
		set(&a.in, num(u["prompt_tokens"]))
	}
	if n := num(u["output_tokens"]); n > 0 {
		set(&a.out, n)
	} else {
		set(&a.out, num(u["completion_tokens"]))
	}
	set(&a.cacheRead, num(u["cache_read_input_tokens"]))
	set(&a.cacheWrite, num(u["cache_creation_input_tokens"]))
	for _, k := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if d, ok := u[k].(map[string]any); ok {
			set(&a.cacheRead, num(d["cached_tokens"]))
		}
	}
}

func (a *accumulator) result(fallbackModel string) Usage {
	model := a.model
	if model == "" {
		model = fallbackModel
	}
	u := Usage{Model: model, Input: a.in, Output: a.out, CacheRead: a.cacheRead, CacheWrite: a.cacheWrite}
	// OpenAI counts cached tokens inside the input total; Anthropic does not.
	if a.provider == OpenAI && u.CacheRead > 0 && u.Input >= u.CacheRead {
		u.Input -= u.CacheRead
	}
	return u
}

func num(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			f, ferr := n.Float64()
			if ferr != nil {
				return 0
			}
			return int64(f)
		}
		return i
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}
