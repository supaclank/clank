package host

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/acksell/clank/internal/agent"
)

func TestCatalogStore_RoundTripsPerDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	s, err := newCatalogStore(dir, agent.BackendCodex)
	if err != nil {
		t.Fatalf("newCatalogStore: %v", err)
	}
	s.put("/proj/a", func(e *catalogEntry) { e.Models = []agent.ModelInfo{{ID: "gpt-5.2-codex"}} })
	s.put("/proj/a", func(e *catalogEntry) { e.Modes = []agent.SessionMode{{ID: "agent"}} })
	s.put("/proj/b", func(e *catalogEntry) { e.Models = []agent.ModelInfo{{ID: "o5"}} })

	reopened, err := newCatalogStore(dir, agent.BackendCodex)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	all := reopened.all()
	// Both sinks land on the same dir entry: the modes write must not
	// clobber the models the previous write recorded.
	if got := all["/proj/a"]; len(got.Models) != 1 || len(got.Modes) != 1 {
		t.Errorf("/proj/a = %+v, want both models and modes", got)
	}
	if got := all["/proj/b"].Models; len(got) != 1 || got[0].ID != "o5" {
		t.Errorf("/proj/b models = %+v", got)
	}
	if dirs := reopened.persistedDirs(); len(dirs) != 2 {
		t.Errorf("persisted dirs = %v, want both projects", dirs)
	}
}

// Each backend owns its own file, so codex's catalog can never answer for
// opencode — whose modes and models are project-config dependent.
func TestCatalogStore_IsPerBackend(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	codex, err := newCatalogStore(dir, agent.BackendCodex)
	if err != nil {
		t.Fatalf("newCatalogStore: %v", err)
	}
	codex.put("/proj/a", func(e *catalogEntry) { e.Modes = []agent.SessionMode{{ID: "agent"}} })

	oc, err := newCatalogStore(dir, agent.BackendOpenCode)
	if err != nil {
		t.Fatalf("newCatalogStore: %v", err)
	}
	if got := oc.all(); len(got) != 0 {
		t.Errorf("opencode store read codex's catalog: %+v", got)
	}
}

// The catalog is derived data: a corrupt file must not fail the daemon or
// wedge the picker — it starts empty, which costs one reprobe.
func TestCatalogStore_CorruptFileStartsEmptyAndStaysWritable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, string(agent.BackendCodex)+".json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}

	s, err := newCatalogStore(dir, agent.BackendCodex)
	if err != nil {
		t.Fatalf("newCatalogStore on corrupt file: %v", err)
	}
	if got := s.all(); len(got) != 0 {
		t.Fatalf("corrupt file yielded %+v, want an empty catalog", got)
	}

	s.put("/proj/a", func(e *catalogEntry) { e.Models = []agent.ModelInfo{{ID: "gpt-5.2-codex"}} })
	reopened, err := newCatalogStore(dir, agent.BackendCodex)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := reopened.all()["/proj/a"].Models; len(got) != 1 {
		t.Errorf("catalog did not recover after a corrupt read: %+v", got)
	}
}

// A missing catalog dir is the first-run case: the store creates it on the
// first write rather than erroring or losing the entry.
func TestCatalogStore_CreatesMissingDir(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "cache", "acp-catalog")

	s, err := newCatalogStore(dir, agent.BackendClaudeCode)
	if err != nil {
		t.Fatalf("newCatalogStore: %v", err)
	}
	s.put("/proj/a", func(e *catalogEntry) { e.Modes = []agent.SessionMode{{ID: "plan"}} })

	reopened, err := newCatalogStore(dir, agent.BackendClaudeCode)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := reopened.all()["/proj/a"].Modes; len(got) != 1 {
		t.Errorf("first write to a missing dir was lost: %+v", got)
	}
}

// An empty catalog dir is a wiring mistake (the manager would silently
// lose persistence), so construction fails rather than degrading.
func TestCatalogStore_RequiresDir(t *testing.T) {
	t.Parallel()
	if _, err := newCatalogStore("", agent.BackendCodex); err == nil {
		t.Fatal("newCatalogStore(\"\") succeeded, want an error")
	}
}
