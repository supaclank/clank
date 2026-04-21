package host

// Service is the in-process domain object for the Host plane. It owns the
// BackendManagers that run agent sessions and the git/worktree logic tied
// to repos on this host's filesystem.
//
// The Host has NO repo registry. It resolves a wire GitRef to a working
// directory on demand via workDirFor(): local refs use the path
// directly; remote refs are cloned into a deterministic
// <ClonesDir>/<CloneDirName(remote)>/ on first use.

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/git"
)

// Service is the Host plane's domain object. Construct with New; call
// Init to kick off background goroutines, and Shutdown to release
// resources.
//
// Service owns a registry of live SessionBackends keyed by the
// Hub-assigned session ID (a ULID). The Hub is the source of truth for
// session IDs because it owns the durable registry; the Host stores
// backends under those IDs so HTTP handlers can look them up by URL
// path.
type Service struct {
	id              Hostname
	startedAt       time.Time
	backendManagers map[agent.BackendType]agent.BackendManager
	log             *log.Logger

	mu       sync.RWMutex
	sessions map[string]agent.SessionBackend
	// closed is set by Shutdown under s.mu and gates new session
	// registrations. CreateSession checks it both at entry and after the
	// (potentially slow) mgr.CreateBackend call so a backend created
	// concurrently with Shutdown cannot leak into a torn-down registry.
	closed bool

	// clonesDir is the parent directory under which workDirFor clones
	// remote refs on first use. Each clone lands in
	// `<clonesDir>/<CloneDirName(remote)>/`. Defaults to ~/.clank/clones
	// at construction time; tests override via Options.ClonesDir.
	clonesDir string

	// cloneSF deduplicates concurrent first-use clones of the same
	// remote URL. Two CreateSession calls for the same remote that race
	// past the os.Stat-not-exist check would otherwise both invoke
	// `git clone` into the same target dir; the loser fails noisily.
	cloneSF singleflight.Group
}

// Options configures a Service at construction time.
type Options struct {
	// ID is the host identifier. Defaults to HostLocal when empty.
	ID Hostname
	// BackendManagers maps each backend type to its manager. Required.
	BackendManagers map[agent.BackendType]agent.BackendManager
	// Log is the logger. Defaults to a logger writing to stderr with the
	// "[clank-host]" prefix.
	Log *log.Logger
	// ClonesDir is the parent directory under which workDirFor clones
	// remote refs on first use. Defaults to ~/.clank/clones when empty.
	// Tests should set this to a t.TempDir().
	ClonesDir string
}

// New creates a Service. Panics if opts.BackendManagers is nil — the Host
// is not useful without at least one backend manager, and a fast failure
// at construction time is clearer than a nil deref later.
func New(opts Options) *Service {
	if opts.BackendManagers == nil {
		panic("host.New: BackendManagers is required")
	}
	id := opts.ID
	if id == "" {
		id = HostLocal
	}
	lg := opts.Log
	if lg == nil {
		lg = log.New(os.Stderr, "[clank-host] ", log.LstdFlags|log.Lmsgprefix)
	}
	s := &Service{
		id:              id,
		startedAt:       time.Now(),
		backendManagers: opts.BackendManagers,
		log:             lg,
		sessions:        make(map[string]agent.SessionBackend),
		clonesDir:       opts.ClonesDir,
	}
	if s.clonesDir == "" {
		// Best-effort default; if the home dir lookup fails the field
		// stays empty and workDirFor will reject remote refs with a
		// loud error rather than guessing a path.
		if home, err := os.UserHomeDir(); err == nil {
			s.clonesDir = filepath.Join(home, ".clank", "clones")
		}
	}
	return s
}

// ID returns the host's ID.
func (s *Service) ID() Hostname { return s.id }

// Init initializes all BackendManagers. knownDirs is a per-backend lookup
// that returns previously-seen project directories (used to warm
// long-lived servers like OpenCode). Pass a func returning nil, nil to
// skip warm-up.
//
// Init does NOT block — initialization kicks off reconciler goroutines that
// live for the duration of ctx.
func (s *Service) Init(ctx context.Context, knownDirs func(agent.BackendType) ([]string, error)) error {
	for bt, mgr := range s.backendManagers {
		bt := bt
		fn := func() ([]string, error) {
			if knownDirs == nil {
				return nil, nil
			}
			return knownDirs(bt)
		}
		if err := mgr.Init(ctx, fn); err != nil {
			s.log.Printf("warning: init %s backend: %v", bt, err)
		}
	}
	return nil
}

