// Package proxy implements the Promptape HTTP server: an OpenAI- and
// Anthropic-compatible endpoint that records real responses to tapes and
// replays them.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptape/promptape/internal/cost"
	"github.com/promptape/promptape/internal/redact"
	"github.com/promptape/promptape/internal/tape"
)

// Mode controls whether the proxy talks to the real API.
type Mode string

const (
	// ModeAuto replays when a tape exists and records when it does not.
	ModeAuto Mode = "auto"
	// ModeRecord always calls the real API and overwrites tapes.
	ModeRecord Mode = "record"
	// ModeReplay never calls the real API; unrecorded requests fail.
	ModeReplay Mode = "replay"
	// ModePassthrough forwards everything and records nothing.
	ModePassthrough Mode = "passthrough"
)

// ParseMode validates a mode string.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case ModeAuto, ModeRecord, ModeReplay, ModePassthrough:
		return m, nil
	}
	return "", fmt.Errorf("unknown mode %q (want auto, record, replay or passthrough)", s)
}

// MaxBodyBytes bounds request and recorded response sizes.
const MaxBodyBytes = 64 << 20

// Config configures a Server.
type Config struct {
	Mode              Mode
	TapeDir           string
	OpenAIUpstream    string
	AnthropicUpstream string
	// ReplaySpeed scales recorded timing on replay: 0 replays instantly,
	// 1 reproduces the original latency and streaming cadence.
	ReplaySpeed float64
	// IgnoreFields are top-level JSON body fields left out of request
	// matching, e.g. "user" or "metadata".
	IgnoreFields []string
	Prices       *cost.Table
	Logger       *Logger
	Version      string
	// Client is used for upstream calls; nil means a sensible default.
	Client *http.Client
}

// Server is the Promptape HTTP handler.
type Server struct {
	cfg       Config
	store     *tape.Store
	stats     *Stats
	client    *http.Client
	upstreams map[cost.Provider]*url.URL
}

// New validates cfg and returns a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Mode == "" {
		cfg.Mode = ModeAuto
	}
	if _, err := ParseMode(string(cfg.Mode)); err != nil {
		return nil, err
	}
	if cfg.TapeDir == "" {
		cfg.TapeDir = "tapes"
	}
	if cfg.OpenAIUpstream == "" {
		cfg.OpenAIUpstream = "https://api.openai.com"
	}
	if cfg.AnthropicUpstream == "" {
		cfg.AnthropicUpstream = "https://api.anthropic.com"
	}
	if cfg.Prices == nil {
		cfg.Prices = cost.Default()
	}
	if cfg.Logger == nil {
		cfg.Logger = NewLogger(io.Discard, true)
	}
	if cfg.ReplaySpeed < 0 {
		return nil, errors.New("replay speed must be >= 0")
	}
	ups := map[cost.Provider]*url.URL{}
	for p, raw := range map[cost.Provider]string{cost.OpenAI: cfg.OpenAIUpstream, cost.Anthropic: cfg.AnthropicUpstream} {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("invalid %s upstream %q", p, raw)
		}
		ups[p] = u
	}
	client := cfg.Client
	if client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ResponseHeaderTimeout = 10 * time.Minute
		client = &http.Client{Transport: tr}
	}
	return &Server{
		cfg:       cfg,
		store:     tape.NewStore(cfg.TapeDir),
		stats:     newStats(),
		client:    client,
		upstreams: ups,
	}, nil
}

// Stats returns the session totals.
func (s *Server) Stats() Snapshot { return s.stats.Snapshot() }

// Config returns the effective configuration.
func (s *Server) Config() Config { return s.cfg }

// route decides which provider a request is for and returns the path to
// forward. Explicit /openai and /anthropic prefixes win; otherwise the
// request shape decides.
func route(r *http.Request) (cost.Provider, string) {
	p := r.URL.Path
	for _, pr := range []cost.Provider{cost.OpenAI, cost.Anthropic} {
		prefix := "/" + string(pr)
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			rest := strings.TrimPrefix(p, prefix)
			if rest == "" {
				rest = "/"
			}
			return pr, rest
		}
	}
	if strings.HasPrefix(p, "/v1/messages") || strings.HasPrefix(p, "/v1/complete") ||
		r.Header.Get("anthropic-version") != "" {
		return cost.Anthropic, p
	}
	return cost.OpenAI, p
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_promptape/") {
		s.admin(w, r)
		return
	}
	start := time.Now()
	provider, path := route(r)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		writeError(w, provider, http.StatusRequestEntityTooLarge, "request_too_large", "promptape: could not read request body: "+err.Error())
		return
	}
	model := modelOf(body)
	query := r.URL.Query().Encode()
	key := tape.Key(string(provider), r.Method, path, query, body, s.cfg.IgnoreFields)
	slug := tape.Slug(model, path)
	call := &call{
		provider: provider, path: path, query: query, body: body, model: model,
		key: key, slug: slug, start: start, r: r,
	}
	s.stats.update(func(st *Snapshot) { st.Requests++ })

	switch s.cfg.Mode {
	case ModeReplay, ModeAuto:
		in, ok, err := s.store.Next(string(provider), slug, key)
		if err != nil {
			s.fail(w, call, http.StatusInternalServerError, "tape_error", err.Error())
			return
		}
		if ok {
			s.replay(w, call, in)
			return
		}
		if s.cfg.Mode == ModeReplay {
			s.miss(w, call)
			return
		}
		s.forward(w, call, true, false)
	case ModeRecord:
		s.forward(w, call, true, true)
	case ModePassthrough:
		s.forward(w, call, false, false)
	}
}

