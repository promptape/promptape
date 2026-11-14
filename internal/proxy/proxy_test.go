package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeUpstream imitates the OpenAI and Anthropic APIs closely enough to
// exercise every proxy path. Each response includes the hit count so tests
// can tell recorded responses apart.
type fakeUpstream struct {
	*httptest.Server
	hits       atomic.Int64
	status     atomic.Int64 // forced status, 0 = normal
	lastAuth   atomic.Value
	lastPath   atomic.Value
	eventDelay time.Duration
}

func newFake(t *testing.T) *fakeUpstream {
	f := &fakeUpstream{}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeUpstream) handle(w http.ResponseWriter, r *http.Request) {
	n := f.hits.Add(1)
	f.lastAuth.Store(r.Header.Get("Authorization") + r.Header.Get("X-Api-Key"))
	f.lastPath.Store(r.URL.Path)
	if r.Header.Get("Accept-Encoding") != "" && r.Header.Get("Accept-Encoding") != "gzip" {
		http.Error(w, "proxy leaked client Accept-Encoding", 500)
		return
	}
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if s := f.status.Load(); s != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(s))
		fmt.Fprintf(w, `{"error":{"message":"forced %d"}}`, s)
		return
	}
	anthropic := strings.HasSuffix(r.URL.Path, "/messages")
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		var events []string
		if anthropic {
			events = []string{
				fmt.Sprintf(`event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_%d","model":"%s","usage":{"input_tokens":1000,"output_tokens":1}}}`, n, req.Model),
				`event: content_block_delta` + "\n" + fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello #%d"}}`, n),
				`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":500}}`,
				`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
			}
		} else {
			events = []string{
				fmt.Sprintf(`data: {"id":"c%d","model":"%s","choices":[{"delta":{"content":"hello #%d"}}],"usage":null}`, n, req.Model, n),
				fmt.Sprintf(`data: {"id":"c%d","model":"%s","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":500}}`, n, req.Model),
				`data: [DONE]`,
			}
		}
		for _, e := range events {
			if f.eventDelay > 0 {
				time.Sleep(f.eventDelay)
			}
			io.WriteString(w, e+"\r\n\r\n") // CRLF on purpose: the proxy must normalise it
			fl.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Set-Cookie", "session=secret")
	if anthropic {
		fmt.Fprintf(w, `{"id":"msg_%d","model":"%s","content":[{"type":"text","text":"hello #%d"}],"usage":{"input_tokens":1000,"output_tokens":500}}`, n, req.Model, n)
		return
	}
	fmt.Fprintf(w, `{"id":"c%d","model":"%s","choices":[{"message":{"content":"hello #%d"}}],"usage":{"prompt_tokens":1000,"completion_tokens":500}}`, n, req.Model, n)
}

func newProxy(t *testing.T, f *fakeUpstream, mode Mode, dir string) *httptest.Server {
	t.Helper()
	srv, err := New(Config{Mode: mode, TapeDir: dir, OpenAIUpstream: f.URL, AnthropicUpstream: f.URL, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

const (
	openaiKey    = "sk-proj-SECRETSECRETSECRETSECRET1234"
	anthropicKey = "sk-ant-api03-SECRETSECRETSECRETSECRET1234"
)

func post(t *testing.T, url, path, body string, anthropic bool) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "br") // must not be forwarded
	if anthropic {
		req.Header.Set("X-Api-Key", anthropicKey)
		req.Header.Set("Anthropic-Version", "2026-10-21")
	} else {
		req.Header.Set("Authorization", "Bearer "+openaiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func assertNoSecretsOnDisk(t *testing.T, dir string) {
	t.Helper()
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		for _, s := range []string{"SECRETSECRET", "session=secret", "Authorization", "X-Api-Key"} {
			if strings.Contains(string(b), s) {
				t.Errorf("%s leaked %q:\n%s", p, s, b)
			}
		}
		return nil
	})
}

const chatBody = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
const msgBody = `{"model":"claude-opus-5-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`

func TestAutoRecordsThenReplaysOpenAI(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	px := newProxy(t, f, ModeAuto, dir)

	r1, b1 := post(t, px.URL, "/v1/chat/completions", chatBody, false)
	if r1.StatusCode != 200 || r1.Header.Get("X-Promptape") != "record" {
		t.Fatalf("first call: %d %s %s", r1.StatusCode, r1.Header.Get("X-Promptape"), b1)
	}
	if f.lastAuth.Load() != "Bearer "+openaiKey {
		t.Fatal("credentials must be forwarded to the real API")
	}
	// A fresh proxy process replays from disk without touching upstream.
	px2 := newProxy(t, f, ModeAuto, dir)
	r2, b2 := post(t, px2.URL, "/v1/chat/completions", "{\n\"messages\":[{\"content\":\"hi\",\"role\":\"user\"}],\"model\":\"gpt-4o\"}", false)
	if r2.Header.Get("X-Promptape") != "replay" || f.hits.Load() != 1 {
		t.Fatalf("second call should replay, header=%s hits=%d", r2.Header.Get("X-Promptape"), f.hits.Load())
	}
	if b1 != b2 {
		t.Fatalf("replayed body differs:\n%s\n%s", b1, b2)
	}
	if r2.Header.Get("Set-Cookie") != "" {
		t.Fatal("replay must not carry upstream cookies")
	}
	assertNoSecretsOnDisk(t, dir)
}

func TestStreamingRecordReplayAnthropic(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	px := newProxy(t, f, ModeRecord, dir)
	stream := strings.Replace(msgBody, `"max_tokens"`, `"stream":true,"max_tokens"`, 1)

	r1, b1 := post(t, px.URL, "/v1/messages", stream, true)
	if r1.StatusCode != 200 || !strings.HasPrefix(r1.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("record: %d %s", r1.StatusCode, r1.Header.Get("Content-Type"))
	}
	if strings.Contains(b1, "\r") {
		t.Fatal("stream should be normalised to LF")
	}
	px2 := newProxy(t, f, ModeReplay, dir)
	r2, b2 := post(t, px2.URL, "/v1/messages", stream, true)
	if r2.Header.Get("X-Promptape") != "replay" {
		t.Fatalf("expected replay, got %s: %s", r2.Header.Get("X-Promptape"), b2)
	}
	if b1 != b2 {
		t.Fatalf("stream replay mismatch:\n%q\n%q", b1, b2)
	}
	if n := strings.Count(b2, "event: "); n != 4 {
		t.Fatalf("expected 4 events, got %d", n)
	}
	assertNoSecretsOnDisk(t, dir)
}

func TestReplayMissUsesProviderErrorShape(t *testing.T) {
	f := newFake(t)
	px := newProxy(t, f, ModeReplay, t.TempDir())
	r, b := post(t, px.URL, "/v1/messages", msgBody, true)
	if r.StatusCode != 404 || f.hits.Load() != 0 {
		t.Fatalf("miss: status=%d hits=%d", r.StatusCode, f.hits.Load())
	}
	var e struct {
		Type  string `json:"type"`
		Error struct{ Type, Message string }
	}
	if json.Unmarshal([]byte(b), &e) != nil || e.Type != "error" || e.Error.Type != "promptape_tape_missing" || !strings.Contains(e.Error.Message, "--mode") {
		t.Fatalf("bad anthropic error body: %s", b)
	}
	r, b = post(t, px.URL, "/v1/chat/completions", chatBody, false)
	var oe struct{ Error struct{ Code string } }
	if r.StatusCode != 404 || json.Unmarshal([]byte(b), &oe) != nil || oe.Error.Code != "promptape_tape_missing" {
		t.Fatalf("bad openai error body: %d %s", r.StatusCode, b)
	}
}

func TestAutoModeNeverRecordsTwice(t *testing.T) {
	f := newFake(t)
	px := newProxy(t, f, ModeAuto, t.TempDir())
	_, a := post(t, px.URL, "/v1/chat/completions", chatBody, false)
	_, b := post(t, px.URL, "/v1/chat/completions", chatBody, false)
	_, c := post(t, px.URL, "/v1/chat/completions", chatBody, false)
	if f.hits.Load() != 1 || a != b || b != c {
		t.Fatalf("a long-running auto proxy must pay for each request once (hits=%d)", f.hits.Load())
	}
}

func TestRecordedSequencesReplayInOrder(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	px := newProxy(t, f, ModeRecord, dir)
	_, a := post(t, px.URL, "/v1/chat/completions", chatBody, false)
	_, b := post(t, px.URL, "/v1/chat/completions", chatBody, false)
	if a == b || f.hits.Load() != 2 {
		t.Fatalf("record mode should capture each call (hits=%d)", f.hits.Load())
	}
	rp := newProxy(t, f, ModeReplay, dir)
	var got []string
	for i := 0; i < 4; i++ {
		_, x := post(t, rp.URL, "/v1/chat/completions", chatBody, false)
		got = append(got, x)
	}
	if got[0] != a || got[1] != b || got[2] != a || got[3] != b {
		t.Fatal("replay should follow recorded order and wrap around for the next test run")
	}
	post(t, rp.URL, "/v1/chat/completions", chatBody, false) // cursor now mid-sequence
	resp, err := http.Post(rp.URL+"/_promptape/rewind", "application/json", nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("rewind: %v", err)
	}
	resp.Body.Close()
	if _, x := post(t, rp.URL, "/v1/chat/completions", chatBody, false); x != a {
		t.Fatal("rewind should restart every sequence")
	}
}

func TestRecordModeOverwrites(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	post(t, newProxy(t, f, ModeRecord, dir).URL, "/v1/chat/completions", chatBody, false)
	_, fresh := post(t, newProxy(t, f, ModeRecord, dir).URL, "/v1/chat/completions", chatBody, false)
	_, replayed := post(t, newProxy(t, f, ModeReplay, dir).URL, "/v1/chat/completions", chatBody, false)
	if replayed != fresh || !strings.Contains(fresh, "hello #2") {
		t.Fatalf("record mode should replace old responses: %s", replayed)
	}
}

func TestTransientErrorsAreNotRecorded(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	f.status.Store(429)
	r, _ := post(t, newProxy(t, f, ModeAuto, dir).URL, "/v1/chat/completions", chatBody, false)
	if r.StatusCode != 429 {
		t.Fatalf("status should pass through, got %d", r.StatusCode)
	}
	f.status.Store(400)
	post(t, newProxy(t, f, ModeAuto, dir).URL, "/v1/messages", msgBody, true)
	r, _ = post(t, newProxy(t, f, ModeReplay, dir).URL, "/v1/chat/completions", chatBody, false)
	if r.StatusCode != 404 {
		t.Fatal("a 429 must not be saved to a tape")
	}
	r, _ = post(t, newProxy(t, f, ModeReplay, dir).URL, "/v1/messages", msgBody, true)
	if r.StatusCode != 400 {
		t.Fatalf("a deterministic 400 should be replayed, got %d", r.StatusCode)
	}
}

func TestPassthroughSavesNothing(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	r, _ := post(t, newProxy(t, f, ModePassthrough, dir).URL, "/v1/chat/completions", chatBody, false)
	if r.Header.Get("X-Promptape") != "passthrough" {
		t.Fatal("expected passthrough header")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("passthrough must not write tapes")
	}
}

func TestRoutingPrefixesAndUpstreamPaths(t *testing.T) {
	f := newFake(t)
	srv, err := New(Config{Mode: ModePassthrough, TapeDir: t.TempDir(), OpenAIUpstream: f.URL + "/v1", AnthropicUpstream: f.URL + "/api"})
	if err != nil {
		t.Fatal(err)
	}
	px := httptest.NewServer(srv)
	defer px.Close()
	post(t, px.URL, "/v1/chat/completions", chatBody, false)
	if f.lastPath.Load() != "/v1/chat/completions" {
		t.Fatalf("duplicate /v1 not collapsed: %v", f.lastPath.Load())
	}
	post(t, px.URL, "/anthropic/v1/messages", msgBody, false)
	if f.lastPath.Load() != "/api/v1/messages" {
		t.Fatalf("anthropic prefix routing: %v", f.lastPath.Load())
	}
	post(t, px.URL, "/openai/v1/messages", chatBody, false)
	if f.lastPath.Load() != "/v1/messages" {
		t.Fatalf("explicit openai prefix should win: %v", f.lastPath.Load())
	}

	// Gemini-style OpenAI-compatible endpoint with a nested base path.
	gem, _ := New(Config{Mode: ModePassthrough, TapeDir: t.TempDir(), OpenAIUpstream: f.URL + "/v1beta/openai/"})
	gpx := httptest.NewServer(gem)
	defer gpx.Close()
	post(t, gpx.URL, "/chat/completions", chatBody, false)
	if f.lastPath.Load() != "/v1beta/openai/chat/completions" {
		t.Fatalf("nested upstream path: %v", f.lastPath.Load())
	}
}

func TestStatsAndAdmin(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	srv, _ := New(Config{Mode: ModeAuto, TapeDir: dir, OpenAIUpstream: f.URL, AnthropicUpstream: f.URL})
	px := httptest.NewServer(srv)
	defer px.Close()
	post(t, px.URL, "/v1/messages", msgBody, true)
	srv2, _ := New(Config{Mode: ModeAuto, TapeDir: dir, OpenAIUpstream: f.URL, AnthropicUpstream: f.URL})
	px2 := httptest.NewServer(srv2)
	defer px2.Close()
	post(t, px2.URL, "/v1/messages", msgBody, true)

	s := srv2.Stats()
	// 1000 in * $4/M + 500 out * $20/M = 0.004 + 0.01
	if s.Replayed != 1 || s.SavedUSD < 0.0139 || s.SavedUSD > 0.0141 || s.SavedTokens != 1500 {
		t.Fatalf("stats: %+v", s)
	}
	resp, err := http.Get(px2.URL + "/_promptape/stats")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stats endpoint: %v", err)
	}
	var snap Snapshot
	json.NewDecoder(resp.Body).Decode(&snap)
	resp.Body.Close()
	if snap.Replayed != 1 {
		t.Fatalf("stats endpoint body: %+v", snap)
	}
	resp, _ = http.Get(px2.URL + "/_promptape/health")
	if resp.StatusCode != 200 {
		t.Fatal("health endpoint failed")
	}
	resp.Body.Close()
}

func TestReplaySpeedReproducesTiming(t *testing.T) {
	f := newFake(t)
	f.eventDelay = 40 * time.Millisecond
	dir := t.TempDir()
	stream := strings.Replace(chatBody, `"model"`, `"stream":true,"model"`, 1)
	post(t, newProxy(t, f, ModeRecord, dir).URL, "/v1/chat/completions", stream, false)

	instant := newProxy(t, f, ModeReplay, dir)
	t0 := time.Now()
	post(t, instant.URL, "/v1/chat/completions", stream, false)
	if d := time.Since(t0); d > 60*time.Millisecond {
		t.Fatalf("instant replay took %v", d)
	}
	srv, _ := New(Config{Mode: ModeReplay, TapeDir: dir, OpenAIUpstream: f.URL, AnthropicUpstream: f.URL, ReplaySpeed: 1})
	timed := httptest.NewServer(srv)
	defer timed.Close()
	t0 = time.Now()
	post(t, timed.URL, "/v1/chat/completions", stream, false)
	if d := time.Since(t0); d < 100*time.Millisecond {
		t.Fatalf("speed=1 replay should take ~120ms, took %v", d)
	}
}

func TestStreamIsRelayedIncrementally(t *testing.T) {
	f := newFake(t)
	f.eventDelay = 150 * time.Millisecond
	px := newProxy(t, f, ModeRecord, t.TempDir())
	stream := strings.Replace(chatBody, `"model"`, `"stream":true,"model"`, 1)
	req, _ := http.NewRequest("POST", px.URL+"/v1/chat/completions", strings.NewReader(stream))
	t0 := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if !strings.HasPrefix(line, "data:") {
		t.Fatalf("unexpected first line %q", line)
	}
	if d := time.Since(t0); d > 400*time.Millisecond {
		t.Fatalf("first event arrived after %v; the proxy is buffering the stream", d)
	}
}

func TestUpstreamDown(t *testing.T) {
	srv, _ := New(Config{Mode: ModeAuto, TapeDir: t.TempDir(), OpenAIUpstream: "http://127.0.0.1:1", AnthropicUpstream: "http://127.0.0.1:1"})
	px := httptest.NewServer(srv)
	defer px.Close()
	r, b := post(t, px.URL, "/v1/chat/completions", chatBody, false)
	if r.StatusCode != http.StatusBadGateway || !strings.Contains(b, "upstream_unreachable") {
		t.Fatalf("got %d %s", r.StatusCode, b)
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := New(Config{Mode: "bogus"}); err == nil {
		t.Fatal("bad mode accepted")
	}
	if _, err := New(Config{OpenAIUpstream: "ftp://x"}); err == nil {
		t.Fatal("bad upstream accepted")
	}
	if _, err := ParseMode(" Replay "); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentTraffic(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	px := newProxy(t, f, ModeAuto, dir)
	done := make(chan string, 40)
	for i := 0; i < 40; i++ {
		go func(i int) {
			body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":"q%d"}]}`, i%8)
			r, _ := post(t, px.URL, "/v1/chat/completions", body, false)
			done <- r.Header.Get("X-Promptape")
		}(i)
	}
	for i := 0; i < 40; i++ {
		if h := <-done; h != "record" && h != "replay" {
			t.Fatalf("unexpected result %q", h)
		}
	}
	// Every distinct prompt now replays offline.
	rp := newProxy(t, f, ModeReplay, dir)
	for i := 0; i < 8; i++ {
		r, _ := post(t, rp.URL, "/v1/chat/completions", fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":"q%d"}]}`, i), false)
		if r.StatusCode != 200 {
			t.Fatalf("prompt %d not recorded", i)
		}
	}
}
