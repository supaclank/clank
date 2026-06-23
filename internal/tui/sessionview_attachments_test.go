package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func newPillModel() *SessionViewModel {
	ta := newPromptTextarea("", 5)
	ta.SetWidth(80)
	ta.Focus()
	return &SessionViewModel{input: ta}
}

func TestStageClipboardImage_InsertsPillAndDataSource(t *testing.T) {
	m := newPillModel()
	m.input.InsertString("look at ")
	m.stageAttachment("image/png", "", []byte("PNGDATA"))

	if got, want := m.input.Value(), "look at <image1.png>"; got != want {
		t.Fatalf("value=%q want %q", got, want)
	}
	text, atts := m.promptForSend()
	if text != "look at" {
		t.Fatalf("text=%q", text)
	}
	if len(atts) != 1 {
		t.Fatalf("atts=%d want 1", len(atts))
	}
	if atts[0].Mime != "image/png" || atts[0].Filename != "image1.png" {
		t.Fatalf("attachment meta: %+v", atts[0])
	}
	if !strings.HasPrefix(atts[0].Source, "data:image/png;base64,") {
		t.Fatalf("clipboard image should be a data: source, got %q", atts[0].Source)
	}
}

func TestStageDroppedFile_UsesFileSource(t *testing.T) {
	m := newPillModel()
	m.stageAttachment("image/jpeg", "/tmp/shot.jpg", nil)
	_, atts := m.promptForSend()
	if len(atts) != 1 {
		t.Fatalf("atts=%d want 1", len(atts))
	}
	if atts[0].Source != "file:///tmp/shot.jpg" {
		t.Fatalf("dropped file should be a file:// source, got %q", atts[0].Source)
	}
}

func TestPromptForSend_DropsRemovedPill(t *testing.T) {
	m := newPillModel()
	m.stageAttachment("image/png", "", []byte("A"))
	m.input.SetValue("hello") // pill token gone; registry stale
	text, atts := m.promptForSend()
	if text != "hello" || len(atts) != 0 {
		t.Fatalf("expected pill dropped: text=%q atts=%d", text, len(atts))
	}
}

func TestDeletePillBeforeCursor_DeletesWholeTokenAtomically(t *testing.T) {
	m := newPillModel()
	m.input.InsertString("hi ")
	m.stageAttachment("image/png", "", []byte("A")) // cursor lands right after the pill

	if !m.deletePillBeforeCursor(tea.KeyPressMsg{Code: tea.KeyBackspace}) {
		t.Fatal("expected the pill to be deleted")
	}
	if got := m.input.Value(); got != "hi " {
		t.Fatalf("after delete value=%q want %q", got, "hi ")
	}
	if len(m.attachments) != 0 {
		t.Fatalf("attachment not unregistered: %d", len(m.attachments))
	}
}

func TestDeletePillBeforeCursor_NoPillFallsThrough(t *testing.T) {
	m := newPillModel()
	m.input.InsertString("hello")
	if m.deletePillBeforeCursor(tea.KeyPressMsg{Code: tea.KeyBackspace}) {
		t.Fatal("no pill present — should not handle the backspace")
	}
}

func TestHighlightPills_WrapsToken(t *testing.T) {
	m := newPillModel()
	m.stageAttachment("image/png", "", []byte("A"))
	out := m.highlightPills("x <image1.png> y")
	if !strings.Contains(out, "<image1.png>") {
		t.Fatalf("token text missing: %q", out)
	}
	if out == "x <image1.png> y" {
		t.Fatalf("token was not styled (no ANSI added): %q", out)
	}
}

func TestMaybePasteImagePath_StagesExistingImageFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.png")
	if err := os.WriteFile(path, []byte("PNG"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newPillModel()
	if !m.maybePasteImagePath(path) {
		t.Fatal("expected the image path to be staged")
	}
	_, atts := m.promptForSend()
	if len(atts) != 1 || atts[0].Source != "file://"+path {
		t.Fatalf("atts=%+v", atts)
	}

	// A non-image path is ignored.
	if m.maybePasteImagePath(filepath.Join(dir, "notes.txt")) {
		t.Fatal("non-image path should not stage")
	}
}

func TestCleanPastedPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/tmp/a.png", "/tmp/a.png"},
		{"  /tmp/a.png \n", "/tmp/a.png"},
		{`"/tmp/a b.png"`, "/tmp/a b.png"},
		{`/tmp/a\ b.png`, "/tmp/a b.png"},
		{"line1\nline2", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := cleanPastedPath(c.in); got != c.want {
			t.Errorf("cleanPastedPath(%q)=%q want %q", c.in, got, c.want)
		}
	}
}
