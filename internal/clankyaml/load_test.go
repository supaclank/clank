package clankyaml

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		wantErr string // substring of the error; empty = success
		want    func(t *testing.T, f *File)
	}{
		{
			name: "full preview section",
			yaml: `
preview:
  dir: web-app
  install: pnpm install
  command: pnpm dev --port ${PORT}
  ready:
    path: /healthz
    expect: ok
`,
			want: func(t *testing.T, f *File) {
				p := f.Preview
				if p.Dir != "web-app" || p.Install != "pnpm install" {
					t.Errorf("dir/install = %q/%q", p.Dir, p.Install)
				}
				if p.Command != "pnpm dev --port ${PORT}" {
					t.Errorf("command = %q", p.Command)
				}
				if p.Ready == nil || p.Ready.Path != "/healthz" || p.Ready.Expect != "ok" {
					t.Errorf("ready = %+v", p.Ready)
				}
			},
		},
		{
			name: "dir only",
			yaml: "preview:\n  dir: web-app\n",
			want: func(t *testing.T, f *File) {
				if f.Preview.Dir != "web-app" || f.Preview.Command != "" {
					t.Errorf("preview = %+v", f.Preview)
				}
			},
		},
		{
			name: "unknown top-level section tolerated",
			yaml: "agent:\n  backend: claude\npreview:\n  install: npm install\n",
			want: func(t *testing.T, f *File) {
				if f.Preview.Install != "npm install" {
					t.Errorf("install = %q", f.Preview.Install)
				}
			},
		},
		{
			name: "no preview section",
			yaml: "agent:\n  backend: claude\n",
			want: func(t *testing.T, f *File) {
				if f.Preview != nil {
					t.Errorf("preview = %+v, want nil", f.Preview)
				}
			},
		},
		{
			name:    "unknown key inside preview errors",
			yaml:    "preview:\n  comand: pnpm dev --port ${PORT}\n",
			wantErr: "comand",
		},
		{
			name:    "unknown key inside ready errors",
			yaml:    "preview:\n  ready:\n    path: /\n    timeout: 5\n",
			wantErr: "timeout",
		},
		{
			name:    "command without port placeholder",
			yaml:    "preview:\n  command: pnpm dev --port 3000\n",
			wantErr: PortPlaceholder,
		},
		{
			name:    "absolute dir rejected",
			yaml:    "preview:\n  dir: /etc\n",
			wantErr: "preview.dir",
		},
		{
			name:    "escaping dir rejected",
			yaml:    "preview:\n  dir: ../sibling\n",
			wantErr: "preview.dir",
		},
		{
			name:    "ready without path rejected",
			yaml:    "preview:\n  ready:\n    expect: ok\n",
			wantErr: "ready.path",
		},
		{
			name:    "ready path without leading slash rejected",
			yaml:    "preview:\n  ready:\n    path: healthz\n",
			wantErr: "must start with /",
		},
		{
			name:    "malformed yaml",
			yaml:    "preview: [unclosed\n",
			wantErr: "parse",
		},
		{
			name:    "wrong type for section",
			yaml:    "preview: just-a-string\n",
			wantErr: "parse",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, err := parse([]byte(tt.yaml))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parse succeeded, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			tt.want(t, f)
		})
	}
}

func TestParseEmptyFile(t *testing.T) {
	t.Parallel()
	f, err := parse(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f != nil {
		t.Fatalf("empty file parsed to %+v, want nil (same as absent)", f)
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()

	t.Run("absent file is nil, nil", func(t *testing.T) {
		t.Parallel()
		f, err := Load(t.TempDir())
		if err != nil || f != nil {
			t.Fatalf("Load = %+v, %v; want nil, nil", f, err)
		}
	})

	t.Run("present file parses", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte("preview:\n  dir: app\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := Load(dir)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if f.Preview.Dir != "app" {
			t.Errorf("dir = %q", f.Preview.Dir)
		}
	})

	t.Run("invalid file names the path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte("preview:\n  command: no-placeholder\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(dir)
		if err == nil || !strings.Contains(err.Error(), FileName) {
			t.Fatalf("Load error = %v, want mention of %s", err, FileName)
		}
	})
}