// Shutdown stops all live SessionBackends and then shuts down all
// BackendManagers. Safe to call multiple times — subsequent calls are
// no-ops. After Shutdown returns, CreateSession will reject new
// registrations rather than leaking backends into a torn-down service.
func (s *Service) Shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	live := s.sessions
	s.sessions = make(map[string]agent.SessionBackend)
	s.mu.Unlock()
	for id, b := range live {
		if err := b.Stop(); err != nil {
			s.log.Printf("warning: stop session %s: %v", id, err)
		}
	}
	for bt, mgr := range s.backendManagers {
		s.log.Printf("shutting down %s backend manager", bt)
		mgr.Shutdown()
	}
}

// Status returns the current host status.
func (s *Service) Status(_ context.Context) (HostStatus, error) {
	s.mu.RLock()
	live := len(s.sessions)
	s.mu.RUnlock()
	return HostStatus{
		Hostname:  s.id,
		Version:   "", // Populated once we have a version string wired up
		StartedAt: s.startedAt,
		Sessions:  live,
	}, nil
}

// ListBackends returns the set of backends known to this host.
func (s *Service) ListBackends(_ context.Context) ([]BackendInfo, error) {
	out := make([]BackendInfo, 0, len(s.backendManagers))
	for bt := range s.backendManagers {
		out = append(out, BackendInfo{
			Name:        bt,
			DisplayName: string(bt),
			Available:   true,
		})
	}
	return out, nil
}

// ListAgents returns the agents supported by the given backend for the
// repo identified by ref. Per §7.3 of hub_host_refactor_code_review.md
// the wire is path-free: callers send a GitRef and the host resolves it
// to a working directory via workDirFor.
//
// Returns (nil, nil) when the backend is unknown to this host or its
// manager does not implement listing — both are normal "this host does
// not surface that capability" answers, not errors.
func (s *Service) ListAgents(ctx context.Context, bt agent.BackendType, ref agent.GitRef) ([]AgentInfo, error) {
	mgr, ok := s.backendManagers[bt]
	if !ok {
		return nil, nil
	}
	lister, ok := mgr.(agent.AgentLister)
	if !ok {
		return nil, nil
	}
	workDir, err := s.workDirFor(ctx, ref)
	if err != nil {
		return nil, err
	}
	return lister.ListAgents(ctx, workDir)
}

// ListModels mirrors ListAgents for model catalogs.
func (s *Service) ListModels(ctx context.Context, bt agent.BackendType, ref agent.GitRef) ([]ModelInfo, error) {
	mgr, ok := s.backendManagers[bt]
	if !ok {
		return nil, nil
	}
	lister, ok := mgr.(agent.ModelLister)
	if !ok {
		return nil, nil
	}
	workDir, err := s.workDirFor(ctx, ref)
	if err != nil {
		return nil, err
	}
	return lister.ListModels(ctx, workDir)
}

// DiscoverSessions asks the given backend manager for historical sessions
// it already knows about. Returns nil, nil for managers that do not
// implement discovery (e.g. Claude Code, which is stateless across runs).
func (s *Service) DiscoverSessions(ctx context.Context, bt agent.BackendType, seedDir string) ([]agent.SessionSnapshot, error) {
	mgr, ok := s.backendManagers[bt]
	if !ok {
		return nil, nil
	}
	disc, ok := mgr.(agent.SessionDiscoverer)
	if !ok {
		return nil, nil
	}
	return disc.DiscoverSessions(ctx, seedDir)
}

