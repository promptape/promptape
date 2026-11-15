package redact

import (
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	secrets := []string{
		"sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz012345",
		"sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
		"sk-AbCdEfGhIjKlMnOpQrStUvWx",
		"AIzaSyA1234567890abcdefghijklmnopqrstuv",
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"AKIAABCDEFGHIJKLMNOP",
		"xoxb-1234567890-abcdefghij",
	}
	for _, s := range secrets {
		out := String("my key is " + s + " ok")
		if strings.Contains(out, s) || !strings.Contains(out, Placeholder) {
			t.Errorf("not redacted: %q -> %q", s, out)
		}
	}
	safe := []string{
		"a risk-assessment-framework-for-teams",
		"use the task-scheduler-component-library",
		"Explain sk-learning briefly",
		"hello world",
	}
	for _, s := range safe {
		if got := String(s); got != s {
			t.Errorf("false positive: %q -> %q", s, got)
		}
	}
}

func TestJSON(t *testing.T) {
	in := []byte(`{"messages":[{"role":"user","content":"key sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz012345 <b>&"}],"n":1.50}`)
	out := string(JSON(in))
	if strings.Contains(out, "AbCdEf") {
		t.Fatalf("secret survived: %s", out)
	}
	if !strings.Contains(out, "<b>&") || !strings.Contains(out, "1.50") {
		t.Fatalf("content altered beyond the secret: %s", out)
	}
	clean := []byte(`{"a": 1}`)
	if string(JSON(clean)) != string(clean) {
		t.Fatal("clean JSON must be returned untouched")
	}
}
