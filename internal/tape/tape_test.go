package tape

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestKeyIsFormattingInsensitive(t *testing.T) {
	a := Key("openai", "POST", "/v1/chat/completions", "", []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`), nil)
	b := Key("openai", "POST", "/v1/chat/completions", "", []byte("{\n  \"messages\": [{\"content\": \"hi\", \"role\": \"user\"}],\n  \"model\": \"gpt-5\"\n}"), nil)
	if a != b {
		t.Fatal("key should ignore whitespace and key order")
	}
	c := Key("openai", "POST", "/v1/chat/completions", "", []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`), nil)
	if a == c {
		t.Fatal("different prompts must have different keys")
	}
	d := Key("anthropic", "POST", "/v1/chat/completions", "", []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`), nil)
	if a == d {
		t.Fatal("provider must be part of the key")
	}
}

func TestKeyIgnoreFields(t *testing.T) {
	a := Key("openai", "POST", "/p", "", []byte(`{"model":"m","user":"alice"}`), []string{"user"})
	b := Key("openai", "POST", "/p", "", []byte(`{"model":"m","user":"bob"}`), []string{"user"})
	if a != b {
		t.Fatal("ignored fields must not affect the key")
	}
}

func TestKeyKeepsNumberPrecision(t *testing.T) {
	a := Key("openai", "POST", "/p", "", []byte(`{"temperature":0.1}`), nil)
	b := Key("openai", "POST", "/p", "", []byte(`{"temperature":0.10000000000000001}`), nil)
	if a == b {
		t.Fatal("numbers should be compared as written, not as rounded floats")
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"claude-opus-5-5", "/v1/messages"}: "claude-opus-5-5",
		{"openai/GPT-5:beta", ""}:           "openai-gpt-5-beta",
		{"", "/v1/embeddings"}:              "v1-embeddings",
		{"", ""}:                            "request",
		{"../../etc/passwd", ""}:            "etc-passwd",
	} {
		if got := Slug(in[0], in[1]); got != want {
			t.Errorf("Slug(%q,%q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestStoreSequenceAndPersistence(t *testing.T) {
	dir := t.TempDir()
	key := Key("openai", "POST", "/v1/x", "", []byte(`{}`), nil)
	s := NewStore(dir)
	if _, ok, err := s.Next("openai", "m", key); ok || err != nil {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	req := Request{Method: "POST", Path: "/v1/x"}
	for _, body := range []string{`{"n":1}`, `{"n":2}`} {
		if _, err := s.Append("openai", "m", key, req, &Interaction{Status: 200, Body: json.RawMessage(body)}, false); err != nil {
			t.Fatal(err)
		}
	}
	// New session: replay in order and wrap around.
	s2 := NewStore(dir)
	var got []string
	next := func() {
		in, ok, err := s2.Next("openai", "m", key)
		if !ok || err != nil {
			t.Fatalf("replay: ok=%v err=%v", ok, err)
		}
		var buf bytes.Buffer
		json.Compact(&buf, in.Body)
		got = append(got, buf.String())
	}
	next()
	next()
	next()
	s2.Rewind()
	next()
	want := []string{`{"n":1}`, `{"n":2}`, `{"n":1}`, `{"n":1}`}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected order %v, want %v", got, want)
		}
	}

	// Record mode resets the tape once per session.
	s3 := NewStore(dir)
	s3.Append("openai", "m", key, req, &Interaction{Status: 200, Body: json.RawMessage(`{"n":3}`)}, true)
	s3.Append("openai", "m", key, req, &Interaction{Status: 200, Body: json.RawMessage(`{"n":4}`)}, true)
	tapes, _, bad, err := Walk(dir)
	if err != nil || len(bad) != 0 || len(tapes) != 1 {
		t.Fatalf("walk: %v %v %d", err, bad, len(tapes))
	}
	if n := len(tapes[0].Interactions); n != 2 {
		t.Fatalf("record mode should keep only this session's 2 responses, got %d", n)
	}
}

func TestStoreRejectsCorruptTape(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	key := Key("openai", "GET", "/v1/models", "", nil, nil)
	p := s.PathFor("openai", "m", key)
	os.MkdirAll(dir+"/openai", 0o755)
	os.WriteFile(p, []byte("{not json"), 0o644)
	if _, _, err := s.Next("openai", "m", key); err == nil {
		t.Fatal("expected an error for a corrupt tape")
	}
}