// CreateSession creates a fresh SessionBackend for req and registers it
// under the given Hub-assigned session ID. The backend is NOT started —
// callers call Start() or Watch() on the returned backend.
//
// Returns the resolved serverURL (empty string for backends without an
// HTTP server, e.g. Claude Code). The hub uses serverURL on a per-session
// basis (e.g. for `opencode attach <url>` shell-out).
//
// Identity (Hostname, GitRef) is path-free. The host resolves
// req.GitRef → workDir via workDirFor (clone-on-first-use for Remote
// refs, direct path for Local refs, plus optional WorktreeBranch
// resolution).
func (s *Service) CreateSession(ctx context.Context, sessionID string, req agent.StartRequest) (agent.SessionBackend, string, error) {
	if sessionID == "" {
		return nil, "", fmt.Errorf("session id is required")
	}
	if req.Backend == "" {
		return nil, "", fmt.Errorf("backend is required")
	}
	mgr, ok := s.backendManagers[req.Backend]
	if !ok {
		return nil, "", fmt.Errorf("no backend manager for %s", req.Backend)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, "", fmt.Errorf("host service is shut down")
	}
	if _, exists := s.sessions[sessionID]; exists {
		s.mu.Unlock()
		return nil, "", fmt.Errorf("session %s already registered", sessionID)
	}
	s.mu.Unlock()

	if err := req.GitRef.Validate(); err != nil {
		return nil, "", fmt.Errorf("git_ref: %w", err)
	}
	workDir, err := s.workDirFor(ctx, req.GitRef)
	if err != nil {
		return nil, "", err
	}

	b, err := mgr.CreateBackend(ctx, agent.BackendInvocation{
		WorkDir:          workDir,
		ResumeExternalID: req.SessionID,
	})
	if err != nil {
		return nil, "", err
	}
	// Re-check closed AND duplicate sessionID under the lock before
	// publishing. mgr.CreateBackend can take seconds (process spawn,
	// HTTP probe) and Shutdown — or another CreateSession racing on
	// the same sessionID — may have run in the interim.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		if stopErr := b.Stop(); stopErr != nil {
			s.log.Printf("warning: stop backend created during shutdown: %v", stopErr)
		}
		return nil, "", fmt.Errorf("host service is shut down")
	}
	if _, exists := s.sessions[sessionID]; exists {
		s.mu.Unlock()
		if stopErr := b.Stop(); stopErr != nil {
			s.log.Printf("warning: stop backend for duplicate session %s: %v", sessionID, stopErr)
		}
		return nil, "", fmt.Errorf("session %s already registered", sessionID)
	}
	s.sessions[sessionID] = b
	s.mu.Unlock()

	// Resolve per-session serverURL for backends that expose one. Only
	// OpenCode currently does; iterate ListServers (no fresh start —
	// CreateBackend already ensured a server exists for workDir).
	serverURL := ""
	if oc, ok := mgr.(*OpenCodeBackendManager); ok {
		for _, srv := range oc.ListServers() {
			if srv.ProjectDir == workDir {
				serverURL = srv.URL
				break
			}
		}
	}

	return b, serverURL, nil
}

// workDirFor resolves a GitRef to an absolute working directory on this
// host.
//
// Resolution precedence (matches the GitRef godoc):
//  1. If ref.LocalPath is set AND it exists on this host AND it is the
//     repo root → use it directly. No clone.
//  2. Else if ref.RemoteURL is set → clone into
//     <clonesDir>/<CloneDirName(RemoteURL)>/ and use that.
//  3. Else → error.
//
// When ref.WorktreeBranch is non-empty, the result is the worktree path
// for that branch under the resolved base; otherwise it is the repo
// root.
//
// Step 1 failing (path missing or not a repo root) is *not* an error
// when RemoteURL is set — it falls through to step 2. This is the
// "laptop TUI sent a path that doesn't exist on this remote host" case;
// the host clones from the remote and proceeds. Step 1 failing with
// no RemoteURL set IS an error.
func (s *Service) workDirFor(ctx context.Context, ref agent.GitRef) (string, error) {
	var base string

	if ref.LocalPath != "" {
		res := s.tryLocalPath(ref.LocalPath)
		if res.HardErr != nil {
			return "", res.HardErr
		}
		if res.Usable {
			base = ref.LocalPath
		} else if ref.RemoteURL == "" {
			// Surface the actual reason rather than a generic "not
			// usable" — the caller has no remote fallback, so they
			// need to know why their path was rejected.
			return "", fmt.Errorf("local_path %q not usable: %w", ref.LocalPath, res.SoftFail)
		}
	}

	if base == "" {
		if ref.RemoteURL == "" {
			return "", fmt.Errorf("git ref must set at least one of local_path or remote_url")
		}
		if s.clonesDir == "" {
			return "", fmt.Errorf("cannot resolve remote ref: host has no clones_dir configured")
		}
		name, err := agent.CloneDirName(ref.RemoteURL)
		if err != nil {
			return "", fmt.Errorf("clone dir name for %q: %w", ref.RemoteURL, err)
		}
		base = filepath.Join(s.clonesDir, name)
		if _, err := os.Stat(base); os.IsNotExist(err) {
			// Singleflight on the clone target so concurrent
			// CreateSession calls for the same remote don't both
			// invoke `git clone` into the same dir.
			_, cloneErr, _ := s.cloneSF.Do(base, func() (any, error) {
				// Re-check under the singleflight: a peer may have
				// finished cloning while we were queued.
				if _, statErr := os.Stat(base); statErr == nil {
					return nil, nil
				}
				if mkErr := os.MkdirAll(s.clonesDir, 0o755); mkErr != nil {
					return nil, fmt.Errorf("create clones dir %q: %w", s.clonesDir, mkErr)
				}
				s.log.Printf("cloning %s into %s", ref.RemoteURL, base)
				if cloneErr := git.Clone(ref.RemoteURL, base); cloneErr != nil {
					return nil, fmt.Errorf("clone %q: %w", ref.RemoteURL, cloneErr)
				}
				return nil, nil
			})
			if cloneErr != nil {
				return "", cloneErr
			}
		} else if err != nil {
			return "", fmt.Errorf("stat clone dir %q: %w", base, err)
		}
	}

	if ref.WorktreeBranch != "" {
		wt, err := s.resolveWorktree(ctx, base, ref.WorktreeBranch)
		if err != nil {
			return "", fmt.Errorf("resolve worktree for branch %q: %w", ref.WorktreeBranch, err)
		}
		return wt.WorktreeDir, nil
	}
	return base, nil
}

