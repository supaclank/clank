package sync

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// AgentStore is the subset of *store.Store the laptop sync agent needs.
// Defined as an interface so tests can inject an in-memory fake.
type AgentStore interface {
	LoadSyncStateTip(repoKey, branch string) (string, error)
	UpsertSyncStateTip(repoKey, branch, sha string) error
}

// AgentOptions configures the laptop-side sync agent.
type AgentOptions struct {
	// Repos is the list of local repo paths to scan. Read once at
	// construction time. Each path's "origin" remote URL is used as the
	// repo's stable identity (sync.RepoKey).
	Repos []string

	// Pusher is the configured client to the remote hub.
	Pusher *Pusher

	// Store is required for incremental pushes — without it every push
	// would be a full bundle.
	Store AgentStore

	// Interval is the polling interval. Defaults to 5s when zero.
	Interval time.Duration

	// Log is the logger. Defaults to log.Default() when nil.
	Log *log.Logger
}

// Agent is the laptop-side goroutine that polls the configured repos
// and pushes bundle deltas to the remote hub when local branch tips
// move.
type Agent struct {
	repos    []string
	pusher   *Pusher
	store    AgentStore
	interval time.Duration
	log      *log.Logger

	// pushNow trips one immediate scan from external callers (e.g. a
	// "Sync now" TUI action). Buffered so a sender never blocks.
	pushNow chan struct{}

	// startedMu guards started so Stop knows whether to wait on doneCh.
	startedMu sync.Mutex
	started   bool
	cancel    context.CancelFunc // cancels in-flight git subprocesses on Stop
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
}

// NewAgent wires up an Agent. Repos may be empty; the agent will simply
// idle until configuration changes (config reloading is future work).
func NewAgent(opts AgentOptions) (*Agent, error) {
	if opts.Pusher == nil {
		return nil, fmt.Errorf("sync agent: pusher is required")
	}
	if opts.Store == nil {
		return nil, fmt.Errorf("sync agent: store is required")
	}
	interval := opts.Interval
	if interval == 0 {
		interval = 5 * time.Second
	}
	lg := opts.Log
	if lg == nil {
		lg = log.Default()
	}
	return &Agent{
		repos:    append([]string(nil), opts.Repos...),
		pusher:   opts.Pusher,
		store:    opts.Store,
		interval: interval,
		log:      lg,
		pushNow:  make(chan struct{}, 1),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}, nil
}

// Start kicks off the goroutine. Returns immediately.
// A second Start is a no-op rather than a panic.
func (a *Agent) Start(ctx context.Context) {
	a.startedMu.Lock()
	defer a.startedMu.Unlock()
	if a.started {
		a.log.Printf("sync agent: Start called more than once; ignoring")
		return
	}
	a.started = true
	ctx, a.cancel = context.WithCancel(ctx)
	go a.run(ctx)
}

// Stop signals the goroutine to exit and waits for it to finish.
// Idempotent. Stop without a matching Start is a no-op.
func (a *Agent) Stop() {
	a.startedMu.Lock()
	if !a.started {
		a.startedMu.Unlock()
		return
	}
	cancel := a.cancel
	a.startedMu.Unlock()
	// Cancel first so in-flight git subprocesses exit promptly.
	cancel()
	a.stopOnce.Do(func() { close(a.stopCh) })
	<-a.doneCh
}

// PushNow asks the agent to run a scan as soon as possible. Drops on
// the floor if a scan is already queued.
func (a *Agent) PushNow() {
	select {
	case a.pushNow <- struct{}{}:
	default:
	}
}

func (a *Agent) run(ctx context.Context) {
	defer close(a.doneCh)
	a.scanAll(ctx)
	t := time.NewTicker(a.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.stopCh:
			return
		case <-t.C:
			a.scanAll(ctx)
		case <-a.pushNow:
			a.scanAll(ctx)
		}
	}
}

func (a *Agent) scanAll(ctx context.Context) {
	for _, repo := range a.repos {
		if err := a.scanRepo(ctx, repo); err != nil {
			a.log.Printf("sync agent: %s: %v", repo, err)
		}
	}
}

