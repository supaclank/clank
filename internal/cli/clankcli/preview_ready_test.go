package clankcli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
	daemonclient "github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/host"
	hostmux "github.com/supaclank/clank/internal/host/mux"
	"github.com/supaclank/clank/internal/host/preview"
)

func TestWaitPreviewReadyStreamsDevServerLogs(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	projectDir := t.TempDir()
	launchDir := filepath.Join(projectDir, ".clank")
	if err := os.MkdirAll(launchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := `default: web
previews:
  web:
    directory: .
    command: printf 'installing dependencies\n'; sleep 0.5; exec python3 -m http.server "$PORT"
    ready:
      path: /
      expect: Directory listing
`
	if err := os.WriteFile(filepath.Join(launchDir, "launch.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := host.New(host.Options{BackendManagers: map[agent.BackendType]agent.BackendManager{}})
	t.Cleanup(svc.Shutdown)
	server := httptest.NewServer(hostmux.New(svc, nil).Handler())
	t.Cleanup(server.Close)

	client := daemonclient.NewTCPClient(server.URL, "")
	pv := client.Preview(host.LocalRepoSlug(projectDir)).Named("web")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := pv.Start(ctx)
	if err != nil {
		t.Fatalf("start preview: %v", err)
	}
	var out bytes.Buffer
	if _, err := waitPreviewReady(ctx, pv, status, 4*time.Second, &out); err != nil {
		t.Fatalf("waitPreviewReady: %v", err)
	}
	if !strings.Contains(out.String(), "installing dependencies") {
		t.Fatalf("startup output did not include dev-server progress:\n%s", out.String())
	}
	if strings.Count(out.String(), "installing dependencies") != 1 {
		t.Fatalf("startup progress was duplicated:\n%s", out.String())
	}
	if !strings.Contains(out.String(), previewLogHeader) {
		t.Fatalf("startup output did not identify the dev-server stream:\n%s", out.String())
	}
}

func TestWaitPreviewReadyAlreadyReadyIsSilent(t *testing.T) {
	t.Parallel()
	status := &daemonclient.PreviewStatus{State: string(preview.StateReady)}
	var out bytes.Buffer
	got, err := waitPreviewReady(context.Background(), nil, status, time.Second, &out)
	if err != nil {
		t.Fatalf("waitPreviewReady: %v", err)
	}
	if got != status {
		t.Fatalf("status = %p, want original %p", got, status)
	}
	if out.Len() != 0 {
		t.Fatalf("already-ready preview dumped historical logs:\n%s", out.String())
	}
}

func TestPreviewReadyState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  *daemonclient.PreviewStatus
		isReady bool
		wantErr string
	}{
		{name: "ready", status: &daemonclient.PreviewStatus{State: string(preview.StateReady)}, isReady: true},
		{name: "starting", status: &daemonclient.PreviewStatus{State: string(preview.StateStarting)}},
		{name: "failed with cause", status: &daemonclient.PreviewStatus{State: string(preview.StateFailed), LastError: "process exited"}, wantErr: "process exited"},
		{name: "failed without cause", status: &daemonclient.PreviewStatus{State: string(preview.StateFailed)}, wantErr: "failed during startup"},
		{name: "stopped", status: &daemonclient.PreviewStatus{State: string(preview.StateStopped)}, wantErr: "stopped during startup"},
		{name: "unknown", status: &daemonclient.PreviewStatus{State: "mystery"}, wantErr: "unknown startup state"},
		{name: "nil", wantErr: "no status"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := previewReadyState(tt.status)
			if got != tt.isReady {
				t.Errorf("isReady = %t, want %t", got, tt.isReady)
			}
			if tt.wantErr == "" && err != nil {
				t.Fatalf("error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
