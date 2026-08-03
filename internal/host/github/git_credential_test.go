package github

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunGitCredentialHelper_AnswersGet(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store := NewStore(home)
	if err := store.Write(Credentials{AccessToken: "gho_secret"}); err != nil {
		t.Fatalf("store.Write: %v", err)
	}

	var out strings.Builder
	in := strings.NewReader("protocol=https\nhost=github.com\n\n")
	if err := RunGitCredentialHelper("get", in, &out, store, false); err != nil {
		t.Fatalf("RunGitCredentialHelper: %v", err)
	}
	want := "username=x-access-token\npassword=gho_secret\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestRunGitCredentialHelper_AnswersGetFromGhCLI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	putFakeGh(t, `[ "$1 $2" = "auth token" ] && { echo gho_fromcli; exit 0; }; exit 1`)

	var out strings.Builder
	in := strings.NewReader("protocol=https\nhost=github.com\n\n")
	if err := RunGitCredentialHelper("get", in, &out, NewStore(os.Getenv("HOME")), true); err != nil {
		t.Fatalf("RunGitCredentialHelper: %v", err)
	}
	want := "username=x-access-token\npassword=gho_fromcli\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestRunGitCredentialHelper_PrefersStoreOverGhCLI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	putFakeGh(t, `[ "$1 $2" = "auth token" ] && { echo gho_fromcli; exit 0; }; exit 1`)
	store := NewStore(os.Getenv("HOME"))
	if err := store.Write(Credentials{AccessToken: "gho_fromstore"}); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	in := strings.NewReader("protocol=https\nhost=github.com\n\n")
	if err := RunGitCredentialHelper("get", in, &out, store, true); err != nil {
		t.Fatal(err)
	}
	want := "username=x-access-token\npassword=gho_fromstore\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestRunGitCredentialHelper_SilentCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		action string
		input  string
		token  string // "" = leave the store empty
	}{
		{"store action ignored", "store", "protocol=https\nhost=github.com\n\n", "gho_x"},
		{"erase action ignored", "erase", "protocol=https\nhost=github.com\n\n", "gho_x"},
		{"empty action ignored", "", "protocol=https\nhost=github.com\n\n", "gho_x"},
		{"non-github host", "get", "protocol=https\nhost=gitlab.com\n\n", "gho_x"},
		{"non-https protocol", "get", "protocol=ssh\nhost=github.com\n\n", "gho_x"},
		{"disconnected store", "get", "protocol=https\nhost=github.com\n\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := NewStore(t.TempDir())
			if tc.token != "" {
				if err := store.Write(Credentials{AccessToken: tc.token}); err != nil {
					t.Fatalf("store.Write: %v", err)
				}
			}
			var out strings.Builder
			if err := RunGitCredentialHelper(tc.action, strings.NewReader(tc.input), &out, store, false); err != nil {
				t.Fatalf("RunGitCredentialHelper: %v", err)
			}
			if out.String() != "" {
				t.Errorf("output = %q, want empty (silent no-answer)", out.String())
			}
		})
	}
}

func TestRunGitCredentialHelper_MalformedInput(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	var out strings.Builder
	err := RunGitCredentialHelper("get", strings.NewReader("not-a-key-value\n\n"), &out, store, false)
	if err == nil {
		t.Fatal("RunGitCredentialHelper with malformed input: err = nil, want protocol violation")
	}
}

// Non-get actions must stay silent even when git sends stdin the parser
// can't read — malformed input on store/erase should never fail the helper.
func TestRunGitCredentialHelper_NonGetIgnoresMalformedInput(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	var out strings.Builder
	err := RunGitCredentialHelper("store", strings.NewReader("not-a-key-value\n\n"), &out, store, false)
	if err != nil {
		t.Fatalf("RunGitCredentialHelper(store) with malformed input: err = %v, want nil", err)
	}
	if out.String() != "" {
		t.Errorf("output = %q, want empty", out.String())
	}
}

