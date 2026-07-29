package clankcli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/acksell/clank/internal/filepreview"
)

// previewFileArg decides whether the preview args name a file to open.
// Exactly one arg that stats as a regular file selects file mode; a
// single path-shaped arg (contains a dot or separator) that doesn't
// resolve is a hard error — a typo'd path must never silently become an
// agent prompt. Anything else is the prompt flow.
func previewFileArg(args []string) (string, bool, error) {
	if len(args) != 1 {
		return "", false, nil
	}
	arg := args[0]
	info, err := os.Stat(arg)
	if err == nil {
		if info.Mode().IsRegular() {
			return arg, true, nil
		}
		if info.IsDir() {
			return "", false, fmt.Errorf("%s is a directory — pass a file to preview one, or run clank preview without arguments to preview the project", arg)
		}
		return "", false, fmt.Errorf("%s is not a regular file", arg)
	}
	if pathShaped(arg) {
		return "", false, fmt.Errorf("no such file %s (path-shaped arguments are never treated as prompts)", arg)
	}
	return "", false, nil
}

// pathShaped: contains a dot or separator, i.e. reads as a filename.
func pathShaped(s string) bool {
	return strings.ContainsAny(s, "./"+string(os.PathSeparator))
}

// runFilePreview is the `clank preview <file>` arm: open one file raw
// in the browser behind the overlay proxy — highlight text or ⌘-point
// at parts of it and tell the agent what to change; the page reloads
// live as the agent edits the file on disk. No dev server, no QR, works
// in any folder: the daemon is only needed for the overlay's session
// API.
func runFilePreview(projectDir, file, backend string, port int) error {
	projectDir, err := resolveProjectDir(projectDir)
	if err != nil {
		return err
	}
	rel, err := projectRelPath(projectDir, file)
	if err != nil {
		return err
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	_, sockPath, startedDaemon, err := ensurePreviewDaemon()
	if err != nil {
		return err
	}
	if startedDaemon {
		defer func() {
			fmt.Println("Stopping the daemon clank preview started…")
			stopLocalDaemon()
		}()
	}

	bt, err := resolveBackend(backend, os.Stderr)
	if err != nil {
		return err
	}

	srv, err := filepreview.Start(filepreview.Options{Root: projectDir, Entry: rel})
	if err != nil {
		return fmt.Errorf("start file preview: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	fmt.Printf("Previewing %s — the page live-reloads when the file changes on disk.\n", rel)
	// No session pre-create: the overlay lazily creates one on the first
	// submit, exactly like a promptless project preview.
	return runWebPreview(sigCtx, projectDir, sockPath, "", string(bt), srv.Port, port)
}

// projectRelPath resolves file (absolute or cwd-relative) to its
// project-relative path — symlink-resolved on both sides so macOS's
// /var → /private/var aliasing compares equal — and rejects anything
// outside the project.
func projectRelPath(projectDir, file string) (string, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", err
	}
	fileReal, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", file, err)
	}
	rootReal, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		return "", fmt.Errorf("resolve project dir %s: %w", projectDir, err)
	}
	rel, err := filepath.Rel(rootReal, fileReal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the project directory %s — pass --project with a directory that contains it", file, projectDir)
	}
	return filepath.ToSlash(rel), nil
}
