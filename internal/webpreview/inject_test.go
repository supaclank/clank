package webpreview

import (
	"strings"
	"testing"
)

func TestInjectHTML(t *testing.T) {
	t.Parallel()
	snip := []byte("<script>X</script>")

	cases := []struct {
		name string
		in   string
		// wantBefore is the marker the snippet must appear immediately
		// before; empty means "appended at the end".
		wantBefore string
	}{
		{"before head close", "<html><head><title>t</title></head><body>b</body></html>", "</head>"},
		{"case-insensitive head", "<HTML><HEAD></HEAD><BODY></BODY></HTML>", "</HEAD>"},
		{"body fallback when no head", "<html><body>hi</body></html>", "</body>"},
		{"append when no head or body", "<div>fragment</div>", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := string(injectHTML([]byte(tc.in), snip))
			idx := strings.Index(out, string(snip))
			if idx < 0 {
				t.Fatalf("snippet not injected: %q", out)
			}
			rest := out[idx+len(snip):]
			if !strings.HasPrefix(rest, "\n") {
				t.Fatalf("snippet not newline-terminated: %q", rest)
			}
			rest = rest[1:]
			if tc.wantBefore == "" {
				if rest != "" {
					t.Fatalf("want snippet appended at end, got trailing %q", rest)
				}
				return
			}
			if !strings.HasPrefix(rest, tc.wantBefore) {
				t.Fatalf("want snippet before %q, got %q", tc.wantBefore, rest)
			}
		})
	}

	if got := injectHTML(nil, snip); len(got) != 0 {
		t.Fatalf("empty body must stay empty, got %q", got)
	}
}
