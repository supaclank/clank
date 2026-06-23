//go:build !darwin

package tui

// readClipboardImage is not yet implemented off macOS. It returns ok=false so
// the paste path falls back to normal text handling rather than erroring. Add a
// platform implementation (e.g. xclip/wl-paste on Linux) behind this same
// signature to enable image paste there.
func readClipboardImage() (data []byte, mime string, ok bool, err error) {
	return nil, "", false, nil
}
