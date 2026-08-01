package clankcli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePreviewAttachArgs(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	absProjectDir, err := filepath.Abs(projectDir)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		args       []string
		wantURL    string
		wantErr    string
		wantAttach bool
	}{
		{
			name:       "folder then URL",
			args:       []string{projectDir, "http://127.0.0.1:5173"},
			wantURL:    "http://127.0.0.1:5173",
			wantAttach: true,
		},
		{
			name:       "URL then folder",
			args:       []string{"http://127.0.0.1:5173", projectDir},
			wantURL:    "http://127.0.0.1:5173",
			wantAttach: true,
		},
		{
			name:       "port shorthand uses IPv4 loopback",
			args:       []string{projectDir, ":5173"},
			wantURL:    "http://127.0.0.1:5173",
			wantAttach: true,
		},
		{
			name:       "explicit localhost is preserved",
			args:       []string{projectDir, "http://localhost:5173/"},
			wantURL:    "http://localhost:5173",
			wantAttach: true,
		},
		{
			name:       "IPv6 loopback is accepted",
			args:       []string{projectDir, "http://[::1]:5173"},
			wantURL:    "http://[::1]:5173",
			wantAttach: true,
		},
		{
			name:    "remote HTTP origins are rejected",
			args:    []string{projectDir, "https://preview.example.test:8443"},
			wantErr: "loopback host",
		},
		{
			name:    "URL alone requires project context",
			args:    []string{"http://127.0.0.1:5173"},
			wantErr: "folder is required",
		},
		{
			name:    "port alone requires project context",
			args:    []string{":5173"},
			wantErr: "folder is required",
		},
		{
			name:    "both arguments cannot be targets",
			args:    []string{":5173", ":4173"},
			wantErr: "exactly one folder",
		},
		{
			name:    "two folders are not an attach invocation",
			args:    []string{projectDir, projectDir},
			wantErr: "URL or :port",
		},
		{
			name:    "missing folder is rejected",
			args:    []string{filepath.Join(projectDir, "missing"), ":5173"},
			wantErr: "preview folder",
		},
		{
			name:    "URL paths are rejected",
			args:    []string{projectDir, "http://127.0.0.1:5173/dashboard"},
			wantErr: "origin only",
		},
		{
			name:    "unsupported URL scheme is rejected",
			args:    []string{projectDir, "ftp://127.0.0.1:5173"},
			wantErr: "http or https",
		},
		{
			name:    "invalid shorthand port is rejected",
			args:    []string{projectDir, ":70000"},
			wantErr: "port",
		},
		{
			name: "no args select no attach",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parsePreviewAttachArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parsePreviewAttachArgs(%q): err = %v, want containing %q", tt.args, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePreviewAttachArgs(%q): %v", tt.args, err)
			}
			if !tt.wantAttach {
				if got != nil {
					t.Fatalf("parsePreviewAttachArgs(%q) = %#v, want no attach", tt.args, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("parsePreviewAttachArgs(%q) = nil, want attach", tt.args)
			}
			if got.ProjectDir != absProjectDir {
				t.Errorf("ProjectDir = %q, want %q", got.ProjectDir, absProjectDir)
			}
			if got.UpstreamURL.String() != tt.wantURL {
				t.Errorf("UpstreamURL = %q, want %q", got.UpstreamURL, tt.wantURL)
			}
		})
	}
}
