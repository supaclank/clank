package sessionsync

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
)

// fingerprintTailWindow bounds the tail read for the last-uuid scan. The
// last conversation entry plus any trailing control lines live well within
// this; a final entry larger than the window yields "" and the caller falls
// back to the file-mtime comparison.
const fingerprintTailWindow = 64 * 1024

// claudeSessionFingerprint returns a content version for a Claude session:
// the uuid of the last transcript entry that carries one.
//
// Conversation graph nodes (user/assistant/system/…) have a top-level uuid;
// the control lines a read-only `claude --resume` appends — mode, ai-title,
// permission-mode, queue-operation — do not. So merely opening a session to
// read it does NOT move the fingerprint, where the file mtime would. The log
// is append-only, so the last uuid changes iff a genuine turn was appended.
//
// Returns "" when the transcript is absent or no uuid is found in the tail
// window; the caller then falls back to the (mtime-based) UpdatedAt.
func claudeSessionFingerprint(cwd, sessionID string) string {
	path, err := claudeTranscriptPath(cwd, sessionID)
	if err != nil {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	start := int64(0)
	if fi.Size() > fingerprintTailWindow {
		start = fi.Size() - fingerprintTailWindow
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return ""
	}

	lines := bytes.Split(buf, []byte{'\n'})
	// Drop the first (possibly partial) line when we started mid-file.
	// TODO(ai-review): a UUID entry straddling the window boundary is silently missed (falls back to mtime). https://github.com/Acksell/clank/pull/41#discussion_r3343017381
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var probe struct {
			UUID string `json:"uuid"`
		}
		if json.Unmarshal(line, &probe) == nil && probe.UUID != "" {
			return probe.UUID
		}
	}
	return ""
}
