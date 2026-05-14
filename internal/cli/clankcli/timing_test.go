package clankcli

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPhaseTimer_DisabledIsNoOp(t *testing.T) {
	t.Parallel()
	pt := newPhaseTimer(false)
	done := pt.Start("anything")
	time.Sleep(2 * time.Millisecond)
	done()
	if len(pt.phases) != 0 {
		t.Errorf("disabled timer recorded %d phases; want 0", len(pt.phases))
	}
	var buf bytes.Buffer
	pt.Summary(&buf)
	if buf.Len() != 0 {
		t.Errorf("disabled timer Summary wrote %q; want empty", buf.String())
	}
}

func TestPhaseTimer_RecordsAndSorts(t *testing.T) {
	t.Parallel()
	pt := newPhaseTimer(true)
	// Record phases in a deliberately not-sorted order. Sleep durations
	// chosen well above OS scheduler jitter so the ordering is stable.
	pt.phases = append(pt.phases,
		phaseEntry{name: "small", duration: 5 * time.Millisecond},
		phaseEntry{name: "biggest", duration: 100 * time.Millisecond},
		phaseEntry{name: "middle", duration: 30 * time.Millisecond},
	)
	var buf bytes.Buffer
	pt.Summary(&buf)
	out := buf.String()
	bigIdx := strings.Index(out, "biggest")
	midIdx := strings.Index(out, "middle")
	smallIdx := strings.Index(out, "small")
	if bigIdx < 0 || midIdx < 0 || smallIdx < 0 {
		t.Fatalf("summary missing phases: %q", out)
	}
	if !(bigIdx < midIdx && midIdx < smallIdx) {
		t.Errorf("summary not sorted largest-first: biggest@%d middle@%d small@%d\n%s", bigIdx, midIdx, smallIdx, out)
	}
	if !strings.Contains(out, "total") {
		t.Errorf("summary missing total row: %q", out)
	}
	// 100 + 30 + 5 = 135ms; biggest is 100/135 ≈ 74%
	if !strings.Contains(out, "74%") {
		t.Errorf("summary missing expected 74%% for biggest phase; got:\n%s", out)
	}
}

func TestPhaseTimer_StartActuallyTimes(t *testing.T) {
	t.Parallel()
	pt := newPhaseTimer(true)
	done := pt.Start("sleeping")
	time.Sleep(10 * time.Millisecond)
	done()
	if len(pt.phases) != 1 {
		t.Fatalf("want 1 recorded phase, got %d", len(pt.phases))
	}
	if pt.phases[0].duration < 5*time.Millisecond {
		t.Errorf("phase recorded duration %v; want >= 5ms", pt.phases[0].duration)
	}
}

func TestHumanDuration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   time.Duration
		want string
	}{
		{500 * time.Microsecond, "500µs"},
		{2 * time.Millisecond, "2ms"},
		{999 * time.Millisecond, "999ms"},
		{1 * time.Second, "1.0s"},
		{1234 * time.Millisecond, "1.2s"},
	}
	for _, c := range cases {
		if got := humanDuration(c.in); got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
