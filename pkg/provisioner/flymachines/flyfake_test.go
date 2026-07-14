package flymachines

// fakeFly is an in-memory Fly Machines API for regression tests. Repo
// rule: no mocks — this is a real HTTP server speaking the flaps REST
// protocol, exercised through the real flaps client (pointed here via
// FLY_FLAPS_BASE_URL, which flaps.NewWithOptions reads at construction).
// It mirrors the Fly behaviors the provisioner's races hang on: volume
// IDs are server-assigned and volume names are NOT unique; machine
// names ARE unique (duplicate launch → "already_exists"); app names are
// unique. Counters record the calls the regression tests assert on.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	fly "github.com/superfly/fly-go"
	"github.com/superfly/fly-go/flaps"
)

type fakeApp struct {
	network  string
	ips      []string
	volumes  []fly.Volume
	machines []*fly.Machine
}

type fakeFly struct {
	srv *httptest.Server

	mu     sync.Mutex
	apps   map[string]*fakeApp
	nextID int

	volumeCreates   int
	volumeDeletes   int
	machineLaunches int // successful launches only
	machineUpdates  int
}

func newFakeFly(t *testing.T) *fakeFly {
	t.Helper()
	f := &fakeFly{apps: map[string]*fakeApp{}}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/apps", func(w http.ResponseWriter, r *http.Request) {
		var in flaps.CreateAppRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, exists := f.apps[in.Name]; exists {
			writeFlapsErr(w, http.StatusUnprocessableEntity, "name has already been taken")
			return
		}
		f.apps[in.Name] = &fakeApp{network: in.Network}
		writeJSON(w, flaps.App{Name: in.Name, Network: in.Network})
	})

	mux.HandleFunc("GET /v1/apps/{app}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		app, ok := f.apps[r.PathValue("app")]
		if !ok {
			writeFlapsErr(w, http.StatusNotFound, "app not found")
			return
		}
		writeJSON(w, flaps.App{Name: r.PathValue("app"), Network: app.network})
	})

	mux.HandleFunc("DELETE /v1/apps/{app}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.apps, r.PathValue("app"))
		w.WriteHeader(http.StatusAccepted)
	})

	mux.HandleFunc("GET /v1/apps/{app}/ip_assignments", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		app, ok := f.apps[r.PathValue("app")]
		if !ok {
			writeFlapsErr(w, http.StatusNotFound, "app not found")
			return
		}
		resp := flaps.ListIPAssignmentsResponse{}
		for _, ip := range app.ips {
			resp.IPs = append(resp.IPs, flaps.IPAssignment{IP: ip})
		}
		writeJSON(w, resp)
	})

	mux.HandleFunc("POST /v1/apps/{app}/ip_assignments", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		app, ok := f.apps[r.PathValue("app")]
		if !ok {
			writeFlapsErr(w, http.StatusNotFound, "app not found")
			return
		}
		f.nextID++
		ip := fmt.Sprintf("fdaa:0:fa4e::%d", f.nextID)
		app.ips = append(app.ips, ip)
		writeJSON(w, flaps.IPAssignment{IP: ip})
	})

	mux.HandleFunc("GET /v1/apps/{app}/volumes", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		app, ok := f.apps[r.PathValue("app")]
		if !ok {
			writeFlapsErr(w, http.StatusNotFound, "app not found")
			return
		}
		vols := app.volumes
		if vols == nil {
			vols = []fly.Volume{}
		}
		writeJSON(w, vols)
	})

	mux.HandleFunc("POST /v1/apps/{app}/volumes", func(w http.ResponseWriter, r *http.Request) {
		var in fly.CreateVolumeRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		app, ok := f.apps[r.PathValue("app")]
		if !ok {
			writeFlapsErr(w, http.StatusNotFound, "app not found")
			return
		}
		// Server-assigned ID; duplicate names allowed — the exact Fly
		// semantics the adopt-or-delete claim exists for.
		f.nextID++
		f.volumeCreates++
		vol := fly.Volume{ID: fmt.Sprintf("vol_fake%d", f.nextID), Name: in.Name, CreatedAt: time.Now()}
		app.volumes = append(app.volumes, vol)
		writeJSON(w, vol)
	})

	mux.HandleFunc("DELETE /v1/apps/{app}/volumes/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		app, ok := f.apps[r.PathValue("app")]
		if !ok {
			writeFlapsErr(w, http.StatusNotFound, "app not found")
			return
		}
		id := r.PathValue("id")
		for i, v := range app.volumes {
			if v.ID == id {
				app.volumes = append(app.volumes[:i], app.volumes[i+1:]...)
				f.volumeDeletes++
				writeJSON(w, v)
				return
			}
		}
		writeFlapsErr(w, http.StatusNotFound, "volume not found")
	})

	mux.HandleFunc("GET /v1/apps/{app}/machines", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		app, ok := f.apps[r.PathValue("app")]
		if !ok {
			writeFlapsErr(w, http.StatusNotFound, "app not found")
			return
		}
		machines := app.machines
		if machines == nil {
			machines = []*fly.Machine{}
		}
		writeJSON(w, machines)
	})

	mux.HandleFunc("POST /v1/apps/{app}/machines", func(w http.ResponseWriter, r *http.Request) {
		var in fly.LaunchMachineInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		app, ok := f.apps[r.PathValue("app")]
		if !ok {
			writeFlapsErr(w, http.StatusNotFound, "app not found")
			return
		}
		for _, m := range app.machines {
			if m.Name == in.Name {
				writeFlapsErr(w, http.StatusUnprocessableEntity,
					fmt.Sprintf("already_exists: unique machine name violation, machine ID %s already exists with name %q", m.ID, m.Name))
				return
			}
		}
		f.nextID++
		f.machineLaunches++
		m := &fly.Machine{
			ID:     fmt.Sprintf("m%dfake", f.nextID),
			Name:   in.Name,
			State:  fly.MachineStateStarted,
			Config: in.Config,
		}
		app.machines = append(app.machines, m)
		writeJSON(w, m)
	})

	mux.HandleFunc("GET /v1/apps/{app}/machines/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if m := f.findMachineLocked(r.PathValue("app"), r.PathValue("id")); m != nil {
			writeJSON(w, m)
			return
		}
		writeFlapsErr(w, http.StatusNotFound, "machine not found")
	})

	// Update — restarts the workload on real Fly; the fake just swaps
	// config and counts, which is exactly what the never-update-started
	// regression tests assert on.
	mux.HandleFunc("POST /v1/apps/{app}/machines/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in fly.LaunchMachineInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if m := f.findMachineLocked(r.PathValue("app"), r.PathValue("id")); m != nil {
			f.machineUpdates++
			m.Config = in.Config
			writeJSON(w, m)
			return
		}
		writeFlapsErr(w, http.StatusNotFound, "machine not found")
	})

	mux.HandleFunc("POST /v1/apps/{app}/machines/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if m := f.findMachineLocked(r.PathValue("app"), r.PathValue("id")); m != nil {
			m.State = fly.MachineStateStopped
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		writeFlapsErr(w, http.StatusNotFound, "machine not found")
	})

	mux.HandleFunc("DELETE /v1/apps/{app}/machines/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		app, ok := f.apps[r.PathValue("app")]
		if !ok {
			writeFlapsErr(w, http.StatusNotFound, "app not found")
			return
		}
		id := r.PathValue("id")
		for i, m := range app.machines {
			if m.ID == id {
				app.machines = append(app.machines[:i], app.machines[i+1:]...)
				writeJSON(w, map[string]bool{"ok": true})
				return
			}
		}
		writeFlapsErr(w, http.StatusNotFound, "machine not found")
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFly) findMachineLocked(appName, machineID string) *fly.Machine {
	app, ok := f.apps[appName]
	if !ok {
		return nil
	}
	for _, m := range app.machines {
		if m.ID == machineID {
			return m
		}
	}
	return nil
}

// setMachineState flips a machine's state out-of-band (simulating
// clank-host's idle self-exit → stopped).
func (f *fakeFly) setMachineState(t *testing.T, appName, machineID, state string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.findMachineLocked(appName, machineID)
	if m == nil {
		t.Fatalf("setMachineState: machine %s not found in %s", machineID, appName)
	}
	m.State = state
}

// snapshot returns the counters under lock.
func (f *fakeFly) snapshot() (volumeCreates, volumeDeletes, machineLaunches, machineUpdates int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.volumeCreates, f.volumeDeletes, f.machineLaunches, f.machineUpdates
}

func (f *fakeFly) volumeCount(appName string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if app, ok := f.apps[appName]; ok {
		return len(app.volumes)
	}
	return 0
}

// addVolume seeds a volume out-of-band (simulating a claimer that
// crashed between CreateVolume and its CAS).
func (f *fakeFly) addVolume(appName, id, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	app, ok := f.apps[appName]
	if !ok {
		app = &fakeApp{}
		f.apps[appName] = app
	}
	app.volumes = append(app.volumes, fly.Volume{ID: id, Name: name, CreatedAt: time.Now()})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeFlapsErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
