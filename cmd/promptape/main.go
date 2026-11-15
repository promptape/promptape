// Command promptape records LLM API calls once and replays them forever.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/promptape/promptape/internal/cost"
	"github.com/promptape/promptape/internal/proxy"
	"github.com/promptape/promptape/internal/tape"
)

// version is overridden at build time with -ldflags "-X main.version=...".
// Builds made with `go install ...@vX.Y.Z` fall back to the module version.
var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
}

const usage = `promptape — record your LLM API calls once, replay them forever.

Usage:
  promptape [serve] [flags]   start the proxy (default)
  promptape report [flags]    show what your recorded tapes would cost live
  promptape version           print the version

Point your SDK at the proxy:
  export OPENAI_BASE_URL=http://127.0.0.1:4000/v1
  export ANTHROPIC_BASE_URL=http://127.0.0.1:4000

Run "promptape serve -h" or "promptape report -h" for flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(args, stderr)
	case "report":
		return report(args, stdout, stderr)
	case "version", "--version":
		fmt.Fprintf(stdout, "promptape %s\n", version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "promptape: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

func envFloat(name string, def float64) float64 {
	if v := os.Getenv(name); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func serve(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", env("PROMPTAPE_MODE", "auto"), "auto | record | replay | passthrough  (env PROMPTAPE_MODE)")
	dir := fs.String("dir", env("PROMPTAPE_DIR", "tapes"), "directory for tape files  (env PROMPTAPE_DIR)")
	host := fs.String("host", env("PROMPTAPE_HOST", "127.0.0.1"), "interface to listen on  (env PROMPTAPE_HOST)")
	port := fs.String("port", env("PROMPTAPE_PORT", "4000"), "port to listen on  (env PROMPTAPE_PORT)")
	openaiUp := fs.String("openai-upstream", env("PROMPTAPE_OPENAI_UPSTREAM", "https://api.openai.com"), "real OpenAI-compatible API  (env PROMPTAPE_OPENAI_UPSTREAM)")
	anthropicUp := fs.String("anthropic-upstream", env("PROMPTAPE_ANTHROPIC_UPSTREAM", "https://api.anthropic.com"), "real Anthropic API  (env PROMPTAPE_ANTHROPIC_UPSTREAM)")
	speed := fs.Float64("replay-speed", envFloat("PROMPTAPE_REPLAY_SPEED", 0), "0 = instant replay, 1 = original latency and stream timing")
	ignore := fs.String("ignore-fields", env("PROMPTAPE_IGNORE_FIELDS", ""), "comma-separated request body fields to ignore when matching, e.g. user,metadata")
	prices := fs.String("prices", env("PROMPTAPE_PRICES", ""), "JSON file with extra or corrected model prices")
	quiet := fs.Bool("quiet", os.Getenv("PROMPTAPE_QUIET") != "", "only print the startup banner and summary")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	m, err := proxy.ParseMode(*mode)
	if err != nil {
		fmt.Fprintln(stderr, "promptape:", err)
		return 2
	}
	table, err := cost.Load(*prices)
	if err != nil {
		fmt.Fprintln(stderr, "promptape:", err)
		return 2
	}
	var fields []string
	for _, f := range strings.Split(*ignore, ",") {
		if f = strings.TrimSpace(f); f != "" {
			fields = append(fields, f)
		}
	}
	logger := proxy.NewLogger(stderr, *quiet)
	srv, err := proxy.New(proxy.Config{
		Mode: m, TapeDir: *dir, OpenAIUpstream: *openaiUp, AnthropicUpstream: *anthropicUp,
		ReplaySpeed: *speed, IgnoreFields: fields, Prices: table, Logger: logger, Version: version,
	})
	if err != nil {
		fmt.Fprintln(stderr, "promptape:", err)
		return 2
	}

	addr := net.JoinHostPort(*host, *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(stderr, "promptape: cannot listen on %s: %v\n", addr, err)
		return 1
	}
	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 30 * time.Second}

	shown := *host
	if shown == "0.0.0.0" || shown == "::" || shown == "" {
		shown = "127.0.0.1"
	}
	base := "http://" + net.JoinHostPort(shown, *port)
	p := logger.Paint
	logger.Printf("\n  %s %s  %s\n\n", p("\x1b[1m", "◉ promptape"), p("\x1b[2m", version), modeBadge(logger, m))
	logger.Printf("  %-10s %s\n", "listening", base)
	logger.Printf("  %-10s %s\n\n", "tapes", *dir)
	logger.Printf("  %s\n", p("\x1b[2m", "point your SDKs here:"))
	logger.Printf("    export OPENAI_BASE_URL=%s/v1\n", base)
	logger.Printf("    export ANTHROPIC_BASE_URL=%s\n\n", base)

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "promptape:", err)
			return 1
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpSrv.Shutdown(ctx)
	printSummary(logger, srv.Stats())
	return 0
}

func modeBadge(l *proxy.Logger, m proxy.Mode) string {
	desc := map[proxy.Mode]string{
		proxy.ModeAuto:        "replay if recorded, otherwise record",
		proxy.ModeRecord:      "always call the real API and re-record",
		proxy.ModeReplay:      "never call the real API",
		proxy.ModePassthrough: "forward and measure only",
	}[m]
	return l.Paint("\x1b[1;32m", "mode="+string(m)) + l.Paint("\x1b[2m", " ("+desc+")")
}

func printSummary(l *proxy.Logger, s proxy.Snapshot) {
	if s.Requests == 0 {
		l.Printf("\n  no requests served. bye.\n")
		return
	}
	l.Printf("\n  %s\n", l.Paint("\x1b[1m", "session summary"))
	l.Printf("  %-14s %d  (%d replayed · %d recorded · %d passthrough · %d missed)\n",
		"requests", s.Requests, s.Replayed, s.Recorded, s.Passthrough, s.Misses)
	l.Printf("  %-14s %s\n", "money saved", l.Paint("\x1b[1;32m", cost.FormatUSD(s.SavedUSD)))
	l.Printf("  %-14s %s\n", "money spent", cost.FormatUSD(s.SpentUSD))
	l.Printf("  %-14s %s\n", "time saved", (time.Duration(s.SavedTimeMs) * time.Millisecond).Round(time.Millisecond))
	l.Printf("  %-14s %d\n", "tokens saved", s.SavedTokens)
	if s.Unpriced > 0 {
		l.Printf("  %d responses used models without a known price; add them with --prices.\n", s.Unpriced)
	}
	l.Printf("\n")
}

type modelRow struct {
	Model     string  `json:"model"`
	Tapes     int     `json:"tapes"`
	Responses int     `json:"responses"`
	Tokens    int64   `json:"tokens"`
	USD       float64 `json:"usd"`
	Priced    bool    `json:"priced"`
}

type reportOut struct {
	Dir        string     `json:"dir"`
	Tapes      int        `json:"tapes"`
	Responses  int        `json:"responses"`
	Tokens     int64      `json:"tokens"`
	USDPerRun  float64    `json:"usd_per_run"`
	LatencyMs  int64      `json:"recorded_latency_ms"`
	Models     []modelRow `json:"models"`
	Invalid    []string   `json:"invalid_files,omitempty"`
	PricesAsOf string     `json:"prices_as_of"`
}

func report(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", env("PROMPTAPE_DIR", "tapes"), "directory with tape files")
	prices := fs.String("prices", env("PROMPTAPE_PRICES", ""), "JSON file with extra or corrected model prices")
	asJSON := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	table, err := cost.Load(*prices)
	if err != nil {
		fmt.Fprintln(stderr, "promptape:", err)
		return 2
	}
	if _, err := os.Stat(*dir); err != nil {
		fmt.Fprintf(stderr, "promptape: no tapes at %s (%v)\n", *dir, err)
		return 1
	}
	tapes, _, bad, err := tape.Walk(*dir)
	if err != nil {
		fmt.Fprintln(stderr, "promptape:", err)
		return 1
	}
	out := reportOut{Dir: *dir, Tapes: len(tapes), PricesAsOf: cost.PricesAsOf}
	rows := map[string]*modelRow{}
	for _, t := range tapes {
		seen := map[string]bool{}
		for _, in := range t.Interactions {
			u := usageFor(t, in)
			name := u.Model
			if name == "" {
				name = "(unknown)"
			}
			r := rows[name]
			if r == nil {
				r = &modelRow{Model: name, Priced: true}
				rows[name] = r
			}
			if !seen[name] {
				r.Tapes++
				seen[name] = true
			}
			r.Responses++
			r.Tokens += u.Total()
			usd, ok := table.Estimate(u)
			if !ok && !u.IsZero() {
				r.Priced = false
			}
			r.USD += usd
			out.Responses++
			out.Tokens += u.Total()
			out.USDPerRun += usd
			out.LatencyMs += in.LatencyMs
		}
	}
	for _, r := range rows {
		out.Models = append(out.Models, *r)
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].USD > out.Models[j].USD })
	for p := range bad {
		out.Invalid = append(out.Invalid, p)
	}
	sort.Strings(out.Invalid)

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
		return 0
	}
	fmt.Fprintf(stdout, "\npromptape report · %s\n\n", *dir)
	tw := tabwriter.NewWriter(stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tTAPES\tRESPONSES\tTOKENS\tLIVE COST")
	for _, r := range out.Models {
		c := cost.FormatUSD(r.USD)
		if !r.Priced {
			c = "unpriced"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\n", r.Model, r.Tapes, r.Responses, r.Tokens, c)
	}
	tw.Flush()
	fmt.Fprintf(stdout, "\n%d tapes · %d responses · %d tokens\n", out.Tapes, out.Responses, out.Tokens)
	fmt.Fprintf(stdout, "Every replayed run saves ~%s and ~%s of API wait time.\n",
		cost.FormatUSD(out.USDPerRun), (time.Duration(out.LatencyMs) * time.Millisecond).Round(100*time.Millisecond))
	fmt.Fprintf(stdout, "Prices are estimates as of %s; override with --prices.\n", cost.PricesAsOf)
	for _, p := range out.Invalid {
		fmt.Fprintf(stdout, "warning: skipped %s (%v)\n", p, bad[p])
	}
	fmt.Fprintln(stdout)
	return 0
}

func usageFor(t *tape.Tape, in *tape.Interaction) cost.Usage {
	if in.Usage != nil {
		return *in.Usage
	}
	p := cost.Provider(t.Provider)
	if in.IsStream() {
		return cost.FromSSE(p, in.StreamData(), "")
	}
	return cost.FromBody(p, in.Body, "")
}