type call struct {
	provider cost.Provider
	path     string
	query    string
	body     []byte
	model    string
	key      string
	slug     string
	start    time.Time
	r        *http.Request
}

func modelOf(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	return m.Model
}

// ---- replay -------------------------------------------------------------

func (s *Server) replay(w http.ResponseWriter, c *call, in *tape.Interaction) {
	h := w.Header()
	for k, v := range in.Headers {
		h.Set(k, v)
	}
	h.Set("X-Promptape", "replay")
	h.Set("X-Promptape-Key", c.key[:12])
	ctx := c.r.Context()
	speed := s.cfg.ReplaySpeed

	if in.IsStream() {
		if h.Get("Content-Type") == "" {
			h.Set("Content-Type", "text/event-stream")
		}
		h.Set("Cache-Control", "no-cache")
		w.WriteHeader(in.Status)
		fl, _ := w.(http.Flusher)
		for _, ev := range in.Stream {
			if speed > 0 {
				due := c.start.Add(time.Duration(float64(ev.AtMs)*speed) * time.Millisecond)
				if !sleepUntil(ctx, due) {
					return
				}
			}
			if _, err := io.WriteString(w, ev.Data+"\n\n"); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	} else {
		payload, err := interactionBody(in)
		if err != nil {
			s.fail(w, c, http.StatusInternalServerError, "tape_error", err.Error())
			return
		}
		if speed > 0 && !sleepUntil(ctx, c.start.Add(time.Duration(float64(in.LatencyMs)*speed)*time.Millisecond)) {
			return
		}
		h.Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(in.Status)
		w.Write(payload)
	}

	usage := s.usageOf(c, in)
	usd, priced := s.cfg.Prices.Estimate(usage)
	s.stats.update(func(st *Snapshot) {
		st.Replayed++
		st.SavedUSD += usd
		st.SavedTokens += usage.Total()
		st.SavedTimeMs += in.LatencyMs
		if !priced && !usage.IsZero() {
			st.Unpriced++
		}
	})
	note := s.cfg.Logger.Paint(cGreen, "saved "+priceLabel(usd, priced, usage))
	s.cfg.Logger.request(logLine{"REPLAY", string(c.provider), firstNonEmpty(usage.Model, c.model), c.r.Method, c.path, in.Status, time.Since(c.start), note})
}

func (s *Server) usageOf(c *call, in *tape.Interaction) cost.Usage {
	if in.Usage != nil {
		return *in.Usage
	}
	if in.IsStream() {
		return cost.FromSSE(c.provider, in.StreamData(), c.model)
	}
	b, _ := interactionBody(in)
	return cost.FromBody(c.provider, b, c.model)
}

func interactionBody(in *tape.Interaction) ([]byte, error) {
	if in.BodyB64 != "" {
		return base64.StdEncoding.DecodeString(in.BodyB64)
	}
	if len(in.Body) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, in.Body); err != nil {
		return nil, fmt.Errorf("tape body is not valid JSON: %w", err)
	}
	return buf.Bytes(), nil
}

func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Server) miss(w http.ResponseWriter, c *call) {
	s.stats.update(func(st *Snapshot) { st.Misses++ })
	where := s.store.PathFor(string(c.provider), c.slug, c.key)
	msg := fmt.Sprintf("promptape: no recording for %s %s (model %q). Expected tape: %s. "+
		"Run promptape with --mode auto or --mode record to capture it.", c.r.Method, c.path, c.model, where)
	w.Header().Set("X-Promptape", "miss")
	writeError(w, c.provider, http.StatusNotFound, "promptape_tape_missing", msg)
	s.cfg.Logger.request(logLine{"MISS", string(c.provider), c.model, c.r.Method, c.path, http.StatusNotFound, time.Since(c.start),
		s.cfg.Logger.Paint(cYellow, "no tape → "+relPath(where))})
}