// tryLocalPath inspects path and reports whether it can be used as a
// local repo root on this host.
//
// localPathResult is the structured outcome of tryLocalPath, replacing
// the prior (bool, error, error) return shape that conflated three
// distinct cases. Exactly one of {Usable, SoftFail, HardErr} is
// meaningful per result:
//
//   - Usable=true:                use the path directly.
//   - SoftFail!=nil:              path is missing or not a git repo on
//     this host. Caller may fall back to cloning RemoteURL; the reason
//     is surfaced if no fallback exists.
//   - HardErr!=nil:               caller bug — relative path, symlink
//     failure, or a path that IS inside a git repo but isn't its root.
//     Never fall back.
type localPathResult struct {
	Usable   bool
	SoftFail error
	HardErr  error
}

// tryLocalPath inspects path and reports whether it can be used as a
// session work directory on this host. See localPathResult for the
// semantics of each field.
func (s *Service) tryLocalPath(path string) localPathResult {
	if !filepath.IsAbs(path) {
		return localPathResult{HardErr: fmt.Errorf("local_path must be absolute, got %q", path)}
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return localPathResult{SoftFail: fmt.Errorf("path does not exist on this host")}
		}
		return localPathResult{HardErr: fmt.Errorf("stat local_path %q: %w", path, err)}
	}
	root, err := git.RepoRoot(path)
	if err != nil {
		return localPathResult{SoftFail: fmt.Errorf("not a git repo")}
	}
	// Resolve symlinks on both sides because macOS reports /var/folders
	// as /private/var/folders for the root.
	givenAbs, err := filepath.EvalSymlinks(path)
	if err != nil {
		return localPathResult{HardErr: fmt.Errorf("resolve symlinks for %q: %w", path, err)}
	}
	rootAbs, err := filepath.EvalSymlinks(root)
	if err != nil {
		return localPathResult{HardErr: fmt.Errorf("resolve symlinks for repo root %q: %w", root, err)}
	}
	if filepath.Clean(rootAbs) != filepath.Clean(givenAbs) {
		return localPathResult{HardErr: fmt.Errorf("local_path %q is not the repo root (root is %q)", path, root)}
	}
	return localPathResult{Usable: true}
}

// Session returns the SessionBackend registered under id, or nil and
// false if there is no such session.
func (s *Service) Session(id string) (agent.SessionBackend, bool) {
	s.mu.RLock()
	b, ok := s.sessions[id]
	s.mu.RUnlock()
	return b, ok
}

// StopSession stops the SessionBackend registered under id and removes
// it from the registry. Returns ErrNotFound if there is no such session.
// Safe to call concurrently with reads.
func (s *Service) StopSession(id string) error {
	s.mu.Lock()
	b, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	delete(s.sessions, id)
	s.mu.Unlock()
	return b.Stop()
}

// --- Worktree / branch ops ----------------------------------------------

// ListBranches returns the branches (and their checked-out worktrees)
// for the repository identified by ref. Skips bare and detached entries.
// ref.WorktreeBranch is ignored — listing operates on the repo root.
func (s *Service) ListBranches(ctx context.Context, ref agent.GitRef) ([]BranchInfo, error) {
	repoRef := ref
	repoRef.WorktreeBranch = ""
	root, err := s.workDirFor(ctx, repoRef)
	if err != nil {
		return nil, err
	}
	return s.listBranches(ctx, root)
}

