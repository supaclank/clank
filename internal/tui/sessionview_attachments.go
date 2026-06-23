package tui

// Image attachments in the prompt box. A pasted/dropped image becomes an inline
// <imageN.ext> "pill" in the textarea that behaves like one atomic character: a
// single backspace on it deletes the whole token. On send the TUI emits an
// agent.Attachment whose Source clank-host can fetch directly — a file:// path
// for a dropped local file (zero-copy) or an inline data: URL for a clipboard
// image. No gateway, no upload: the TUI only talks to clank-host.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
)

// maxPromptImageBytes caps a staged image, matching the host-side cap.
const maxPromptImageBytes = 5 << 20

// promptPillStyle renders an inline <imageN.ext> token with a background
// highlight so it reads as one object in the prompt.
var promptPillStyle = lipgloss.NewStyle().Foreground(textColor).Background(primaryColor).Bold(true)

// imageExtMimes is the closed set of image file extensions accepted for
// path/drag-drop paste. Mirrors the gateway's mime allowlist.
var imageExtMimes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
}

// pendingAttachment is an image staged in the prompt. Exactly one of path
// (a local file, sent as file://) or data (clipboard bytes, sent as data:) is
// set. It shows as an inline pill token until send.
type pendingAttachment struct {
	token    string // inline pill text, e.g. "<image1.png>"
	filename string // e.g. "image1.png"
	mime     string
	path     string // local file path → file:// source (zero-copy)
	data     []byte // inline bytes → data: source
}

// stageAttachment registers a new pill and inserts its token at the cursor.
func (m *SessionViewModel) stageAttachment(mime, path string, data []byte) {
	m.attachmentSeq++
	filename := fmt.Sprintf("image%d.%s", m.attachmentSeq, extForImageMime(mime))
	token := "<" + filename + ">"
	m.attachments = append(m.attachments, pendingAttachment{
		token:    token,
		filename: filename,
		mime:     mime,
		path:     path,
		data:     data,
	})
	m.input.InsertString(token)
}

// pasteClipboardImage reads an image from the OS clipboard (explicit ctrl+v) and
// stages it inline (data:). Returns false when the clipboard holds no image so
// the caller can fall through.
func (m *SessionViewModel) pasteClipboardImage() bool {
	data, mime, ok, err := readClipboardImage()
	if err != nil || !ok || len(data) == 0 || len(data) > maxPromptImageBytes {
		return false
	}
	m.stageAttachment(mime, "", data)
	return true
}

// maybePasteImagePath treats a pasted/dropped path pointing at an image as a
// file:// attachment (zero-copy — clank-host reads the file directly). Returns
// false when the text isn't an image path, so normal text paste proceeds.
func (m *SessionViewModel) maybePasteImagePath(text string) bool {
	p := cleanPastedPath(text)
	if p == "" {
		return false
	}
	mime, ok := imageExtMimes[strings.ToLower(filepath.Ext(p))]
	if !ok {
		return false
	}
	info, err := os.Stat(p)
	if err != nil || info.IsDir() || info.Size() == 0 || info.Size() > maxPromptImageBytes {
		return false
	}
	m.stageAttachment(mime, p, nil)
	return true
}

// deletePillBeforeCursor deletes a whole <imageN.ext> pill in one keypress when
// the cursor sits immediately after it: it replays the backspace across the
// token's runes (so the textarea keeps cursor math correct) and drops the staged
// attachment. Returns false when the cursor isn't on a pill, so a normal
// backspace runs.
func (m *SessionViewModel) deletePillBeforeCursor(backspaceMsg tea.KeyPressMsg) bool {
	if len(m.attachments) == 0 {
		return false
	}
	value := m.input.Value()
	lines := strings.Split(value, "\n")
	row := m.input.Line()
	if row < 0 || row >= len(lines) {
		return false
	}
	col := m.input.LineInfo().CharOffset
	lineRunes := []rune(lines[row])
	if col < 0 || col > len(lineRunes) {
		return false
	}
	before := string(lineRunes[:col])
	for i := range m.attachments {
		tok := m.attachments[i].token
		if strings.HasSuffix(before, tok) {
			for k := 0; k < len([]rune(tok)); k++ {
				m.input, _ = m.input.Update(backspaceMsg)
			}
			m.attachments = append(m.attachments[:i], m.attachments[i+1:]...)
			return true
		}
	}
	return false
}

// highlightPills wraps each staged pill token in a rendered prompt with a
// background highlight. Best-effort literal replace on the rendered string, so a
// token the cursor currently sits on (split by the cursor's ANSI) is left
// unstyled until the cursor moves off it.
func (m *SessionViewModel) highlightPills(view string) string {
	for _, a := range m.attachments {
		view = strings.ReplaceAll(view, a.token, promptPillStyle.Render(a.token))
	}
	return view
}

// promptForSend returns the message text with pill tokens stripped, plus the
// attachments still present in the textarea as wire-ready agent.Attachments. The
// textarea value is the source of truth — a pill removed by any means is
// dropped. A dropped file becomes a file:// source; a clipboard image an inline
// data: source.
func (m *SessionViewModel) promptForSend() (string, []agent.Attachment) {
	value := m.input.Value()
	var atts []agent.Attachment
	for _, a := range m.attachments {
		if !strings.Contains(value, a.token) {
			continue
		}
		value = strings.ReplaceAll(value, a.token, "")
		source := agent.DataURL(a.mime, a.data)
		if a.path != "" {
			source = "file://" + a.path
		}
		atts = append(atts, agent.Attachment{
			Mime:     a.mime,
			Filename: a.filename,
			Source:   source,
		})
	}
	return strings.TrimSpace(value), atts
}

func extForImageMime(mime string) string {
	switch mime {
	case "image/jpeg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	default:
		return "png"
	}
}

// cleanPastedPath normalizes a pasted/dropped path: trims whitespace and
// surrounding quotes, and unescapes the backslash-space terminals emit when a
// path with spaces is dragged in. Returns "" for multi-line or empty text.
func cleanPastedPath(text string) string {
	s := strings.TrimSpace(text)
	if s == "" || strings.ContainsAny(s, "\n\r") {
		return ""
	}
	s = strings.Trim(s, "'\"")
	s = strings.ReplaceAll(s, `\ `, " ")
	return s
}
