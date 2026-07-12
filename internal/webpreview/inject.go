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
	lower := bytes.ToLower(body)
	idx := bytes.Index(lower, headClose)
	if idx < 0 {
		idx = bytes.Index(lower, bodyClose)
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
