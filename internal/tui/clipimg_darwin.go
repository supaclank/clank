//go:build darwin

package tui

import (
	"fmt"
	"os"
	"os/exec"
)

// readClipboardImage extracts a PNG from the macOS clipboard via osascript —
// terminals don't deliver image bytes through paste, so we read the OS
// clipboard directly. Returns ok=false (nil error) when the clipboard holds no
// image: the PNGf coercion fails and we treat that as "nothing to attach" so
// the caller falls back to normal text paste. osascript ships with macOS, so
// there's no external dependency.
func readClipboardImage() (data []byte, mime string, ok bool, err error) {
	tmp, err := os.CreateTemp("", "clank-clip-*.png")
	if err != nil {
		return nil, "", false, err
	}
	path := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(path) }()

	// Coerce FIRST so a non-image clipboard errors before we touch the file.
	script := fmt.Sprintf(`set thePNG to (the clipboard as «class PNGf»)
set theFile to (open for access POSIX file %q with write permission)
set eof theFile to 0
write thePNG to theFile
close access theFile`, path)

	if _, runErr := exec.Command("osascript", "-e", script).CombinedOutput(); runErr != nil {
		// Most common case: the clipboard has no image. Not an error.
		return nil, "", false, nil
	}

	data, err = os.ReadFile(path)
	if err != nil {
		return nil, "", false, err
	}
	if len(data) == 0 {
		return nil, "", false, nil
	}
	return data, "image/png", true, nil
}