func (s *Server) fail(w http.ResponseWriter, c *call, status int, kind, msg string) {
	writeError(w, c.provider, status, kind, "promptape: "+msg)
	s.cfg.Logger.request(logLine{"ERROR", string(c.provider), c.model, c.r.Method, c.path, status, time.Since(c.start), s.cfg.Logger.Paint(cRed, msg)})
}

// writeError responds in the error shape the provider's SDK expects so the
// message surfaces cleanly in the caller's exception.
func writeError(w http.ResponseWriter, p cost.Provider, status int, kind, msg string) {
	var payload any
	if p == cost.Anthropic {
		payload = map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": msg}}
	} else {
		payload = map[string]any{"error": map[string]any{"message": msg, "type": kind, "code": kind}}
	}
	b, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	w.WriteHeader(status)
	w.Write(b)
}

// ---- forward / record ---------------------------------------------------

var hopHeaders = map[string]bool{
	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
}

// savedRequestHeaders is the allowlist of request headers written to tapes.
// Credentials are never in it.
var savedRequestHeaders = []string{"Content-Type", "Anthropic-Version", "Anthropic-Beta", "Openai-Beta"}

func (s *Server) upstreamURL(c *call) string {
	base := s.upstreams[c.provider]
	bp := strings.TrimRight(base.Path, "/")
	p := c.path
	if strings.HasSuffix(bp, "/v1") && (p == "/v1" || strings.HasPrefix(p, "/v1/")) {
		p = strings.TrimPrefix(p, "/v1")
	}
	u := *base
	u.Path = bp + p
	u.RawPath = ""
	u.RawQuery = c.r.URL.RawQuery
	return u.String()
}

