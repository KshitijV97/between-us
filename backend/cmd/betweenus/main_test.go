package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunCommands(t *testing.T) {
	tests := []struct {
		args     []string
		wantCode int
		wantOut  string // substring of stdout+stderr
	}{
		{nil, exitUsage, "Usage:"},
		{[]string{"help"}, exitOK, "Commands:"},
		{[]string{"version"}, exitOK, "betweenus "},
		{[]string{"bogus"}, exitUsage, `unknown command "bogus"`},
		{[]string{"version", "-nope"}, exitUsage, "flag provided but not defined"},
	}
	for _, tt := range tests {
		var out, errOut bytes.Buffer
		code := run(tt.args, &out, &errOut)
		if code != tt.wantCode {
			t.Errorf("%v: exit %d, want %d", tt.args, code, tt.wantCode)
		}
		if all := out.String() + errOut.String(); !strings.Contains(all, tt.wantOut) {
			t.Errorf("%v: output %q lacks %q", tt.args, all, tt.wantOut)
		}
	}
}

func TestVersionJSON(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"version", "-json"}, &out, &out); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	var v map[string]any
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if v["version"] == "" || v["go_version"] == "" {
		t.Errorf("fields missing: %v", v)
	}
}

func TestTitleFromFileName(t *testing.T) {
	for in, want := range map[string]string{
		"WhatsApp Chat with Bob.zip": "Bob",
		"WhatsApp Chat - Bob.zip":    "Bob",
		"export.zip":                 "",
	} {
		if got := titleFromFileName(in); got != want {
			t.Errorf("titleFromFileName(%q) = %q, want %q", in, got, want)
		}
	}
}
