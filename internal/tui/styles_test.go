package tui

import (
	"errors"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestRenderError_PreservesFullMessage is a regression test for error banners
// at the top of the session/inbox/compose views being clipped on the right
// edge when the error string is longer than the terminal width — making it
// impossible to copy the full error from the terminal.
//
// All characters of the underlying error must be present in the rendered
// (ANSI-stripped) output, and no rendered visual line may exceed `width`.
func TestRenderError_PreservesFullMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		err   error
		width int
	}{
		{
			name:  "long unbreakable token (URL-like) wraps within width",
			err:   errors.New("failed to POST https://very.long.example.com/api/v1/sessions/abcdefghijklmnopqrstuvwxyz/messages: connection refused"),
			width: 40,
		},
		{
			name:  "long sentence wraps at word boundaries",
			err:   errors.New("the agent backend reported an unexpected internal failure while attempting to commit the working tree"),
			width: 30,
		},
		{
			name:  "narrow width still preserves all bytes",
			err:   errors.New("short-but-hyphenated-error-message-here"),
			width: 10,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rendered := renderError(tc.err, tc.width)
			plain := ansi.Strip(rendered)

			// Every word of the error must appear in the output.
			// (We can't substring-match the whole message because wrapping
			// inserts newlines mid-message — but no original word is split
			// when we wrap on whitespace/hyphens, and even hard-broken
			// long tokens preserve all runes when concatenated.)
			compactGot := strings.Join(strings.Fields(strings.ReplaceAll(plain, "\n", " ")), "")
			compactWant := strings.Join(strings.Fields(tc.err.Error()), "")
			if !strings.Contains(compactGot, compactWant) {
				t.Errorf("rendered output is missing characters from the error.\nwant chars: %q\n got chars: %q\nrendered:\n%s",
					compactWant, compactGot, plain)
			}

			// No visual line may exceed width — that's what causes the
			// terminal to clip and break copy/paste.
			for i, line := range strings.Split(plain, "\n") {
				if w := lipgloss.Width(line); w > tc.width {
					t.Errorf("line %d width %d exceeds limit %d: %q", i, w, tc.width, line)
				}
			}
		})
	}
}

// TestRenderError_NilReturnsEmpty ensures the helper is safe to call with no
// active error.
func TestRenderError_NilReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := renderError(nil, 80); got != "" {
		t.Errorf("renderError(nil, 80) = %q, want empty", got)
	}
}

// TestRenderError_ZeroWidthDoesNotWrap documents that width<=0 falls back to
// a single styled line — callers must pass a real width when one is available.
func TestRenderError_ZeroWidthDoesNotWrap(t *testing.T) {
	t.Parallel()
	err := errors.New("hello world this is a long message")
	got := ansi.Strip(renderError(err, 0))
	if strings.Contains(got, "\n") {
		t.Errorf("expected no wrapping at width=0, got %q", got)
	}
}