// ResolveWorktree ensures a worktree exists for (ref's repo, branch) and
// returns its info. ref.WorktreeBranch is ignored — pass branch as a
// distinct argument so the caller's intent ("resolve THIS branch") is
// explicit at the call site.
func (s *Service) ResolveWorktree(ctx context.Context, ref agent.GitRef, branch string) (WorktreeInfo, error) {
	repoRef := ref
	repoRef.WorktreeBranch = ""
	root, err := s.workDirFor(ctx, repoRef)
	if err != nil {
		return WorktreeInfo{}, err
	}
	return s.resolveWorktree(ctx, root, branch)
}

// RemoveWorktree removes the worktree for (ref's repo, branch).
func (s *Service) RemoveWorktree(ctx context.Context, ref agent.GitRef, branch string, force bool) error {
	repoRef := ref
	repoRef.WorktreeBranch = ""
	root, err := s.workDirFor(ctx, repoRef)
	if err != nil {
		return err
	}
	return s.removeWorktree(ctx, root, branch, force)
}

// MergeBranch merges branch into ref's repo's default branch.
func (s *Service) MergeBranch(ctx context.Context, ref agent.GitRef, branch, commitMessage string) (MergeResult, error) {
	repoRef := ref
	repoRef.WorktreeBranch = ""
	root, err := s.workDirFor(ctx, repoRef)
	if err != nil {
		return MergeResult{}, err
	}
	return s.mergeBranch(ctx, root, branch, commitMessage)
}

// listBranches returns the branches (and their checked-out worktrees)
// for the repository at projectDir. Skips bare and detached entries.
func (s *Service) listBranches(_ context.Context, projectDir string) ([]BranchInfo, error) {
	worktrees, err := git.ListWorktrees(projectDir)
	if err != nil {
		return nil, err
	}

	defaultBranch, _ := git.DefaultBranch(projectDir)
	currentBranch, _ := git.CurrentBranch(projectDir)

	result := make([]BranchInfo, 0, len(worktrees))
	for _, wt := range worktrees {
		if wt.Bare || wt.Branch == "" {
			continue
		}
		info := BranchInfo{
			Name:        wt.Branch,
			WorktreeDir: wt.Path,
			IsDefault:   wt.Branch == defaultBranch,
			IsCurrent:   wt.Branch == currentBranch,
		}
		// Diff stats + ahead count only make sense for non-default branches.
		if wt.Branch != defaultBranch {
			if added, removed, err := git.DiffStat(wt.Path, defaultBranch); err == nil {
				info.LinesAdded = added
				info.LinesRemoved = removed
			}
			if ahead, err := git.CommitsAhead(projectDir, defaultBranch, wt.Branch); err == nil {
				info.CommitsAhead = ahead
			}
		}
		result = append(result, info)
	}
	return result, nil
}

// resolveWorktree ensures a git worktree exists for (projectDir, branch).
// If the branch has a worktree, returns its path; otherwise creates one.
// If the branch does not exist locally, creates a new branch from the
// repo's default branch.
func (s *Service) resolveWorktree(_ context.Context, projectDir, branch string) (WorktreeInfo, error) {
	wt, err := git.FindWorktreeForBranch(projectDir, branch)
	if err != nil {
		return WorktreeInfo{}, err
	}
	if wt != nil {
		return WorktreeInfo{Branch: branch, WorktreeDir: wt.Path}, nil
	}

	projectName := filepath.Base(projectDir)
	wtDir, err := git.WorktreeDir(projectName, branch)
	if err != nil {
		return WorktreeInfo{}, err
	}

	exists, err := git.BranchExists(projectDir, branch)
	if err != nil {
		return WorktreeInfo{}, err
	}

	if exists {
		if err := git.AddWorktree(projectDir, wtDir, branch); err != nil {
			return WorktreeInfo{}, err
		}
	} else {
		base, err := git.DefaultBranch(projectDir)
		if err != nil {
			return WorktreeInfo{}, fmt.Errorf("determine default branch: %w", err)
		}
		if err := git.AddWorktreeNewBranch(projectDir, wtDir, branch, base); err != nil {
			return WorktreeInfo{}, err
		}
	}
	s.log.Printf("created worktree for branch %q at %s", branch, wtDir)
	return WorktreeInfo{Branch: branch, WorktreeDir: wtDir}, nil
}

