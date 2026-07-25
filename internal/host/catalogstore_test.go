package host

import (
	"bytes"
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
	s.putDir("/proj/a", func(e *catalogEntry) { e.Models = []agent.ModelInfo{{ID: "gpt-5.2-codex"}} })
	s.putDir("/proj/a", func(e *catalogEntry) { e.Modes = []agent.SessionMode{{ID: "agent"}} })
	s.putDir("/proj/b", func(e *catalogEntry) { e.Models = []agent.ModelInfo{{ID: "o5"}} })

	reopened, err := newCatalogStore(dir, agent.BackendCodex)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_, dirs := reopened.snapshot()
	// Both sinks land on the same dir entry: the modes write must not
	// clobber the models the previous write recorded.
	if got := dirs["/proj/a"]; len(got.Models) != 1 || len(got.Modes) != 1 {
		t.Errorf("/proj/a = %+v, want both models and modes", got)
	}
	if got := dirs["/proj/b"].Models; len(got) != 1 || got[0].ID != "o5" {
		t.Errorf("/proj/b models = %+v", got)
	}
	if ds := reopened.persistedDirs(); len(ds) != 2 {
		t.Errorf("persisted dirs = %v, want both projects", ds)
	}
}

// The backend-global entry (from the neutral prewarm) persists alongside
// the per-dir entries and survives a reopen.
func TestCatalogStore_RoundTripsGlobal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	s, err := newCatalogStore(dir, agent.BackendCodex)
	if err != nil {
		t.Fatalf("newCatalogStore: %v", err)
	}
	s.putGlobal(func(e *catalogEntry) {
		e.Models = []agent.ModelInfo{{ID: "gpt-5.2-codex"}}
		e.Modes = []agent.SessionMode{{ID: "agent"}}
	})

	reopened, err := newCatalogStore(dir, agent.BackendCodex)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	global, _ := reopened.snapshot()
	if len(global.Models) != 1 || len(global.Modes) != 1 {
		t.Errorf("global = %+v, want persisted models and modes", global)
	}
}

// Every session Open republishes its dir's (usually unchanged) catalog
// through the store, so an unchanged save must not rewrite the file — that
// blocking disk I/O would sit on the session-open path for nothing.
func TestCatalogStore_Save_SkipsRedundantWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := newCatalogStore(dir, agent.BackendOpenCode)
	if err != nil {
		t.Fatalf("newCatalogStore: %v", err)
	}
	models := []agent.ModelInfo{{ID: "gpt-5", ProviderID: "openai"}}
	s.putDir("/repo", func(e *catalogEntry) { e.Models = models })

	// Clobber the on-disk file with a sentinel. An identical put marshals to
	// the same bytes as the last write, so save() must skip and leave the
	// sentinel intact.
	sentinel := []byte("SENTINEL")
	if err := os.WriteFile(s.path, sentinel, 0o644); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}
	s.putDir("/repo", func(e *catalogEntry) { e.Models = models })
	if got, _ := os.ReadFile(s.path); !bytes.Equal(got, sentinel) {
		t.Fatalf("identical put rewrote the catalog file (want skipped write); file = %q", got)
	}

	// A genuine change must still rewrite, clobbering the sentinel.
	s.putDir("/repo", func(e *catalogEntry) { e.Models = []agent.ModelInfo{{ID: "opus", ProviderID: "anthropic"}} })
	if got, _ := os.ReadFile(s.path); bytes.Equal(got, sentinel) {
		t.Fatal("changed put did not rewrite the catalog file")
	}
}

// A neutral prewarm probe against an agent that advertises nothing (auth not
// configured yet) must not persist an empty global entry — an empty write
// with no data to show.
func TestStoreGlobal_EmptyProbeDoesNotPersist(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := newCatalogStore(dir, agent.BackendClaudeCode)
	if err != nil {
		t.Fatalf("newCatalogStore: %v", err)
	}
	m := &ACPBackendManager{store: store}

	m.storeGlobal(nil, nil)
	if _, err := os.Stat(store.path); !os.IsNotExist(err) {
		raw, _ := os.ReadFile(store.path)
		t.Fatalf("empty probe persisted a global entry (%q); want no write at all", raw)
	}

	// A probe that advertised something still persists.
	m.storeGlobal([]agent.ModelInfo{{ID: "opus"}}, nil)
	if global, _ := store.snapshot(); len(global.Models) != 1 {
		t.Fatalf("non-empty storeGlobal did not persist: %+v", global)
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
	codex.putDir("/proj/a", func(e *catalogEntry) { e.Modes = []agent.SessionMode{{ID: "agent"}} })

	oc, err := newCatalogStore(dir, agent.BackendOpenCode)
	if err != nil {
		t.Fatalf("newCatalogStore: %v", err)
	}
	if _, dirs := oc.snapshot(); len(dirs) != 0 {
		t.Errorf("opencode store read codex's catalog: %+v", dirs)
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
	if _, dirs := s.snapshot(); len(dirs) != 0 {
		t.Fatalf("corrupt file yielded %+v, want an empty catalog", dirs)
	}

	s.putDir("/proj/a", func(e *catalogEntry) { e.Models = []agent.ModelInfo{{ID: "gpt-5.2-codex"}} })
	reopened, err := newCatalogStore(dir, agent.BackendCodex)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, dirs := reopened.snapshot(); len(dirs["/proj/a"].Models) != 1 {
		t.Errorf("catalog did not recover after a corrupt read: %+v", dirs)
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
	s.putDir("/proj/a", func(e *catalogEntry) { e.Modes = []agent.SessionMode{{ID: "plan"}} })

	reopened, err := newCatalogStore(dir, agent.BackendClaudeCode)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, dirs := reopened.snapshot(); len(dirs["/proj/a"].Modes) != 1 {
		t.Errorf("first write to a missing dir was lost: %+v", dirs)
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
