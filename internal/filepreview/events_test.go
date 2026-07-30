package filepreview

import (
	"bufio"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// openEvents subscribes to the SSE feed and returns a channel of its
// lines (closed when the stream ends).
func openEvents(t *testing.T, url string) <-chan string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	t.Cleanup(func() { resp.Body.Close() })
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	return lines
}

func awaitLine(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stream ended before %q", want)
			}
			if line == want {
				return
			}
		case <-deadline:
			t.Fatalf("no %q within 5s", want)
		}
	}
}

func TestEventsEmitOnFileChange(t *testing.T) {
	t.Parallel()
	srv, root := newTestServer(t, "note.txt", map[string]string{"note.txt": "v1"})
	lines := openEvents(t, srv.URL+"/__file/events?path=note.txt")
	awaitLine(t, lines, ": watching") // headers committed, flush proven

	target := filepath.Join(root, "note.txt")
	if err := os.WriteFile(target, []byte("v2 longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitLine(t, lines, "data: change")

	// The stream must survive its own event — the agent edits the file
	// many times over one preview.
	if err := os.WriteFile(target, []byte("v3 even longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitLine(t, lines, "data: change")
}

// TestEventsNotifyOnDeletion pins the fix for a real deletion (as
// opposed to an editor's brief unlink-then-rename gap): past the grace
// period, the client must be told to reload rather than sit on stale
// content forever.
func TestEventsNotifyOnDeletion(t *testing.T) {
	t.Parallel()
	srv, root := newTestServer(t, "note.txt", map[string]string{"note.txt": "v1"})
	lines := openEvents(t, srv.URL+"/__file/events?path=note.txt")
	awaitLine(t, lines, ": watching")

	if err := os.Remove(filepath.Join(root, "note.txt")); err != nil {
		t.Fatal(err)
	}
	awaitLine(t, lines, "data: change")
}

// TestEventsSurviveRenameGap pins the opposite side of the same fix: a
// brief unlink-then-recreate (well under the grace period) must still
// be reported as the single content change it is, not a deletion.
func TestEventsSurviveRenameGap(t *testing.T) {
	t.Parallel()
	srv, root := newTestServer(t, "note.txt", map[string]string{"note.txt": "v1"})
	lines := openEvents(t, srv.URL+"/__file/events?path=note.txt")
	awaitLine(t, lines, ": watching")

	target := filepath.Join(root, "note.txt")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitLine(t, lines, "data: change")
}

func TestEventsRequirePath(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, "note.txt", map[string]string{"note.txt": "v1"})
	resp, err := http.Get(srv.URL + "/__file/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestEventsMissingFileNotFound(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, "note.txt", map[string]string{"note.txt": "v1"})
	resp, err := http.Get(srv.URL + "/__file/events?path=nope.txt")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
