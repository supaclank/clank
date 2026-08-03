package clankcli

import (
	"strings"
	"testing"

	daemonclient "github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/host/preview"
)

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
