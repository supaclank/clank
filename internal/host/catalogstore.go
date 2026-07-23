package host

import (
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/acksell/clank/internal/agent"
)

// catalogEntry is one project dir's agent-advertised pickers.
type catalogEntry struct {
	Models []agent.ModelInfo   `json:"models,omitempty"`
	Modes  []agent.SessionMode `json:"modes,omitempty"`
}

// catalogStore persists one backend's per-project-dir catalog so a picker
// fills instantly for any dir this host has already seen — across folder
// switches and daemon restarts. Purely derived data: deleting the file
// costs one reprobe per dir and nothing else.
//
// Freshness needs no TTL or background refresh: every real session open
// republishes its dir's catalog through the same sinks, so a project that
// gains an agent or provider corrects itself the next time it is used.
type catalogStore struct {
	path string
	mu   sync.Mutex
	data map[string]catalogEntry
}

// newCatalogStore opens the catalog for one backend under dir, reading
// whatever a previous run persisted.
func newCatalogStore(dir string, backend agent.BackendType) (*catalogStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("catalog dir is required")
	}
	s := &catalogStore{
		path: filepath.Join(dir, string(backend)+".json"),
		data: map[string]catalogEntry{},
	}
	s.load()
	return s, nil
}

// load reads the persisted catalog. An unreadable or corrupt file leaves
// the store empty rather than failing the daemon: the catalog is a cache,
// and an empty one only costs a reprobe.
func (s *catalogStore) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("acp catalog: read %s: %v (starting empty)", s.path, err)
		}
		return
	}
	var data map[string]catalogEntry
	if err := json.Unmarshal(raw, &data); err != nil {
		log.Printf("acp catalog: parse %s: %v (starting empty)", s.path, err)
		return
	}
	s.data = data
}

// all returns every persisted entry, deep-copied so the caller's maps
// never share backing arrays with the store.
func (s *catalogStore) all() map[string]catalogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]catalogEntry, len(s.data))
	for dir, e := range s.data {
		out[dir] = catalogEntry{Models: slices.Clone(e.Models), Modes: slices.Clone(e.Modes)}
	}
	return out
}

// put applies mutate to one dir's entry and rewrites the file.
func (s *catalogStore) put(workDir string, mutate func(*catalogEntry)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.data[workDir]
	mutate(&entry)
	s.data[workDir] = entry
	if err := s.save(); err != nil {
		log.Printf("acp catalog: write %s: %v", s.path, err)
	}
}

// save rewrites the catalog through a temp file so a crash mid-write
// leaves the previous catalog intact instead of a truncated one.
func (s *catalogStore) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// seed copies the persisted catalog into the manager's in-memory maps and
// marks those dirs probed, so a known dir answers without opening a session.
func (m *ACPBackendManager) seed(persisted map[string]catalogEntry) {
	for dir, entry := range persisted {
		if len(entry.Models) > 0 {
			m.catalog[dir] = entry.Models
		}
		if len(entry.Modes) > 0 {
			m.modes[dir] = entry.Modes
		}
		m.probed[dir] = true
	}
}

// persistedDirs reports the project dirs the store answers for, in sorted
// order. Test-facing; the manager itself reads from its seeded maps.
func (s *catalogStore) persistedDirs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.data))
}
