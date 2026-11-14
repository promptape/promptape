package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleTape = `{
  "promptape": 1,
  "key": "abc",
  "provider": "anthropic",
  "request": {"method": "POST", "path": "/v1/messages"},
  "interactions": [
    {"recorded_at": "2026-11-14T00:00:00Z", "status": 200, "latency_ms": 1500,
     "body": {"model": "claude-opus-5-5", "usage": {"input_tokens": 1000000, "output_tokens": 0}}}
  ]
}`

func TestReport(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "anthropic"), 0o755)
	os.WriteFile(filepath.Join(dir, "anthropic", "a.json"), []byte(sampleTape), 0o644)
	os.WriteFile(filepath.Join(dir, "anthropic", "broken.json"), []byte("{"), 0o644)

	var out, errb bytes.Buffer
	if code := run([]string{"report", "--dir", dir}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"claude-opus-5-5", "$4.00", "1.5s", "broken.json"} {
		if !strings.Contains(s, want) {
			t.Errorf("report missing %q:\n%s", want, s)
		}
	}

	out.Reset()
	if code := run([]string{"report", "--dir", dir, "--json"}, &out, &errb); code != 0 {
		t.Fatal("json report failed")
	}
	var r reportOut
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.Tapes != 1 || r.USDPerRun != 4 {
		t.Fatalf("json report: %v %+v", err, r)
	}
}

func TestCLIErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if run([]string{"nope"}, &out, &errb) != 2 {
		t.Fatal("unknown command should exit 2")
	}
	if run([]string{"report", "--dir", filepath.Join(t.TempDir(), "missing")}, &out, &errb) != 1 {
		t.Fatal("missing dir should exit 1")
	}
	if run([]string{"serve", "--mode", "bogus"}, &out, &errb) != 2 {
		t.Fatal("bad mode should exit 2")
	}
	out.Reset()
	if run([]string{"version"}, &out, &errb) != 0 || !strings.Contains(out.String(), "promptape") {
		t.Fatal("version failed")
	}
}
