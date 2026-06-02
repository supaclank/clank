package pushlock

import "testing"

// TestAcquire_SerializesConcurrentPushes covers the many-idles case: a
// second push for the same worktree is refused (no error) while the
// first holds the lock, and succeeds again after release. flock treats
// the two open file descriptions independently even in one process, so
// this exercises the real cross-process behavior.
func TestAcquire_SerializesConcurrentPushes(t *testing.T) {
	t.Parallel()
	gitDir := t.TempDir()

	ok, release, err := Acquire(gitDir)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if !ok {
		t.Fatal("first Acquire should succeed")
	}

	ok2, release2, err := Acquire(gitDir)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if ok2 {
		release2()
		t.Fatal("second Acquire should be refused while the first holds the lock")
	}

	release()

	ok3, release3, err := Acquire(gitDir)
	if err != nil {
		t.Fatalf("third Acquire: %v", err)
	}
	if !ok3 {
		t.Fatal("Acquire should succeed after release")
	}
	release3()
}

func TestAcquire_DistinctWorktreesDontBlock(t *testing.T) {
	t.Parallel()
	a, b := t.TempDir(), t.TempDir()

	ok1, r1, err := Acquire(a)
	if err != nil || !ok1 {
		t.Fatalf("acquire a: ok=%v err=%v", ok1, err)
	}
	defer r1()

	ok2, r2, err := Acquire(b)
	if err != nil || !ok2 {
		t.Fatalf("acquire b should not be blocked by a: ok=%v err=%v", ok2, err)
	}
	defer r2()
}
