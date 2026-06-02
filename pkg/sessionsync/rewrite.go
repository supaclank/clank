package sessionsync

import (
	"encoding/json"
	"fmt"
	"os"
)

// RewriteImportBlob reads an opencode export blob, rebases
// info.directory to destDir, and writes the result to a sibling temp
// file (srcPath + ".rewritten.json"). Returns the new path; the caller
// owns cleanup (os.Remove).
//
// The rebase exists because info.directory in the source's blob is a
// filesystem path that doesn't resolve on the destination — opencode
// would file the imported session under a synthetic "global" project
// without it. projectID is cleared so opencode rederives it from the
// new directory. This is the ONLY blob mutation we perform; every other
// field is preserved verbatim, and any `opencode import` validation
// error is surfaced rather than papered over.
func RewriteImportBlob(srcPath, destDir string) (string, error) {
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return "", fmt.Errorf("read blob: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("parse blob: %w", err)
	}
	info, ok := doc["info"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("blob missing top-level info object")
	}
	info["directory"] = destDir
	info["projectID"] = ""
	doc["info"] = info

	out, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal rewritten blob: %w", err)
	}
	dstPath := srcPath + ".rewritten.json"
	if err := os.WriteFile(dstPath, out, 0o600); err != nil {
		return "", fmt.Errorf("write rewritten blob: %w", err)
	}
	return dstPath, nil
}
