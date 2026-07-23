package acp

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"sync"
	"time"
)

// AdapterProc is one supervised adapter process (or its in-process test
// stand-in): a live conn plus a stop hook. Spawn functions produce it.
type AdapterProc struct {
	Conn *AdapterConn
	// Stop terminates the process: graceful first, then forceful after
	// stopGrace. Must be safe to call more than once.
	Stop func()
	// envFP is the profile-env fingerprint at spawn time; reconcile
	// restarts the proc when the current fingerprint differs.
	envFP string
}

// SpawnFunc launches one adapter for scopeDir. The default implementation
// execs the profile's Command; tests substitute in-process pipe pairs
// (the same seam OpenCodeServerManager exposes via SetStartServerFn).
type SpawnFunc func(ctx context.Context, scopeDir string) (*AdapterProc, error)

type connWaiter struct{ ch chan connResult }

type connResult struct {
	conn *AdapterConn
	err  error
}

// AdapterSupervisor reconciles desired adapter processes, one per scope
// key. Modeled on OpenCodeServerManager: a single Run goroutine owns all
// process starts/stops; everything else registers desire and waits.
type AdapterSupervisor struct {
	profile        AdapterProfile
	logf           func(format string, args ...any)
	reconcileEvery time.Duration
	spawn          SpawnFunc

	mu      sync.Mutex
	procs   map[string]*AdapterProc
	desired map[string]struct{}
	waiters map[string][]connWaiter
	stopped bool

	nudge chan struct{}
}

// NewAdapterSupervisor validates the profile and prepares a supervisor;
// call Run to start reconciling.
func NewAdapterSupervisor(profile AdapterProfile, logf func(string, ...any)) (*AdapterSupervisor, error) {
	if err := profile.validate(); err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &AdapterSupervisor{
		profile:        profile,
		logf:           logf,
		reconcileEvery: defaultReconcileEvery,
		procs:          make(map[string]*AdapterProc),
		desired:        make(map[string]struct{}),
		waiters:        make(map[string][]connWaiter),
		nudge:          make(chan struct{}, 1),
	}
	s.spawn = s.execSpawn
	return s, nil
}

// SetSpawnFunc replaces process launching — test hook.
func (s *AdapterSupervisor) SetSpawnFunc(fn SpawnFunc) { s.spawn = fn }

// SetReconcileInterval shortens the reconcile cadence — test hook.
func (s *AdapterSupervisor) SetReconcileInterval(d time.Duration) { s.reconcileEvery = d }

// Run is the reconciler loop and the only goroutine that starts or stops
// adapter processes. Blocks until ctx is cancelled, then stops everything.
func (s *AdapterSupervisor) Run(ctx context.Context) {
	ticker := time.NewTicker(s.reconcileEvery)
	defer ticker.Stop()
	s.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			s.StopAll()
			return
		case <-ticker.C:
			s.reconcile(ctx)
		case <-s.nudge:
			s.reconcile(ctx)
		}
	}
}

