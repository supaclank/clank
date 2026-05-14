package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// OpenCodeExportSession writes the opencode session export blob for
// externalID to dst. Calls `opencode export <externalID>`; stdout is
// the JSON blob, stderr carries an "Exporting session: ..." prefix
// which we discard.
//
// Hermetic — does NOT require a running `opencode serve`. Reads
// directly from opencode's local storage. (Verified by
// TestOpenCodeImportSemantics.)
//
// projectDir is passed as cwd to the subprocess for parity with
// startServer; opencode currently ignores cwd for export/import,
// but matching the convention insulates us from future changes.
func OpenCodeExportSession(ctx context.Context, projectDir, externalID string, dst io.Writer) error {
	if externalID == "" {
		return fmt.Errorf("opencode export: externalID is required")
	}
	cmd := exec.CommandContext(ctx, "opencode", "export", externalID)
	if projectDir != "" {
		cmd.Dir = projectDir
	}
	cmd.Stdout = dst
	// Capture stderr so we can surface it on error. On success we
	// discard the "Exporting session: <id>" prefix opencode writes
	// there.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("opencode export %s: %w: %s", externalID, err, stderr.String())
	}
	return nil
}

// OpenCodeImportSession invokes `opencode import <blobPath>`.
// Preserves the session's info.id (verified by
// TestOpenCodeImportSemantics) and additive-merges messages by ID
// when re-importing over an existing session.
//
// Returns the external_id (opencode session ID) reported by import's
// stdout — typically a "Imported session: <id>" line. Returns an
// error if the blob is malformed or opencode rejects the schema.
func OpenCodeImportSession(ctx context.Context, projectDir, blobPath string) (externalID string, err error) {
	if blobPath == "" {
		return "", fmt.Errorf("opencode import: blobPath is required")
	}
	cmd := exec.CommandContext(ctx, "opencode", "import", blobPath)
	if projectDir != "" {
		cmd.Dir = projectDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("opencode import %s: %w: %s", blobPath, err, string(out))
	}
	id, ok := parseImportedSessionID(out)
	if !ok {
		return "", fmt.Errorf("opencode import %s: could not parse session ID from output: %s", blobPath, string(out))
	}
	return id, nil
}

// importedSessionPrefix is what `opencode import` prints to stdout on
// success: "Imported session: ses_xxx". Surface plain ANSI-coloured
// variants too — opencode wraps the prefix in color codes when
// stdout is a TTY, but for safety we accept both forms.
const importedSessionPrefix = "Imported session: "

// parseImportedSessionID scans the output of `opencode import` for
// the "Imported session: <id>" line and returns the id. Strips
// surrounding ANSI escape codes so it works whether opencode wrote
// to a TTY or a pipe.
func parseImportedSessionID(out []byte) (string, bool) {
	stripped := stripANSI(out)
	idx := bytes.Index(stripped, []byte(importedSessionPrefix))
	if idx < 0 {
		return "", false
	}
	rest := stripped[idx+len(importedSessionPrefix):]
	// session ID runs until first whitespace / newline / end.
	end := bytes.IndexAny(rest, " \t\r\n")
	if end < 0 {
		end = len(rest)
	}
	id := string(bytes.TrimSpace(rest[:end]))
	if id == "" {
		return "", false
	}
	return id, true
}

// OpenCodeVersion runs `opencode --version` and returns the
// trimmed stdout. opencode 1.x prints just the bare version
// (e.g. "1.14.48"). Caller decides what to do with mismatches —
// see compat.OpencodeVersionsCompatible.
//
// The subprocess is called with no special env so it inherits
// clank-host's environment (HOME etc.). Reading the version
// doesn't touch session storage, so isolation isn't required.
func OpenCodeVersion(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "opencode", "--version")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		stderr := ""
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		return "", fmt.Errorf("opencode --version: %w: %s", err, stderr)
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("opencode --version: empty output")
	}
	return v, nil
}

// stripANSI removes ECMA-48 CSI escape sequences (the most common
// ANSI color/cursor codes) from the byte slice. opencode emits them
// when stdout is a TTY; pipes get plain bytes, but we run via
// exec.Command so usually there's no TTY and this is a no-op.
func stripANSI(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '[' {
			// CSI sequence: skip until a final byte in 0x40..0x7e
			j := i + 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
				j++
			}
			if j < len(b) {
				j++ // skip the final byte too
			}
			i = j
			continue
		}
		out = append(out, b[i])
		i++
	}
	return out
}
