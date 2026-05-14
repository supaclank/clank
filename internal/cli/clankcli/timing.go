package clankcli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// envTrue returns true when the named env var is set to a value
// commonly meaning "yes". Used by --timing-style flags that also
// honour an env override (CLANK_TIMING=1).
func envTrue(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// phaseTimer is the laptop-side instrumentation hook for diagnosing
// where push/pull --migrate spend their time. Disabled-by-default
// (zero-cost when the user hasn't asked for timing); enabled via the
// --timing flag or CLANK_TIMING=1.
//
// Designed for the "what should I optimize next" use case, not for
// continuous metrics collection. One run, one printout, sorted by
// duration so the bottleneck is at the top.
//
// Phases are recorded in completion order (not start order) — start a
// phase, run the work, call the returned closure to record. Callers
// must invoke Start sequentially: the recorder appends to a shared
// slice without synchronization, since every caller today
// (push/pull/push_sessions) drives migration phases serially.
type phaseTimer struct {
	enabled bool
	phases  []phaseEntry
}

type phaseEntry struct {
	name     string
	duration time.Duration
}

// newPhaseTimer returns a timer that records phases when enabled is
// true and is a no-op otherwise. The no-op path costs one method
// dispatch per Start, which is invisible at the milliseconds-per-
// phase scale we care about.
func newPhaseTimer(enabled bool) *phaseTimer {
	return &phaseTimer{enabled: enabled}
}

// Start begins a phase named name and returns a closure that, when
// called, records the elapsed time. Idiomatic usage:
//
//	done := timer.Start("push checkpoint")
//	res, err := cli.PushCheckpoint(ctx, ...)
//	done()
//	if err != nil { ... }
//
// If the timer is disabled, both Start and the returned closure are
// no-ops.
func (t *phaseTimer) Start(name string) func() {
	if !t.enabled {
		return func() {}
	}
	start := time.Now()
	return func() {
		t.phases = append(t.phases, phaseEntry{name: name, duration: time.Since(start)})
	}
}

// Summary writes a human-readable timing breakdown to w. No-op when
// disabled or when no phases were recorded. Sorts largest-first so
// the bottleneck reads at the top; includes percent-of-total so
// ratios are immediately visible.
func (t *phaseTimer) Summary(w io.Writer) {
	if !t.enabled || len(t.phases) == 0 {
		return
	}
	// Snapshot so the user-visible order doesn't depend on completion
	// order.
	sorted := make([]phaseEntry, len(t.phases))
	copy(sorted, t.phases)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].duration > sorted[j].duration
	})

	var total time.Duration
	maxName := 0
	for _, p := range sorted {
		total += p.duration
		if l := len(p.name); l > maxName {
			maxName = l
		}
	}

	const durColWidth = 7 // " 12.3s ", " 999ms ", " 999µs " all fit
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  timing (sorted, largest first):")
	for _, p := range sorted {
		pct := 0.0
		if total > 0 {
			pct = 100.0 * float64(p.duration) / float64(total)
		}
		fmt.Fprintf(w, "    %-*s  %*s  %4.0f%%\n",
			maxName, p.name,
			durColWidth, humanDuration(p.duration),
			pct,
		)
	}
	fmt.Fprintf(w, "    %s\n", strings.Repeat("─", maxName+durColWidth+8))
	fmt.Fprintf(w, "    %-*s  %*s\n", maxName, "total", durColWidth, humanDuration(total))
}

// humanDuration formats d at a granularity that matches how a human
// reads it: sub-millisecond → µs, sub-second → ms, otherwise seconds
// to one decimal. Avoids the "1.234567s" noise of d.String().
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
}
