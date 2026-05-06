package daemonclient

import (
	"strings"
	"testing"
)

// TestSummarizeBody_HTMLExtractsTitle is the headline regression
// test: when the daemon (or the upstream sprite edge / cloudflare
// tunnel) returns a multi-KB HTML error page, the TUI used to dump
// the entire HTML — stylesheet and all — into its error banner.
// summarizeBody should extract just the <title>.
func TestSummarizeBody_HTMLExtractsTitle(t *testing.T) {
	t.Parallel()
	body := []byte(`<!DOCTYPE html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <title>404 | Sprites</title>
    <style>* { box-sizing: border-box; }</style>
  </head>
  <body>not found</body>
</html>`)
	got := summarizeBody("text/html; charset=utf-8", body)
	if got != "404 | Sprites" {
		t.Errorf("HTML title extraction: got %q, want %q", got, "404 | Sprites")
	}
}

func TestSummarizeBody_HTMLWithoutTitleFallsBackToTrunc(t *testing.T) {
	t.Parallel()
	body := []byte(`<html><body>some markup without a title tag</body></html>`)
	got := summarizeBody("text/html", body)
	if got == "" {
		t.Error("expected fallback summary, got empty")
	}
	if strings.Contains(got, "<title") {
		t.Error("fallback should not include <title> markup")
	}
}

func TestSummarizeBody_PlainTextCollapsesWhitespace(t *testing.T) {
	t.Parallel()
	body := []byte("line one\n\n  line  two\t\ttabbed   out\n")
	got := summarizeBody("text/plain", body)
	want := "line one line two tabbed out"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestSummarizeBody_TruncatesLongPayloads pins the hard 240-char cap
// so a giant text response can't blow the inbox banner up.
func TestSummarizeBody_TruncatesLongPayloads(t *testing.T) {
	t.Parallel()
	body := []byte(strings.Repeat("a", 1000))
	got := summarizeBody("text/plain", body)
	if len(got) > 250 {
		t.Errorf("summary length %d exceeds reasonable cap (~240)", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected truncation marker, got tail %q", got[len(got)-10:])
	}
}

func TestHTMLTitle_HandlesAttributesAndCase(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		`<title>Plain</title>`:           "Plain",
		`<TITLE>Upper</TITLE>`:           "Upper",
		`<title id="x">  Spaced  </title>`: "Spaced",
		`<head><title>First</title><title>Second</title></head>`: "First",
		`no title here`:                  "",
	}
	for in, want := range cases {
		if got := htmlTitle([]byte(in)); got != want {
			t.Errorf("htmlTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