// removeWorktree removes the worktree for (projectDir, branch). Returns
// an error if there is no such worktree.
func (s *Service) removeWorktree(_ context.Context, projectDir, branch string, force bool) error {
	wt, err := git.FindWorktreeForBranch(projectDir, branch)
	if err != nil {
		return err
	}
	if wt == nil {
		return fmt.Errorf("%w: no worktree found for branch %q", ErrNotFound, branch)
	}
	if err := git.RemoveWorktree(projectDir, wt.Path, force); err != nil {
		return err
	}
	s.log.Printf("removed worktree for branch %q at %s", branch, wt.Path)
	return nil
}

// MergeResult describes the outcome of MergeBranch.
type MergeResult struct {
	MergedBranch    string
	BranchWorktree  string // Path of the feature-branch worktree (empty if it was cleaned up)
	WorktreeRemoved bool
	BranchDeleted   bool
}

// mergeBranch merges `branch` into the repo's default branch. Before
// merging, it `git add -A`s the feature worktree; if there are staged
// changes, commitMessage is used to commit them first (required in that
// case).
func (s *Service) mergeBranch(_ context.Context, projectDir, branch, commitMessage string) (MergeResult, error) {
	defaultBranch, err := git.DefaultBranch(projectDir)
	if err != nil {
		return MergeResult{}, fmt.Errorf("determine default branch: %w", err)
	}
	if branch == defaultBranch {
		return MergeResult{}, ErrCannotMergeDefault
	}

	branchWt, err := git.FindWorktreeForBranch(projectDir, branch)
	if err != nil {
		return MergeResult{}, fmt.Errorf("find branch worktree: %w", err)
	}
	if branchWt == nil {
		return MergeResult{}, fmt.Errorf("%w: no worktree found for branch %q", ErrNotFound, branch)
	}

	if err := git.AddAll(branchWt.Path); err != nil {
		return MergeResult{}, fmt.Errorf("git add -A in worktree: %w", err)
	}
	hasStagedWork, err := git.HasStagedChanges(branchWt.Path)
	if err != nil {
		return MergeResult{}, fmt.Errorf("check staged changes: %w", err)
	}
	commitsAhead, err := git.CommitsAhead(projectDir, defaultBranch, branch)
	if err != nil {
		return MergeResult{}, fmt.Errorf("count commits ahead: %w", err)
	}
	if !hasStagedWork && commitsAhead == 0 {
		return MergeResult{}, ErrNothingToMerge
	}
	if hasStagedWork {
		if commitMessage == "" {
			return MergeResult{}, ErrCommitMessageRequired
		}
		if err := git.Commit(branchWt.Path, commitMessage); err != nil {
			return MergeResult{}, fmt.Errorf("commit worktree changes: %w", err)
		}
		s.log.Printf("committed worktree changes in %s on branch %q", branchWt.Path, branch)
	}

	mainWt, err := git.FindWorktreeForBranch(projectDir, defaultBranch)
	if err != nil {
		return MergeResult{}, fmt.Errorf("find main worktree: %w", err)
	}
	if mainWt == nil {
		return MergeResult{}, fmt.Errorf("%w: no worktree for default branch %q", ErrNotFound, defaultBranch)
	}
	clean, err := git.IsClean(mainWt.Path)
	if err != nil {
		return MergeResult{}, fmt.Errorf("check worktree clean: %w", err)
	}
	if !clean {
		return MergeResult{}, ErrTargetDirty
	}

	mergeMsg := fmt.Sprintf("Merge branch '%s'", branch)
	if err := git.MergeNoFF(mainWt.Path, branch, mergeMsg); err != nil {
		if git.IsMerging(mainWt.Path) {
			_ = git.AbortMerge(mainWt.Path)
			return MergeResult{}, ErrMergeConflict
		}
		return MergeResult{}, fmt.Errorf("merge failed: %w", err)
	}
	s.log.Printf("merged branch %q into %q in %s", branch, defaultBranch, mainWt.Path)

	res := MergeResult{
		MergedBranch:   branch,
		BranchWorktree: branchWt.Path,
	}
	if err := git.RemoveWorktree(projectDir, branchWt.Path, true); err != nil {
		s.log.Printf("warning: could not remove worktree after merge: %v", err)
	} else {
		res.WorktreeRemoved = true
	}
	if err := git.DeleteBranch(projectDir, branch, false); err != nil {
		s.log.Printf("warning: could not delete branch after merge: %v", err)
	} else {
		res.BranchDeleted = true
	}
	return res, nil
}
