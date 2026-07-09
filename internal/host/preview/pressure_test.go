package preview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Note: these tests mutate the package-level ioPressurePath, so they
// must not run in parallel with each other or with tests that log
// through ioPressureSuffix.

func TestIOPressureSuffix_ParsesPSILines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "io")
	psi := "some avg10=16.34 avg60=26.70 avg300=9.04 total=30520683\nfull avg10=15.67 avg60=25.71 avg300=8.72 total=29521751\n"
	if err := os.WriteFile(p, []byte(psi), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	orig := ioPressurePath
	ioPressurePath = p
	t.Cleanup(func() { ioPressurePath = orig })

	got := ioPressureSuffix()
	want := ` io_psi="some avg10=16.34 avg60=26.70 avg300=9.04; full avg10=15.67 avg60=25.71 avg300=8.72"`
	if got != want {
		t.Errorf("suffix mismatch:\n got: %s\nwant: %s", got, want)
	}
	if strings.Contains(got, "total=") {
		t.Errorf("cumulative counters should be stripped: %s", got)
	}
}

func TestIOPressureSuffix_MissingFileDegradesToEmpty(t *testing.T) {
	orig := ioPressurePath
	ioPressurePath = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { ioPressurePath = orig })

	if got := ioPressureSuffix(); got != "" {
		t.Errorf("want empty suffix on unreadable PSI file, got %q", got)
	}
}