// scanRepo inspects one local clone and pushes any branches whose tip
// has moved since the last successful push. Failures on individual
// branches do not abort the whole repo — we log and move on so a
// transient failure on one branch doesn't block the rest.
func (a *Agent) scanRepo(ctx context.Context, repoPath string) error {
	remoteURL, err := gitRemoteURL(ctx, repoPath, "origin")
	if err != nil {
		return fmt.Errorf("get origin url: %w", err)
	}
	repoKey := RepoKey(remoteURL)

	branches, err := localBranches(ctx, repoPath)
	if err != nil {
		return fmt.Errorf("list local branches: %w", err)
	}

	for _, branch := range branches {
		tip, err := branchTip(ctx, repoPath, branch)
		if err != nil {
			a.log.Printf("sync agent: %s/%s: tip: %v", repoKey, branch, err)
			continue
		}
		prevTip, err := a.store.LoadSyncStateTip(repoKey, branch)
		if err != nil {
			a.log.Printf("sync agent: %s/%s: load state: %v", repoKey, branch, err)
			continue
		}
		if tip == prevTip {
			continue
		}

		if err := a.pushBranch(ctx, repoPath, repoKey, remoteURL, branch, tip, prevTip); err != nil {
			a.log.Printf("sync agent: %s/%s: push: %v", repoKey, branch, err)
			continue
		}
		if err := a.store.UpsertSyncStateTip(repoKey, branch, tip); err != nil {
			a.log.Printf("sync agent: %s/%s: persist state: %v", repoKey, branch, err)
		}
	}
	return nil
}

func (a *Agent) pushBranch(ctx context.Context, repoPath, repoKey, remoteURL, branch, tip, base string) error {
	bundle, err := makeBranchBundle(ctx, repoPath, branch, base)
	if err != nil {
		// A previously-pushed base SHA may have been rewritten or pruned
		// (rebase, force-push, gc). Fall back to a full bundle once so
		// the cloud mirror catches up; subsequent pushes will be
		// incremental again from the new tip.
		if base != "" {
			a.log.Printf("sync agent: %s/%s: incremental bundle failed (%v) — retrying full", repoKey, branch, err)
			full, fullErr := makeBranchBundle(ctx, repoPath, branch, "")
			if fullErr != nil {
				return fmt.Errorf("full bundle: %w", fullErr)
			}
			bundle = full
			base = ""
		} else {
			return err
		}
	}
	return a.pusher.Push(ctx, PushRequest{
		RepoKey:   repoKey,
		RemoteURL: remoteURL,
		Branch:    branch,
		TipSHA:    tip,
		BaseSHA:   base,
		Bundle:    bytes.NewReader(bundle),
	})
}

// makeBranchBundle runs `git bundle create` and returns the bundle's
// raw bytes. When base is non-empty, the bundle excludes commits
// reachable from base — an incremental push. Empty base = full bundle
// of everything reachable from the branch.
func makeBranchBundle(ctx context.Context, repoPath, branch, base string) ([]byte, error) {
	// Fully-qualify branch so a same-named tag can't shadow it. base is
	// a previously-pushed SHA (not a ref name) so it stays as-is.
	args := []string{"-C", repoPath, "bundle", "create", "-", "refs/heads/" + branch}
	if base != "" {
		args = append(args, "^"+base)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git bundle create: %w: %s", err, errBuf.String())
	}
	return out.Bytes(), nil
}

func gitRemoteURL(ctx context.Context, repoPath, remote string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "config", "--get", "remote."+remote+".url").Output()
	if err != nil {
		return "", fmt.Errorf("read remote.%s.url: %w", remote, err)
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", fmt.Errorf("remote.%s.url is empty", remote)
	}
	return url, nil
}

// localBranches returns the names of the repo's local branches
// (refs/heads/*). Skips remote-tracking branches and tags.
func localBranches(ctx context.Context, repoPath string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "for-each-ref", "--format=%(refname:short)", "refs/heads/").Output()
	if err != nil {
		return nil, fmt.Errorf("for-each-ref: %w", err)
	}
	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			branches = append(branches, line)
		}
	}
	return branches, nil
}

func branchTip(ctx context.Context, repoPath, branch string) (string, error) {
	// Fully-qualify so a same-named tag can't shadow the branch.
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "refs/heads/"+branch).Output()
	if err != nil {
		return "", fmt.Errorf("rev-parse %s: %w", branch, err)
	}
	return strings.TrimSpace(string(out)), nil
}
