package clankcli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"
)

func TestWriteJSONOut(t *testing.T) {
	t.Parallel()

	// Capture stdout.
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	input := map[string]string{"status": "ok", "id": "abc123"}
	if err := writeJSONOut(input); err != nil {
		os.Stdout = old
		t.Fatalf("writeJSONOut: %v", err)
	}
	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	io.Copy(&buf, r)

	var got map[string]string
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\nraw: %s", err, buf.String())
	}
	if got["status"] != "ok" || got["id"] != "abc123" {
		t.Errorf("unexpected output: %v", got)
	}
}

func TestSessionsCmd_HasSubcommands(t *testing.T) {
	t.Parallel()

	cmd := sessionsCmd()

	expected := map[string]bool{
		"list":     false,
		"get":      false,
		"messages": false,
		"send":     false,
		"new":      false,
		"abort":    false,
	}

	for _, sub := range cmd.Commands() {
		if _, ok := expected[sub.Name()]; ok {
			expected[sub.Name()] = true
		}
	}

	for name, found := range expected {
		if !found {
			t.Errorf("missing subcommand %q", name)
		}
	}
}

func TestSessionsCmd_AliasS(t *testing.T) {
	t.Parallel()

	cmd := sessionsCmd()
	if len(cmd.Aliases) == 0 || cmd.Aliases[0] != "s" {
		t.Errorf("expected alias 's', got %v", cmd.Aliases)
	}
}
