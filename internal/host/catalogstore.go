package host

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"github.com/acksell/clank/internal/agent"
)

// catalogEntry is one scope's agent-advertised pickers.
type catalogEntry struct {
	Models []agent.ModelInfo   `json:"models,omitempty"`
	Modes  []agent.SessionMode `json:"modes,omitempty"`
}

func (e catalogEntry) clone() catalogEntry {
	return catalogEntry{Models: slices.Clone(e.Models), Modes: slices.Clone(e.Modes)}
}

// catalogFile is the on-disk shape: one backend-global entry (the neutral
// prewarm) plus per-project-dir entries (folder probes and real sessions).
type catalogFile struct {
	Global *catalogEntry           `json:"global,omitempty"`
	Dirs   map[string]catalogEntry `json:"dirs,omitempty"`
}

// catalogStore persists one backend's catalog so a picker fills instantly
// for any dir this host has already answered for — across folder switches
// and process restarts. Purely derived data: deleting the file costs one
// reprobe and nothing else.
//
// Freshness needs no TTL or background refresh: every real session open
// republishes its dir's catalog through the same sinks, so a project that
// gains an agent or provider corrects itself the next time it is used.
type catalogStore struct {
	path string
	mu   sync.Mutex
	data catalogFile
	// lastSaved is the marshaled bytes of the last successful write, so an
	// unchanged save (every resume Open republishes byte-identical catalog)
	// skips the disk write on the session-open path.
	lastSaved []byte
}

// newCatalogStore opens the catalog for one backend under dir, reading
// whatever a previous run persisted.
func newCatalogStore(dir string, backend agent.BackendType) (*catalogStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("catalog dir is required")
	}
	s := &catalogStore{
		path: filepath.Join(dir, string(backend)+".json"),
		data: catalogFile{Dirs: map[string]catalogEntry{}},
	}
	s.load()
	return s, nil
}

// load reads the persisted catalog. An unreadable or corrupt file leaves
// the store empty rather than failing: the catalog is a cache, and an
// empty one only costs a reprobe.
func (s *catalogStore) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("acp catalog: read %s: %v (starting empty)", s.path, err)
		}
		return
	}
	var data catalogFile
	if err := json.Unmarshal(raw, &data); err != nil {
		log.Printf("acp catalog: parse %s: %v (starting empty)", s.path, err)
		return
	}
	if data.Dirs == nil {
		data.Dirs = map[string]catalogEntry{}
	}
	s.data = data
	// Seed lastSaved with the canonical marshaling so the first no-change
	// save after a restart is a no-op, not a redundant rewrite.
	if canonical, err := json.Marshal(s.data); err == nil {
		s.lastSaved = canonical
	}
}

// snapshot deep-copies the persisted catalog for seeding the manager's
// in-memory maps; the returned values share no backing arrays with the store.
func (s *catalogStore) snapshot() (global catalogEntry, dirs map[string]catalogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Global != nil {
		global = s.data.Global.clone()
	}
	dirs = make(map[string]catalogEntry, len(s.data.Dirs))
	for dir, e := range s.data.Dirs {
		dirs[dir] = e.clone()
	}
	return global, dirs
}

// putDir applies mutate to one dir's entry and rewrites the file.
func (s *catalogStore) putDir(workDir string, mutate func(*catalogEntry)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.data.Dirs[workDir]
	mutate(&entry)
	s.data.Dirs[workDir] = entry
	s.save()
}

// putGlobal applies mutate to the backend-global entry and rewrites the file.
func (s *catalogStore) putGlobal(mutate func(*catalogEntry)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := catalogEntry{}
	if s.data.Global != nil {
		entry = *s.data.Global
	}
	mutate(&entry)
	s.data.Global = &entry
	s.save()
}

// save rewrites the catalog through a temp file so a crash mid-write
// leaves the previous catalog intact instead of a truncated one. Caller
// holds s.mu.
func (s *catalogStore) save() {
	raw, err := json.Marshal(s.data)
	if err != nil {
		log.Printf("acp catalog: marshal %s: %v", s.path, err)
		return
	}
	if bytes.Equal(raw, s.lastSaved) {
		return // Unchanged since the last write — skip redundant disk I/O.
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		log.Printf("acp catalog: mkdir %s: %v", filepath.Dir(s.path), err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		log.Printf("acp catalog: write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("acp catalog: rename %s: %v", s.path, err)
		return
	}
	s.lastSaved = raw
}

// seed copies the persisted catalog into the manager's in-memory state
// and marks known dirs probed, so a warm dir answers without a session.
func (m *ACPBackendManager) seed(global catalogEntry, dirs map[string]catalogEntry) {
	m.globalModels = global.Models
	m.globalModes = global.Modes
	for dir, entry := range dirs {
		if len(entry.Models) > 0 {
			m.catalog[dir] = entry.Models
		}
		if len(entry.Modes) > 0 {
			m.modes[dir] = entry.Modes
		}
		m.probed[dir] = true
	}
}

// persistedDirs reports the project dirs the store answers for, sorted.
// Test-facing; the manager reads from its seeded maps.
func (s *catalogStore) persistedDirs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	dirs := make([]string, 0, len(s.data.Dirs))
	for dir := range s.data.Dirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}
