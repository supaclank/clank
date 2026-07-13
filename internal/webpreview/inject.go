package webpreview

import "bytes"

var (
	headClose = []byte("</head>")
	bodyClose = []byte("</body>")
)

// injectHTML returns body with snippet inserted before </head>
// (preferred: the overlay script must exist before the app hydrates so
// a hotkey works even while the guest is still booting), falling back
// to before </body>, then to plain append. Case-insensitive tag match;
// the input is returned unmodified only when it's empty.
func injectHTML(body, snippet []byte) []byte {
	if len(body) == 0 {
		return body
	}
	idx := indexTagFold(body, headClose)
	if idx < 0 {
		idx = indexTagFold(body, bodyClose)
	}
	if idx < 0 {
		idx = len(body)
	}
	out := make([]byte, 0, len(body)+len(snippet)+1)
	out = append(out, body[:idx]...)
	out = append(out, snippet...)
	out = append(out, '\n')
	out = append(out, body[idx:]...)
	return out
}

// indexTagFold finds tag (an ASCII HTML tag like "</head>") in body,
// case-insensitively. HTML tags are strictly ASCII, so this scans byte
// by byte instead of bytes.ToLower(body): ToLower would allocate a full
// copy of body (megabytes of dev-server HTML, on every request) and,
// worse, can desync its match index from the original buffer whenever a
// multi-byte rune folds to fewer bytes (e.g. U+212A KELVIN SIGN → 'k').
//
// tag's first byte ('<') has no case fold, so bytes.IndexByte — a tight,
// heavily-optimized stdlib scan — skips the non-candidate bytes instead
// of running the fold comparison at every position.
func indexTagFold(body, tag []byte) int {
	if len(tag) == 0 {
		return 0
	}
	first := tag[0]
	i := 0
	for {
		idx := bytes.IndexByte(body[i:], first)
		if idx < 0 {
			return -1
		}
		i += idx
		if i+len(tag) > len(body) {
			return -1
		}
		if asciiEqualFold(body[i:i+len(tag)], tag) {
			return i
		}
		i++
	}
}

func asciiEqualFold(a, b []byte) bool {
	for i := range a {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