func (s *Server) forward(w http.ResponseWriter, c *call, record, reset bool) {
	req, err := http.NewRequestWithContext(c.r.Context(), c.r.Method, s.upstreamURL(c), bytes.NewReader(c.body))
	if err != nil {
		s.fail(w, c, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	for k, vv := range c.r.Header {
		ck := http.CanonicalHeaderKey(k)
		if hopHeaders[ck] || ck == "Host" || ck == "Content-Length" || ck == "Accept-Encoding" {
			continue
		}
		req.Header[ck] = append([]string(nil), vv...)
	}
	req.ContentLength = int64(len(c.body))

	resp, err := s.client.Do(req)
	if err != nil {
		s.stats.update(func(st *Snapshot) { st.UpstreamErrors++ })
		s.fail(w, c, http.StatusBadGateway, "upstream_unreachable", "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	action := "PASS"
	if record {
		action = "RECORD"
	}
	h := w.Header()
	for k, vv := range resp.Header {
		ck := http.CanonicalHeaderKey(k)
		if hopHeaders[ck] || ck == "Content-Length" {
			continue
		}
		h[ck] = append([]string(nil), vv...)
	}
	if record {
		h.Set("X-Promptape", "record")
	} else {
		h.Set("X-Promptape", "passthrough")
	}
	h.Set("X-Promptape-Key", c.key[:12])

	in := &tape.Interaction{
		RecordedAt: time.Now().UTC().Truncate(time.Second),
		Status:     resp.StatusCode,
		Headers:    map[string]string{},
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		in.Headers["Content-Type"] = ct
	}
	isStream := strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")

	var usage cost.Usage
	var copyErr error
	if isStream {
		w.WriteHeader(resp.StatusCode)
		in.Stream, copyErr = copyStream(w, resp.Body, c.start)
		usage = cost.FromSSE(c.provider, in.StreamData(), c.model)
	} else {
		var rb []byte
		rb, copyErr = io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
		if copyErr == nil && len(rb) > MaxBodyBytes {
			copyErr = errors.New("response exceeds size limit")
		}
		h.Set("Content-Length", fmt.Sprint(len(rb)))
		w.WriteHeader(resp.StatusCode)
		w.Write(rb)
		usage = cost.FromBody(c.provider, rb, c.model)
		if json.Valid(rb) {
			in.Body = redact.JSON(rb)
		} else if len(rb) > 0 {
			in.BodyB64 = base64.StdEncoding.EncodeToString(rb)
		}
	}
	in.LatencyMs = time.Since(c.start).Milliseconds()
	if !usage.IsZero() {
		u := usage
		in.Usage = &u
	}
	usd, priced := s.cfg.Prices.Estimate(usage)

	saved := ""
	transient := resp.StatusCode >= 500 || resp.StatusCode == 429 || resp.StatusCode == 408
	switch {
	case !record:
	case copyErr != nil:
		saved = s.cfg.Logger.Paint(cYellow, "not saved: "+copyErr.Error())
	case transient:
		saved = s.cfg.Logger.Paint(cYellow, fmt.Sprintf("not saved: transient %d", resp.StatusCode))
	default:
		path, err := s.store.Append(string(c.provider), c.slug, c.key, s.savedRequest(c), in, reset)
		if err != nil {
			msg := "save failed: " + err.Error()
			if errors.Is(err, fs.ErrPermission) {
				msg += " (tapes directory not writable; with Docker add --user \"$(id -u):$(id -g)\")"
			}
			saved = s.cfg.Logger.Paint(cRed, msg)
		} else {
			saved = s.cfg.Logger.Paint(cDim, "→ "+relPath(path))
		}
	}

	s.stats.update(func(st *Snapshot) {
		if record {
			st.Recorded++
		} else {
			st.Passthrough++
		}
		st.SpentUSD += usd
		if !priced && !usage.IsZero() {
			st.Unpriced++
		}
	})
	note := s.cfg.Logger.Paint(cMag, "cost "+priceLabel(usd, priced, usage))
	if saved != "" {
		note += " " + saved
	}
	s.cfg.Logger.request(logLine{action, string(c.provider), firstNonEmpty(usage.Model, c.model), c.r.Method, c.path, resp.StatusCode, time.Since(c.start), note})
}

func (s *Server) savedRequest(c *call) tape.Request {
	req := tape.Request{Method: c.r.Method, Path: c.path, Query: redact.String(c.query), Headers: map[string]string{}}
	for _, k := range savedRequestHeaders {
		if v := c.r.Header.Get(k); v != "" {
			req.Headers[k] = redact.String(v)
		}
	}
	if len(req.Headers) == 0 {
		req.Headers = nil
	}
	if json.Valid(c.body) {
		req.Body = redact.JSON(c.body)
	} else if len(c.body) > 0 {
		req.BodyB64 = base64.StdEncoding.EncodeToString(c.body)
	}
	return req
}

// copyStream relays server-sent events to the client as they arrive and
// returns them with their arrival offsets (from the start of the request,
// so time-to-first-token is preserved). Events are kept verbatim so the
// replay is byte-for-byte what the SDK originally parsed.
func copyStream(w http.ResponseWriter, body io.Reader, t0 time.Time) ([]tape.Event, error) {
	fl, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(body, 64<<10)
	var events []tape.Event
	var lines []string
	clientGone := false
	emit := func() {
		if len(lines) == 0 {
			return
		}
		data := strings.Join(lines, "\n")
		lines = lines[:0]
		events = append(events, tape.Event{AtMs: time.Since(t0).Milliseconds(), Data: redact.String(data)})
		if clientGone {
			return
		}
		if _, err := io.WriteString(w, data+"\n\n"); err != nil {
			clientGone = true
			return
		}
		if fl != nil {
			fl.Flush()
		}
	}
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				emit()
			} else {
				lines = append(lines, line)
			}
		}
		if err == io.EOF {
			emit()
			if clientGone {
				return events, errors.New("client disconnected")
			}
			return events, nil
		}
		if err != nil {
			return events, err
		}
	}
}

func priceLabel(usd float64, priced bool, u cost.Usage) string {
	switch {
	case u.IsZero():
		return "$0 (no usage reported)"
	case !priced:
		return fmt.Sprintf("%d tok (unpriced model)", u.Total())
	default:
		return cost.FormatUSD(usd)
	}
}

// relPath shortens p relative to the working directory for log output.
func relPath(p string) string {
	wd, err := os.Getwd()
	if err != nil {
		return p
	}
	r, err := filepath.Rel(wd, p)
	if err != nil || strings.HasPrefix(r, "..") {
		return p
	}
	return r
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---- admin --------------------------------------------------------------

func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	switch r.URL.Path {
	case "/_promptape/health":
		enc.Encode(map[string]any{"status": "ok", "version": s.cfg.Version, "mode": s.cfg.Mode, "tapes": s.cfg.TapeDir})
	case "/_promptape/stats":
		enc.Encode(s.stats.Snapshot())
	case "/_promptape/rewind":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			enc.Encode(map[string]any{"error": "use POST"})
			return
		}
		s.store.Rewind()
		enc.Encode(map[string]any{"status": "rewound"})
	default:
		w.WriteHeader(http.StatusNotFound)
		enc.Encode(map[string]any{"error": "unknown promptape endpoint", "endpoints": []string{"/_promptape/health", "/_promptape/stats", "/_promptape/rewind"}})
	}
}
