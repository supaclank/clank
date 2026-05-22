package petname

import (
	"regexp"
	"testing"
)

func TestGenerate_Shape(t *testing.T) {
	t.Parallel()

	re := regexp.MustCompile(`^[a-z]+-[a-z]+-[0-9a-f]{4}$`)
	for i := 0; i < 100; i++ {
		name := Generate()
		if !re.MatchString(name) {
			t.Fatalf("petname %q does not match expected shape adjective-animal-hex4", name)
		}
	}
}

func TestGenerate_LowCollisionRate(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{})
	const n = 1000
	collisions := 0
	for i := 0; i < n; i++ {
		name := Generate()
		if _, ok := seen[name]; ok {
			collisions++
		}
		seen[name] = struct{}{}
	}
	// 47 adjectives * 50 animals * 65536 suffixes = ~150M possibilities;
	// across 1000 draws the expected collisions are essentially zero. Allow
	// a tiny tolerance just in case.
	if collisions > 1 {
		t.Fatalf("too many collisions in %d draws: %d", n, collisions)
	}
}
