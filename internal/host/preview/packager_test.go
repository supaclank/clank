package preview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree creates files under root; keys are slash paths, values
// contents. Parent dirs (including empty dirs for "" values ending in
// /) are created as needed.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolvePackager(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// files under the repo root; ".git/" is added automatically.
		files map[string]string
		// dir to resolve from, relative to root ("" = root itself).
		from         string
		want         Packager
		wantEvidence string // substring; "" asserts evidence is empty
		wantErr      string
	}{
		{
			name:  "no signal defaults to bun",
			files: map[string]string{"package.json": `{"name":"x"}`},
			want:  PackagerBun,
		},
		{
			name:    "unreadable package.json errors instead of defaulting to bun",
			files:   map[string]string{"package.json/": ""}, // a directory, not a file: os.ReadFile fails with a non-not-exist error
			wantErr: "package.json",
		},
		{
			name: "packageManager field wins over lockfile",
			files: map[string]string{
				"package.json":      `{"packageManager":"pnpm@9.1.0"}`,
				"package-lock.json": "{}",
			},
			want:         PackagerPNPM,
			wantEvidence: `packageManager "pnpm@9.1.0"`,
		},
		{
			name: "packageManager with hash suffix",
			files: map[string]string{
				"package.json": `{"packageManager":"yarn@4.1.0+sha256.abcdef"}`,
			},
			want:         PackagerYarn,
			wantEvidence: "packageManager",
		},
		{
			name: "devEngines object shape",
			files: map[string]string{
				"package.json": `{"devEngines":{"packageManager":{"name":"npm","version":"^10"}}}`,
			},
			want:         PackagerNPM,
			wantEvidence: `devEngines.packageManager "npm"`,
		},
		{
			name: "devEngines array shape",
			files: map[string]string{
				"package.json": `{"devEngines":{"packageManager":[{"name":"pnpm"}]}}`,
			},
			want:         PackagerPNPM,
			wantEvidence: "devEngines",
		},
		{
			name:         "pnpm lockfile",
			files:        map[string]string{"pnpm-lock.yaml": ""},
			want:         PackagerPNPM,
			wantEvidence: "pnpm-lock.yaml",
		},
		{
			name:         "yarn lockfile",
			files:        map[string]string{"yarn.lock": ""},
			want:         PackagerYarn,
			wantEvidence: "yarn.lock",
		},
		{
			name:         "npm lockfile",
			files:        map[string]string{"package-lock.json": "{}"},
			want:         PackagerNPM,
			wantEvidence: "package-lock.json",
		},
		{
			name: "bun.lock beats package-lock in same dir",
			files: map[string]string{
				"bun.lock":          "",
				"package-lock.json": "{}",
			},
			want:         PackagerBun,
			wantEvidence: "bun.lock",
		},
		{
			name:         "binary bun lockfile",
			files:        map[string]string{"bun.lockb": ""},
			want:         PackagerBun,
			wantEvidence: "bun.lockb",
		},
		{
			name: "walk-up finds monorepo root lockfile",
			files: map[string]string{
				"pnpm-lock.yaml":       "",
				"web-app/package.json": `{"name":"web"}`,
			},
			from:         "web-app",
			want:         PackagerPNPM,
			wantEvidence: "pnpm-lock.yaml in ..",
		},
		{
			name: "nearest dir wins over root",
			files: map[string]string{
				"pnpm-lock.yaml":     "",
				"web-app/yarn.lock":  "",
				"web-app/index.html": "",
			},
			from:         "web-app",
			want:         PackagerYarn,
			wantEvidence: "yarn.lock",
		},
		{
			name: "malformed package.json is no signal",
			files: map[string]string{
				"package.json": "{not json",
				"yarn.lock":    "",
			},
			want:         PackagerYarn,
			wantEvidence: "yarn.lock",
		},
		{
			name: "unsupported packageManager errors",
			files: map[string]string{
				"package.json": `{"packageManager":"moon@1.0.0"}`,
			},
			wantErr: "moon@1.0.0",
		},
		{
			name: "unsupported devEngines manager errors",
			files: map[string]string{
				"package.json": `{"devEngines":{"packageManager":{"name":"vlt"}}}`,
			},
			wantErr: "vlt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeTree(t, root, tt.files)
			writeTree(t, root, map[string]string{".git/": ""})

			from := root
			if tt.from != "" {
				from = filepath.Join(root, tt.from)
			}
			pm, evidence, err := ResolvePackager(from)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolvePackager: %v", err)
			}
			if pm != tt.want {
				t.Errorf("packager = %s, want %s (evidence %q)", pm, tt.want, evidence)
			}
			if tt.wantEvidence == "" {
				if evidence != "" {
					t.Errorf("evidence = %q, want empty (default)", evidence)
				}
			} else if !strings.Contains(evidence, tt.wantEvidence) {
				t.Errorf("evidence = %q, want containing %q", evidence, tt.wantEvidence)
			}
		})
	}
}

// TestResolvePackagerStopsAtRepoRoot pins the walk-up boundary: a
// lockfile in a directory ABOVE the repo's .git root must not leak
// into detection (e.g. a stray yarn.lock in $HOME).
func TestResolvePackagerStopsAtRepoRoot(t *testing.T) {
	t.Parallel()
	outer := t.TempDir()
	writeTree(t, outer, map[string]string{
		"yarn.lock":                 "",
		"repo/.git/":                "",
		"repo/web-app/package.json": `{"name":"web"}`,
	})
	pm, evidence, err := ResolvePackager(filepath.Join(outer, "repo", "web-app"))
	if err != nil {
		t.Fatalf("ResolvePackager: %v", err)
	}
	if pm != PackagerBun || evidence != "" {
		t.Errorf("packager = %s (%q), want default bun with no evidence", pm, evidence)
	}
}
