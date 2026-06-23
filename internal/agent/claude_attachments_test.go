package agent

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildUserStreamMessage_TextAndImage(t *testing.T) {
	t.Parallel()
	imgs := []resolvedImage{{Mime: "image/png", Data: []byte("PNGDATA")}}
	msg := buildUserStreamMessage("hello", imgs)

	if msg.Type != "user" {
		t.Fatalf("type=%q want user", msg.Type)
	}
	if msg.SessionID != claudeDefaultSessionID {
		t.Fatalf("session=%q want %q", msg.SessionID, claudeDefaultSessionID)
	}
	m, ok := msg.Message.(map[string]any)
	if !ok {
		t.Fatalf("Message is %T, want map", msg.Message)
	}
	if m["role"] != "user" {
		t.Fatalf("role=%v", m["role"])
	}
	content, ok := m["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content is %T, want []map", m["content"])
	}
	if len(content) != 2 {
		t.Fatalf("len(content)=%d want 2", len(content))
	}
	if content[0]["type"] != "text" || content[0]["text"] != "hello" {
		t.Fatalf("text block: %+v", content[0])
	}
	if content[1]["type"] != "image" {
		t.Fatalf("image block type: %+v", content[1])
	}
	src, ok := content[1]["source"].(map[string]any)
	if !ok {
		t.Fatalf("source is %T, want map", content[1]["source"])
	}
	if src["type"] != "base64" || src["media_type"] != "image/png" {
		t.Fatalf("source meta: %+v", src)
	}
	if want := base64.StdEncoding.EncodeToString([]byte("PNGDATA")); src["data"] != want {
		t.Fatalf("data=%v want %q", src["data"], want)
	}
}

func TestBuildUserStreamMessage_ImageOnly(t *testing.T) {
	t.Parallel()
	// Empty text must still produce a valid content array — image only.
	msg := buildUserStreamMessage("", []resolvedImage{{Mime: "image/jpeg", Data: []byte("J")}})
	m := msg.Message.(map[string]any)
	content := m["content"].([]map[string]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 block (image only), got %d", len(content))
	}
	if content[0]["type"] != "image" {
		t.Fatalf("expected image block, got %+v", content[0])
	}
}

func TestBuildUserStreamMessage_SerializesToStreamJSON(t *testing.T) {
	t.Parallel()
	// The CLI consumes the marshaled form, so the wire shape matters.
	msg := buildUserStreamMessage("look", []resolvedImage{{Mime: "image/webp", Data: []byte("W")}})
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{
		`"type":"user"`,
		`"role":"user"`,
		`"type":"image"`,
		`"media_type":"image/webp"`,
		`"type":"base64"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("serialized message missing %q\n%s", want, s)
		}
	}
}
