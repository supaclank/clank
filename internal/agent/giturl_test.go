package agent

import (
	"strings"
	"testing"
)

// Regression: file://server/share/x and file:///share/x must not collapse
// to the same identity. Previously both returned host="file" and the same
// path, which made remote file URLs from different hosts share one
// identity/clone target.
func TestParseGitURLFilePreservesAuthority(t *testing.T) {
	t.Parallel()
	hostA, pathA, errA := parseGitURL("file://server/share/repo.git")
	if errA != nil {
		t.Fatalf("file with authority failed: %v", errA)
	}
	hostB, pathB, errB := parseGitURL("file:///share/repo.git")
	if errB != nil {
		t.Fatalf("bare file failed: %v", errB)
	}
	if hostA == hostB && pathA == pathB {
		t.Fatalf("file URLs from different authorities collapsed: a=(%q,%q) b=(%q,%q)",
			hostA, pathA, hostB, pathB)
	}
	if hostA != "file://server" {
		t.Errorf("expected file://server host, got %q", hostA)
	}
	if hostB != "file" {
		t.Errorf("expected bare file host, got %q", hostB)
	}
}

// Regression: distinct remotes that sanitize to the same name must get
// distinct clone dirs. Without the hash suffix, case-sensitive paths
// like Foo/Bar and foo/bar collapse to the same dir name and unrelated
// repos reuse one checkout.
func TestCloneDirNameCollisionResistant(t *testing.T) {
	t.Parallel()
	a, err := CloneDirName("https://example.com/Foo/Bar.git")
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := CloneDirName("https://example.com/foo/bar.git")
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if a == b {
		t.Fatalf("case-different paths collapsed to same dir: %q", a)
	}
}

func TestCloneDirNameStable(t *testing.T) {
	t.Parallel()
	// Same canonical input must always yield the same dir name.
	a, err := CloneDirName("https://github.com/acksell/clank.git")
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := CloneDirName("https://github.com/acksell/clank.git")
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if a != b {
		t.Fatalf("non-deterministic: %q vs %q", a, b)
	}
	// SCP-form and HTTPS form of the same repo should produce the same
	// canonical (host, path) pair, hence the same dir name.
	scp, err := CloneDirName("git@github.com:acksell/clank.git")
	if err != nil {
		t.Fatalf("scp: %v", err)
	}
	if scp != a {
		t.Fatalf("scp form %q differs from https form %q", scp, a)
	}
}

func TestCloneDirNameFormat(t *testing.T) {
	t.Parallel()
	name, err := CloneDirName("https://github.com/acksell/clank.git")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.HasPrefix(name, "github.com-acksell-clank-") {
		t.Errorf("unexpected prefix: %q", name)
	}
}