func TestRunGitCredentialHelper_RejectsInvalidTokenCharacters(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		token string
	}{
		{"embedded newline", "gho_evil\nurl=http://attacker.example"},
		{"embedded carriage return", "gho_evil\rurl=http://attacker.example"},
		{"embedded NUL", "gho_evil\x00trailing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := NewStore(t.TempDir())
			if err := store.Write(Credentials{AccessToken: tc.token}); err != nil {
				t.Fatalf("store.Write: %v", err)
			}
			var out strings.Builder
			in := strings.NewReader("protocol=https\nhost=github.com\n\n")
			err := RunGitCredentialHelper("get", in, &out, store, false)
			if err == nil {
				t.Fatal("RunGitCredentialHelper with invalid token characters: err = nil, want error")
			}
			if out.String() != "" {
				t.Errorf("output = %q, want empty (must not leak partial/corrupt token)", out.String())
			}
		})
	}
}

func TestRunGitCredentialHelper_CaseInsensitiveAttrs(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store := NewStore(home)
	if err := store.Write(Credentials{AccessToken: "gho_secret"}); err != nil {
		t.Fatalf("store.Write: %v", err)
	}

	var out strings.Builder
	in := strings.NewReader("protocol=HTTPS\nhost=GitHub.com\n\n")
	if err := RunGitCredentialHelper("get", in, &out, store, false); err != nil {
		t.Fatalf("RunGitCredentialHelper: %v", err)
	}
	want := "username=x-access-token\npassword=gho_secret\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestGitCredentialHelperValue_QuotesPath(t *testing.T) {
	t.Parallel()
	got := GitCredentialHelperValue("/opt/my tools/clank-host", false)
	want := `!"/opt/my tools/clank-host" git-credential`
	if got != want {
		t.Errorf("GitCredentialHelperValue = %q, want %q", got, want)
	}
}

func TestGitCredentialHelperValue_EnablesGhCLIAuth(t *testing.T) {
	t.Parallel()
	got := GitCredentialHelperValue("/opt/clank-host", true)
	want := `!"/opt/clank-host" git-credential --gh-cli-auth`
	if got != want {
		t.Errorf("GitCredentialHelperValue = %q, want %q", got, want)
	}
}

// TestGitCredentialFill_EndToEnd proves GIT itself invokes the helper and
// parses its answer — hermetically, with zero network. It builds the real
// clank-host binary, configures it as a temp repo's credential.helper (the
// exact config P1 writes into canonicals), and runs `git credential fill`,
// which consults configured helpers the same way fetch/push/lazy-fetch do.
func TestGitCredentialFill_EndToEnd(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	// Build the actual binary. `go build` from the package dir keeps this
	// robust to running the test from any working directory.
	bin := filepath.Join(t.TempDir(), "clank-host")
	build := exec.Command("go", "build", "-o", bin, "github.com/supaclank/clank/cmd/clank-host")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build clank-host: %v\n%s", err, out)
	}

	// A connected store under a fake HOME, so the subcommand (which reads
	// os.UserHomeDir) finds it.
	home := t.TempDir()
	if err := NewStore(home).Write(Credentials{AccessToken: "gho_fill_e2e"}); err != nil {
		t.Fatalf("store.Write: %v", err)
	}

	// Temp repo with the helper configured exactly as canonicals get it.
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q")
	mustGit(t, repo, "config", "credential.helper", GitCredentialHelperValue(bin, false))

	fill := exec.Command("git", "credential", "fill")
	fill.Dir = repo
	fill.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	// Isolate from the developer's real helpers/config: fake HOME (also
	// points the subcommand at the test store), no system config, and no
	// terminal prompting so a miss fails instead of hanging.
	fill.Env = append(os.Environ(),
		"HOME="+home,
		"USERPROFILE="+home, // os.UserHomeDir() prefers USERPROFILE on Windows
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
	)
	out, err := fill.CombinedOutput()
	if err != nil {
		t.Fatalf("git credential fill: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "username=x-access-token") || !strings.Contains(got, "password=gho_fill_e2e") {
		t.Errorf("git credential fill output missing helper answer:\n%s", got)
	}
}

// mustGit runs git with args in dir, failing the test on error.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
