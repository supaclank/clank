package sessionsync

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// JSONL transcript field keys clank reads or rewrites.
const (
	claudeFieldCwd       = "cwd"
	claudeFieldSessionID = "sessionId"
)

// RewriteClaudeImportBlob streams a Claude JSONL transcript and rebases the
// per-line top-level "cwd" to destDir (the importing host's worktree path),
// writing the result to srcPath + ".rewritten.jsonl". Returns the new path;
// the caller owns cleanup.
//
// This is the Claude twin of RewriteImportBlob (opencode). It is the ONLY
// mutation import performs: lines that carry a "cwd" get that one field
// rebased; every other field is preserved byte-for-byte (via
// json.RawMessage, so numbers and message content are untouched), and lines
// without a "cwd" (metadata: ai-title, mode, queue-operation, …) pass
// through verbatim. gitBranch and sessionId are deliberately left alone.
//
// Rewrite is import-only and always targets the local worktree, which is
// what makes the laptop↔sandbox round trip reversible: export copies bytes
// verbatim, import rebases to the destination, so a full round trip lands
// the cwd back at its origin. Pinned by the idempotent round-trip test.
func RewriteClaudeImportBlob(srcPath, destDir string) (string, error) {
	if destDir == "" {
		return "", fmt.Errorf("rewrite claude blob: destDir is required")
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("rewrite claude blob: open: %w", err)
	}
	defer src.Close()

	dstPath := srcPath + ".rewritten" + claudeTranscriptExt
	dst, err := os.Create(dstPath)
	if err != nil {
		return "", fmt.Errorf("rewrite claude blob: create: %w", err)
	}

	if err := rewriteClaudeStream(src, dst, destDir); err != nil {
		_ = dst.Close()
		return "", err
	}
	if err := dst.Close(); err != nil {
		return "", fmt.Errorf("rewrite claude blob: close: %w", err)
	}
	return dstPath, nil
}

// rewriteClaudeStream copies r to w line by line, rebasing cwd on the lines
// that carry it. Uses bufio.Reader.ReadString rather than bufio.Scanner so
// large tool-output lines (which can exceed Scanner's default token cap) are
// handled.
func rewriteClaudeStream(r io.Reader, w io.Writer, destDir string) error {
	br := bufio.NewReader(r)
	bw := bufio.NewWriter(w)
	for {
		line, readErr := br.ReadString('\n')
		if len(line) > 0 {
			out, err := rewriteClaudeLine(line, destDir)
			if err != nil {
				return err
			}
			if _, err := bw.WriteString(out); err != nil {
				return fmt.Errorf("rewrite claude blob: write: %w", err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("rewrite claude blob: read: %w", readErr)
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("rewrite claude blob: flush: %w", err)
	}
	return nil
}

// rewriteClaudeLine rebases the top-level cwd on a single JSONL line if it
// has one, preserving the line terminator. Lines that are blank, not a JSON
// object, or carry no cwd are returned unchanged.
func rewriteClaudeLine(line, destDir string) (string, error) {
	content, suffix := splitLineTerminator(line)
	if strings.TrimSpace(content) == "" {
		return line, nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &obj); err != nil {
		// Not a JSON object — leave untouched rather than risk corrupting
		// it. Claude transcripts are one JSON object per line, so this is a
		// defensive passthrough, not an expected path.
		return line, nil
	}
	if _, ok := obj[claudeFieldCwd]; !ok {
		return line, nil
	}

	rebased, err := json.Marshal(destDir)
	if err != nil {
		return "", fmt.Errorf("rewrite claude blob: marshal cwd: %w", err)
	}
	obj[claudeFieldCwd] = rebased
	out, err := json.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("rewrite claude blob: marshal line: %w", err)
	}
	return string(out) + suffix, nil
}

// splitLineTerminator separates a line's content from its trailing
// terminator ("", "\n", or "\r\n") so the exact terminator can be re-emitted
// after rewriting.
func splitLineTerminator(line string) (content, suffix string) {
	if strings.HasSuffix(line, "\n") {
		line = line[:len(line)-1]
		if strings.HasSuffix(line, "\r") {
			return line[:len(line)-1], "\r\n"
		}
		return line, "\n"
	}
	return line, ""
}