// Nudge asks the reconciler to run promptly (e.g. after a credential
// write changed the profile env). Non-blocking.
func (s *AdapterSupervisor) Nudge() {
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// AddDesired marks workDir's scope as wanted without waiting for it.
func (s *AdapterSupervisor) AddDesired(workDir string) {
	key := s.profile.ScopeKey(workDir)
	s.mu.Lock()
	s.desired[key] = struct{}{}
	s.mu.Unlock()
	s.Nudge()
}

// GetConn returns a live conn for workDir's scope, starting the adapter
// if needed. Blocks until the reconciler delivers one or ctx ends.
func (s *AdapterSupervisor) GetConn(ctx context.Context, workDir string) (*AdapterConn, error) {
	key := s.profile.ScopeKey(workDir)

	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil, fmt.Errorf("acp %s: supervisor stopped", s.profile.ID)
	}
	if p, ok := s.procs[key]; ok && alive(p) {
		s.mu.Unlock()
		return p.Conn, nil
	}
	w := connWaiter{ch: make(chan connResult, 1)}
	s.waiters[key] = append(s.waiters[key], w)
	s.desired[key] = struct{}{}
	s.mu.Unlock()
	s.Nudge()

	select {
	case res := <-w.ch:
		return res.conn, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// StopAll terminates every adapter, fails pending waiters, and clears
// desired state. The supervisor is unusable afterwards.
func (s *AdapterSupervisor) StopAll() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	procs := maps.Clone(s.procs)
	waiters := s.waiters
	s.procs = make(map[string]*AdapterProc)
	s.desired = make(map[string]struct{})
	s.waiters = make(map[string][]connWaiter)
	s.mu.Unlock()

	for _, ws := range waiters {
		for _, w := range ws {
			w.ch <- connResult{err: fmt.Errorf("acp %s: supervisor stopped", s.profile.ID)}
		}
	}
	for _, p := range procs {
		p.Stop()
	}
}

func alive(p *AdapterProc) bool {
	select {
	case <-p.Conn.Closed():
		return false
	default:
		return true
	}
}

// reconcile converges running procs onto desired state: drop dead or
// env-stale procs, then start whatever is missing (in parallel), then
// notify waiters.
func (s *AdapterSupervisor) reconcile(ctx context.Context) {
	currentFP := envFingerprint(s.profileEnv())

	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	var toStop []*AdapterProc
	for key, p := range s.procs {
		switch {
		case !alive(p):
			s.logf("acp %s: adapter for %q died; will restart while desired", s.profile.ID, key)
			delete(s.procs, key)
		case p.envFP != currentFP:
			s.logf("acp %s: env changed; restarting adapter for %q", s.profile.ID, key)
			toStop = append(toStop, p)
			delete(s.procs, key)
		}
	}
	var missing []string
	for key := range s.desired {
		if _, ok := s.procs[key]; !ok {
			missing = append(missing, key)
		}
	}
	s.mu.Unlock()

	for _, p := range toStop {
		p.Stop()
	}
	if len(missing) == 0 {
		return
	}

	type started struct {
		key  string
		proc *AdapterProc
		err  error
	}
	results := make(chan started, len(missing))
	for _, key := range missing {
		go func(key string) {
			// No timeout here: spawn implementations own their budgets
			// (Prepare may run a cold adapter install; initialize uses
			// spawnTimeout).
			p, err := s.spawn(ctx, key)
			if err == nil {
				p.envFP = currentFP
			}
			results <- started{key: key, proc: p, err: err}
		}(key)
	}

	for range missing {
		res := <-results
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			if res.err == nil {
				res.proc.Stop()
			}
			continue
		}
		ws := s.waiters[res.key]
		delete(s.waiters, res.key)
		if res.err != nil {
			// Stay desired: the next tick retries. Waiters fail now —
			// their callers surface the error instead of hanging.
			s.logf("acp %s: start adapter for %q: %v", s.profile.ID, res.key, res.err)
			s.mu.Unlock()
			for _, w := range ws {
				w.ch <- connResult{err: res.err}
			}
			continue
		}
		s.procs[res.key] = res.proc
		s.mu.Unlock()
		for _, w := range ws {
			w.ch <- connResult{conn: res.proc.Conn}
		}
	}
}

func (s *AdapterSupervisor) profileEnv() map[string]string {
	if s.profile.Env == nil {
		return nil
	}
	return s.profile.Env()
}

// execSpawn is the production SpawnFunc: launch the adapter argv with the
// profile env merged over the parent environment, wire pipes, and watch
// for process exit.
func (s *AdapterSupervisor) execSpawn(ctx context.Context, scopeDir string) (*AdapterProc, error) {
	if s.profile.Prepare != nil {
		if err := s.profile.Prepare(ctx); err != nil {
			return nil, fmt.Errorf("acp %s: prepare: %w", s.profile.ID, err)
		}
	}
	initCtx, cancel := context.WithTimeout(ctx, spawnTimeout)
	defer cancel()
	ctx = initCtx
	bin, args := s.profile.Command(scopeDir)
	// Deliberately not CommandContext: the spawn ctx only bounds startup;
	// the supervisor owns process lifetime via Stop.
	cmd := exec.Command(bin, args...)
	if scopeDir != "" {
		cmd.Dir = scopeDir
	}
	cmd.Env = os.Environ()
	for k, v := range s.profileEnv() {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp %s: stdin pipe: %w", s.profile.ID, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("acp %s: stdout pipe: %w", s.profile.ID, err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("acp %s: start %s: %w", s.profile.ID, bin, err)
	}

	conn, err := NewAdapterConn(ctx, s.profile, stdin, stdout, s.logf)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	// Process exit ⇒ conn closed, even if the pipes wedge.
	go func() {
		_ = cmd.Wait()
		conn.markClosed()
	}()

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			_ = cmd.Process.Signal(os.Interrupt)
			select {
			case <-conn.Closed():
			case <-time.After(stopGrace):
				_ = cmd.Process.Kill()
			}
		})
	}
	return &AdapterProc{Conn: conn, Stop: stop}, nil
}
