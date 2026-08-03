package host

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/agent/presets"
)

// presetStore persists USER-created presets as one JSON file under the
// host's data dir. Built-ins never live here: they arrive per boot from
// $CLANK_BUILTIN_PRESETS (the provisioner's declaration) and are merged
// read-only at the service layer — so a clank upgrade updates built-ins
// without a migration, and user edits can never be clobbered by one.
type presetStore struct {
	path string
	mu   sync.Mutex
	byID map[string]presets.Preset
}

func newPresetStore(dir string) (*presetStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("preset store dir is required")
	}
	s := &presetStore{path: filepath.Join(dir, "presets.json"), byID: map[string]presets.Preset{}}
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read presets: %w", err)
	}
	var list []presets.Preset
	if err := json.Unmarshal(raw, &list); err != nil {
		// User data, not a cache: refuse to run rather than silently
		// starting empty and overwriting their presets on the next save.
		return nil, fmt.Errorf("parse %s: %w (fix or remove the file)", s.path, err)
	}
	for _, p := range list {
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("%s: preset %q: %w (fix or remove the file)", s.path, p.ID, err)
		}
		if isBuiltinID(p.ID) || p.Builtin {
			return nil, fmt.Errorf("%s: preset %q: built-ins can't live in the user store (fix or remove the file)", s.path, p.ID)
		}
		if _, dup := s.byID[p.ID]; dup {
			return nil, fmt.Errorf("%s: preset %q: duplicate id (fix or remove the file)", s.path, p.ID)
		}
		s.byID[p.ID] = p
	}
	return s, nil
}

// isBuiltinID reports whether id is reserved for built-in presets.
func isBuiltinID(id string) bool {
	return strings.HasPrefix(id, presets.BuiltinDefaultPrefix) || strings.HasPrefix(id, presets.BuiltinPlanPrefix)
}

// List returns user presets, sorted by ID for stable output.
func (s *presetStore) List() []presets.Preset {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]presets.Preset, 0, len(s.byID))
	for _, p := range s.byID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Put creates or replaces a user preset. Built-in ids are reserved.
func (s *presetStore) Put(p presets.Preset) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if isBuiltinID(p.ID) || p.Builtin {
		return fmt.Errorf("preset %q: built-in presets are immutable — duplicate under a new id to customize", p.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[p.ID] = p
	return s.save()
}

// Delete removes a user preset. Unknown ids error (fail fast, no silent
// no-op); built-ins can't be here by construction.
func (s *presetStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return fmt.Errorf("preset %q not found", id)
	}
	delete(s.byID, id)
	return s.save()
}

// save rewrites the file through a temp rename so a crash mid-write leaves
// the previous contents intact. Caller holds s.mu.
func (s *presetStore) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	list := make([]presets.Preset, 0, len(s.byID))
	for _, p := range s.byID {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Presets returns built-ins plus user presets, optionally filtered by
// backend. Built-ins first, so clients render them at the top.
func (s *Service) Presets(bt agent.BackendType) []presets.Preset {
	out := make([]presets.Preset, 0, len(s.builtinPresets)+8)
	for _, p := range s.builtinPresets {
		if bt == "" || p.Backend == bt {
			out = append(out, p)
		}
	}
	if s.presetStore != nil {
		for _, p := range s.presetStore.List() {
			if bt == "" || p.Backend == bt {
				out = append(out, p)
			}
		}
	}
	return out
}

// ValidateCreateConfig enforces the create-time config contract: every
// key of the backend's built-in Default preset must be present. The host
// never fills values in — a missing key is the client's bug, surfaced
// loudly (400) instead of a hidden substitution. Values are not checked
// here: the agent owns its vocabulary and skips ids it doesn't advertise.
func (s *Service) ValidateCreateConfig(bt agent.BackendType, cfg map[string]string) error {
	required := presets.RequiredKeys(s.builtinPresets, bt)
	var missing []string
	for _, k := range required {
		if cfg[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("%w: config is missing required keys %v for backend %s (take them from the backend's Default preset via GET /presets)", ErrConfigIncomplete, missing, bt)
	}
	return nil
}

// PutPreset stores a user preset.
func (s *Service) PutPreset(p presets.Preset) error {
	if s.presetStore == nil {
		return ErrPresetStoreUnavailable
	}
	return s.presetStore.Put(p)
}

// DeletePreset removes a user preset.
func (s *Service) DeletePreset(id string) error {
	if s.presetStore == nil {
		return ErrPresetStoreUnavailable
	}
	return s.presetStore.Delete(id)
}
