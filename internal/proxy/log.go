package proxy

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Logger prints one line per request. Colour is used only when writing to
// a terminal and NO_COLOR is unset.
type Logger struct {
	mu    sync.Mutex
	out   io.Writer
	color bool
	quiet bool
}

// NewLogger returns a logger writing to out.
func NewLogger(out io.Writer, quiet bool) *Logger {
	return &Logger{out: out, color: wantsColor(out), quiet: quiet}
}

func wantsColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if os.Getenv("FORCE_COLOR") != "" {
		return true
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

const (
	cReset  = "\x1b[0m"
	cDim    = "\x1b[2m"
	cBold   = "\x1b[1m"
	cGreen  = "\x1b[32m"
	cYellow = "\x1b[33m"
	cRed    = "\x1b[31m"
	cCyan   = "\x1b[36m"
	cMag    = "\x1b[35m"
)

// Paint wraps s in an ANSI colour when colour output is enabled.
func (l *Logger) Paint(code, s string) string {
	if !l.color {
		return s
	}
	return code + s + cReset
}

// Printf writes a raw formatted line.
func (l *Logger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.out, format, args...)
}

type logLine struct {
	action   string
	provider string
	model    string
	method   string
	path     string
	status   int
	elapsed  time.Duration
	note     string
}

func (l *Logger) request(ln logLine) {
	if l.quiet {
		return
	}
	color := map[string]string{
		"REPLAY": cGreen, "RECORD": cMag, "PASS": cCyan, "MISS": cYellow, "ERROR": cRed,
	}[ln.action]
	model := ln.model
	if model == "" {
		model = "-"
	}
	if len(model) > 26 {
		model = model[:25] + "…"
	}
	status := ""
	if ln.status != 0 && ln.status != 200 {
		status = l.Paint(cYellow, fmt.Sprintf(" %d", ln.status))
	}
	line := fmt.Sprintf("  %s %s %-26s %s%s %s %s\n",
		l.Paint(cBold+color, fmt.Sprintf("%-6s", ln.action)),
		l.Paint(cDim, fmt.Sprintf("%-9s", ln.provider)),
		model,
		l.Paint(cDim, ln.method+" "+ln.path),
		status,
		l.Paint(cDim, formatDuration(ln.elapsed)),
		ln.note,
	)
	l.mu.Lock()
	defer l.mu.Unlock()
	io.WriteString(l.out, strings.TrimRight(line, " \n")+"\n")
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
}
