package synth

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestWhatsAppDeterministicAndWellFormed(t *testing.T) {
	cfg := DefaultConfig(500)
	var a, b bytes.Buffer
	if err := WriteWhatsApp(&a, cfg); err != nil {
		t.Fatal(err)
	}
	if err := WriteWhatsApp(&b, cfg); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("same seed produced different output")
	}
	lines := strings.Split(strings.TrimSpace(a.String()), "\n")
	if !strings.Contains(lines[0], "end-to-end encrypted") {
		t.Errorf("first line is not the encryption notice: %q", lines[0])
	}
	// Every non-continuation line starts with a dd/mm/yyyy header.
	for _, l := range lines {
		if len(l) > 3 && l[2] == '/' && !strings.Contains(l, " - ") {
			t.Fatalf("malformed header line %q", l)
		}
	}
}

func TestTelegramIsValidJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTelegram(&buf, DefaultConfig(300)); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Type     string           `json:"type"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if doc.Type != "personal_chat" || len(doc.Messages) != 300 {
		t.Errorf("type=%q messages=%d", doc.Type, len(doc.Messages))
	}
}
