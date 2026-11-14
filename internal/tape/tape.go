// Package tape stores recorded API interactions as human-readable JSON
// files that are designed to be committed next to your tests.
package tape

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/promptape/promptape/internal/cost"
)

// FormatVersion is written to every tape so the format can evolve.
const FormatVersion = 1

// Request is the recorded request, minus anything secret.
type Request struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   string            `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
	BodyB64 string            `json:"body_base64,omitempty"`
}

// Event is one server-sent event and when it arrived, in milliseconds after
// the response headers.
type Event struct {
	AtMs int64  `json:"at_ms"`
	Data string `json:"data"`
}

// Interaction is one recorded response.
type Interaction struct {
	RecordedAt time.Time         `json:"recorded_at"`
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers,omitempty"`
	LatencyMs  int64             `json:"latency_ms"`
	Body       json.RawMessage   `json:"body,omitempty"`
	BodyB64    string            `json:"body_base64,omitempty"`
	Stream     []Event           `json:"stream,omitempty"`
	Usage      *cost.Usage       `json:"usage,omitempty"`
}

// IsStream reports whether the interaction was a server-sent-event stream.
func (i *Interaction) IsStream() bool { return len(i.Stream) > 0 }

// StreamData returns the raw events, for usage extraction.
func (i *Interaction) StreamData() []string {
	out := make([]string, len(i.Stream))
	for k, e := range i.Stream {
		out[k] = e.Data
	}
	return out
}

// Tape holds every recorded response for one unique request.
type Tape struct {
	Version      int            `json:"promptape"`
	Key          string         `json:"key"`
	Provider     string         `json:"provider"`
	Request      Request        `json:"request"`
	Interactions []*Interaction `json:"interactions"`
}

// Key computes the match key for a request. The body is canonicalised (keys
// sorted, whitespace removed) so formatting differences between SDKs and
// runs never cause a miss. Top-level fields named in ignore are dropped.
func Key(provider, method, path, query string, body []byte, ignore []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s\n%s\n", provider, method, path, query)
	h.Write(Canonical(body, ignore))
	return hex.EncodeToString(h.Sum(nil))
}

// Canonical returns a stable byte form of a JSON body. Non-JSON bodies are
// returned unchanged.
func Canonical(body []byte, ignore []string) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return body
	}
	if m, ok := v.(map[string]any); ok {
		for _, k := range ignore {
			delete(m, k)
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return body
	}
	return buf.Bytes()
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9.\-]+`)

// Slug builds a readable filename prefix from the request model or path.
func Slug(model, path string) string {
	s := strings.ToLower(model)
	if s == "" {
		s = strings.ToLower(strings.Trim(path, "/"))
		s = strings.ReplaceAll(s, "/", "-")
	}
	s = slugUnsafe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")
	if len(s) > 48 {
		s = s[:48]
	}
	if s == "" {
		s = "request"
	}
	return s
}

// Store loads and saves tapes under a directory.
type Store struct {
	dir string

	mu       sync.Mutex
	tapes    map[string]*entry // by key
	notFound map[string]bool
}

type entry struct {
	tape      *Tape
	path      string
	cursor    int  // next interaction to replay
	recording bool // record mode has reset this tape in this session
}

// NewStore returns a store rooted at dir. The directory is created lazily on
// first write.
func NewStore(dir string) *Store {
	return &Store{dir: dir, tapes: map[string]*entry{}, notFound: map[string]bool{}}
}

// Dir returns the root directory.
func (s *Store) Dir() string { return s.dir }

// PathFor returns where the tape for this request lives.
func (s *Store) PathFor(provider, slug, key string) string {
	return filepath.Join(s.dir, provider, fmt.Sprintf("%s-%s.json", slug, key[:12]))
}

func (s *Store) load(provider, slug, key string) (*entry, error) {
	if e, ok := s.tapes[key]; ok {
		return e, nil
	}
	if s.notFound[key] {
		return nil, nil
	}
	p := s.PathFor(provider, slug, key)
	raw, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		s.notFound[key] = true
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t Tape
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("corrupt tape %s: %w", p, err)
	}
	if t.Key != key {
		return nil, fmt.Errorf("tape %s has key %s, expected %s", p, t.Key, key)
	}
	e := &entry{tape: &t, path: p}
	s.tapes[key] = e
	return e, nil
}

// Next returns the interaction to replay for a request and advances the
// per-request cursor. Identical requests replay the recorded responses in
// order and wrap around, so every run of a test suite sees the same
// sequence even against a long-running proxy. ok is false when nothing has
// been recorded for the request.
func (s *Store) Next(provider, slug, key string) (in *Interaction, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.load(provider, slug, key)
	if err != nil || e == nil || len(e.tape.Interactions) == 0 {
		return nil, false, err
	}
	in = e.tape.Interactions[e.cursor%len(e.tape.Interactions)]
	e.cursor++
	return in, true, nil
}

// Rewind resets every replay cursor, e.g. at the start of a test session.
func (s *Store) Rewind() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.tapes {
		e.cursor = 0
	}
}

// Append records an interaction. In record mode (reset=true) the first
// write in a session replaces whatever the tape held before, so re-recording
// never mixes stale and fresh responses.
func (s *Store) Append(provider, slug, key string, req Request, in *Interaction, reset bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.load(provider, slug, key)
	if err != nil {
		return "", err
	}
	if e == nil {
		e = &entry{
			tape: &Tape{Version: FormatVersion, Key: key, Provider: provider, Request: req},
			path: s.PathFor(provider, slug, key),
		}
		s.tapes[key] = e
		delete(s.notFound, key)
	}
	if reset && !e.recording {
		e.tape.Interactions = nil
		e.tape.Request = req
		e.cursor = 0
	}
	e.recording = reset || e.recording
	e.tape.Interactions = append(e.tape.Interactions, in)
	return e.path, writeAtomic(e.path, e.tape)
}

func writeAtomic(path string, t *Tape) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(t); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".promptape-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Walk loads every tape under dir, sorted by path. Files that fail to parse
// are reported through bad.
func Walk(dir string) (tapes []*Tape, paths []string, bad map[string]error, err error) {
	bad = map[string]error{}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".json") {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			bad[p] = rerr
			return nil
		}
		var t Tape
		if jerr := json.Unmarshal(raw, &t); jerr != nil || t.Version == 0 {
			if jerr == nil {
				jerr = errors.New("not a promptape tape")
			}
			bad[p] = jerr
			return nil
		}
		tapes = append(tapes, &t)
		paths = append(paths, p)
		return nil
	})
	idx := make([]int, len(paths))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return paths[idx[a]] < paths[idx[b]] })
	st, sp := make([]*Tape, len(idx)), make([]string, len(idx))
	for i, j := range idx {
		st[i], sp[i] = tapes[j], paths[j]
	}
	return st, sp, bad, err
}
