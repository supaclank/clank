package agent

import (
	"strings"
	"testing"
)

func TestBuildPromptParams_AppendsImageFilePart(t *testing.T) {
	t.Parallel()
	b := &OpenCodeBackend{sessionID: "sess-1"}
	imgs := []resolvedImage{{Mime: "image/png", Filename: "shot.png", Data: []byte("PNGDATA")}}

	req := b.buildPromptParams(SendMessageOpts{Text: "hi"}, imgs)

	if req.SessionID != "sess-1" {
		t.Fatalf("SessionID=%q want sess-1", req.SessionID)
	}
	if len(req.Parts) != 2 {
		t.Fatalf("len(Parts)=%d want 2", len(req.Parts))
	}
	if req.Parts[0].Text == nil || req.Parts[0].Text.Text != "hi" {
		t.Fatalf("part 0 not the text part: %+v", req.Parts[0])
	}
	fp := req.Parts[1].File
	if fp == nil {
		t.Fatalf("part 1 not a file part: %+v", req.Parts[1])
	}
	if fp.Mime != "image/png" {
		t.Fatalf("file mime=%q", fp.Mime)
	}
	if !strings.HasPrefix(fp.URL, "data:image/png;base64,") {
		t.Fatalf("file url not an inline data URL: %s", fp.URL)
	}
	if fp.Filename == nil || *fp.Filename != "shot.png" {
		t.Fatalf("filename=%v want shot.png", fp.Filename)
	}
}

func TestBuildPromptParams_TextOnly(t *testing.T) {
	t.Parallel()
	b := &OpenCodeBackend{sessionID: "s"}
	req := b.buildPromptParams(SendMessageOpts{Text: "hi"}, nil)
	if len(req.Parts) != 1 {
		t.Fatalf("expected only the text part, got %d", len(req.Parts))
	}
	if req.Parts[0].File != nil {
		t.Fatalf("unexpected file part on a text-only message")
	}
}
