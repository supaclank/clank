package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	codex "github.com/pmenglund/codex-sdk-go"
	"github.com/pmenglund/codex-sdk-go/protocol"
)

// CodexBackend drives an OpenAI Codex session over the app-server JSON-RPC
// protocol (github.com/pmenglund/codex-sdk-go). One `codex app-server`
// subprocess is spawned per session at Open, mirroring the Claude backend's
// process-per-session shape. The codex thread id is the session's ExternalID.
type CodexBackend struct {
	mu     sync.Mutex
	openMu sync.Mutex

	status     SessionStatus
	projectDir string
	threadID   string // codex thread id (ExternalID); pre-seeded on resume
	events     chan Event
	closeOnce  sync.Once
	stopped    bool

	client *codex.Codex
	thread *codex.Thread

	// activeTurnID is the in-flight turn (set by turn/started, cleared by
	// turn/completed). Abort needs it for turn/interrupt. Guarded by mu.
	activeTurnID string

	// currentMsgID is the synthesized assistant message id for the in-flight
	// turn (the turn id — stable across live stream and thread/read, so
	// clients reconcile history refetches by message id). Guarded by mu.
	currentMsgID string

	// pendingPerms maps a synthesized permission request id to the channel
	// that delivers the user's decision. The approval handler registers an
	// entry and blocks on it; RespondPermission resolves it. Guarded by mu.
	pendingPerms map[string]chan permissionDecision
	permSeq      uint64

	// initialPermMode is the posture applied at thread start; overwritten
	// from SendMessageOpts.PermissionMode on the first OpenAndSend.
	// currentPermMode tracks the live posture (per-turn overrides).
	// Codex has no plan mode; see codexTurnPolicy for the mapping.
	initialPermMode ClaudePermissionMode
	currentPermMode ClaudePermissionMode

	// initialModel is passed on turns when set (codex takes model per-turn,
	// unlike Claude's process-lifetime model).
	initialModel string

	// SystemPrompt carries stack-detected guidance, injected as codex
	// developerInstructions at thread start. Fresh sessions only — the
	// instructions persist in codex thread state across resume and fork.
	SystemPrompt string

	// CodexPath overrides the codex binary location (tests, pinned installs).
	// Empty means look up codexBinaryName on PATH.
	CodexPath string

	ctx    context.Context
	cancel context.CancelFunc
}

// NewCodexBackend creates a codex backend for a fresh session in workDir.
func NewCodexBackend(workDir string) *CodexBackend {
	return NewCodexBackendForSession(workDir, "")
}

// NewCodexBackendForSession is the resume variant: resumeThreadID pre-seeds
// the codex thread id so Open reattaches via thread/resume and Messages can
// serve history. Empty for fresh sessions.
func NewCodexBackendForSession(workDir, resumeThreadID string) *CodexBackend {
	ctx, cancel := context.WithCancel(context.Background())
	return &CodexBackend{
		status:          StatusStarting,
		projectDir:      workDir,
		threadID:        resumeThreadID,
		events:          make(chan Event, 128),
		pendingPerms:    make(map[string]chan permissionDecision),
		initialPermMode: ClaudePermBypass,
		ctx:             ctx,
		cancel:          cancel,
	}
}

// codexTurnPolicy maps clank's permission posture onto codex per-turn
// policy: approval policy + tagged sandbox-policy kind. Codex has no plan
// mode: "plan" degrades to a read-only sandbox with approvals, which
// prevents edits like Claude's plan mode does but never produces an
// ExitPlanMode review. acceptEdits has no codex analog (sandboxed edits
// never prompt anyway) and behaves like default.
func codexTurnPolicy(mode ClaudePermissionMode) (codex.ApprovalPolicy, protocol.SandboxPolicyKind) {
	switch mode {
	case ClaudePermBypass:
		return codex.ApprovalPolicyNever, protocol.SandboxPolicyKindDangerFullAccess
	case ClaudePermPlan:
		return codex.ApprovalPolicyOnRequest, protocol.SandboxPolicyKindReadOnly
	default: // ClaudePermDefault, ClaudePermAcceptEdits, ""
		return codex.ApprovalPolicyOnRequest, protocol.SandboxPolicyKindWorkspaceWrite
	}
}

// rawJSONString marshals a plain string policy value for the protocol's
// json.RawMessage policy fields.
func rawJSONString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// codexSandboxPolicyJSON renders the tagged SandboxPolicy union turn/start
// expects (thread-level fields take plain mode strings; the turn-level field
// rejects them). The bare variant carries protocol defaults — deliberately
// NOT the host's [sandbox_workspace_write] config, so sandbox posture stays
// deterministic across hosts.
func codexSandboxPolicyJSON(kind protocol.SandboxPolicyKind) json.RawMessage {
	b, _ := json.Marshal(map[string]protocol.SandboxPolicyKind{"type": kind})
	return b
}

// Open spawns the codex app-server subprocess, starts the notification pump,
// and creates (or resumes) the thread. Idempotent.
func (b *CodexBackend) Open(ctx context.Context) error {
	b.openMu.Lock()
	defer b.openMu.Unlock()

	b.mu.Lock()
	if b.client != nil {
		b.mu.Unlock()
		return nil
	}
	workDir := b.projectDir
	resumeID := b.threadID
	permMode := b.initialPermMode
	systemPrompt := b.SystemPrompt
	binPath := b.CodexPath
	b.mu.Unlock()

	if workDir == "" {
		return fmt.Errorf("codex backend: project dir is empty; refuse to inherit daemon cwd")
	}
	if binPath == "" {
		binPath = codexBinaryName
	}

	connectStart := time.Now()
	// Spawn under b.ctx (backend lifetime), not the request ctx — the
	// subprocess must outlive this call.
	client, err := codex.New(b.ctx, codex.Options{
		Spawn: codex.SpawnOptions{CodexPath: binPath},
		ClientInfo: protocol.ClientInfo{
			Name:    "clank",
			Version: PinnedCodexVersion,
		},
		ApprovalHandler: &codexApprovals{b: b},
	})
	if err != nil {
		b.setStatus(StatusError)
		return fmt.Errorf("spawn codex app-server: %w", err)
	}

	// Subscribe before thread start so no thread event is missed.
	sub := client.Client().SubscribeNotifications(256)
	go b.notificationPump(sub)

	// Thread-level approval/sandbox are deliberately not set: every turn
	// carries explicit policy (see buildCodexTurnParams), the single
	// mechanism — a thread-level default would go stale after a mid-session
	// mode switch and silently govern any turn that forgot its override.
	var thread *codex.Thread
	if resumeID != "" {
		thread, err = client.ResumeThread(b.ctx, codex.ThreadResumeOptions{
			ThreadID: resumeID,
			Cwd:      workDir,
		})
	} else {
		thread, err = client.StartThread(b.ctx, codex.ThreadStartOptions{
			Cwd: workDir,
			// Fresh sessions only: instructions persist in codex thread
			// state, so resumed/forked threads already carry them.
			DeveloperInstructions: systemPrompt,
		})
	}
	if err != nil {
		client.Close()
		b.setStatus(StatusError)
		if resumeID != "" {
			return fmt.Errorf("resume codex thread %s: %w", resumeID, err)
		}
		return fmt.Errorf("start codex thread: %w", err)
	}
	log.Printf("[codex-open] connect=%s resume=%t dir=%s thread=%s",
		time.Since(connectStart).Round(time.Millisecond), resumeID != "", workDir, thread.ID())

	b.mu.Lock()
	b.client = client
	b.thread = thread
	b.threadID = thread.ID()
	b.currentPermMode = permMode
	starting := b.status == StatusStarting
	b.mu.Unlock()

	if starting {
		// Emit through setStatus so the ExternalID rides an event and the hub
		// persists it (Event.ExternalID is the single source of truth).
		b.setStatus(StatusIdle)
	}
	return nil
}

// Send dispatches a prompt as a new turn. Fast-fails when the session is not
// open. Per-turn approval/sandbox overrides carry the current permission mode.
func (b *CodexBackend) Send(ctx context.Context, opts SendMessageOpts) error {
	b.mu.Lock()
	client := b.client
	threadID := b.threadID
	if opts.PermissionMode != "" && opts.PermissionMode.IsValid() {
		b.currentPermMode = opts.PermissionMode
	}
	mode := b.currentPermMode
	model := b.initialModel
	b.mu.Unlock()

	if client == nil {
		return fmt.Errorf("codex session is not open")
	}

	if opts.Model != nil {
		if opts.Model.ProviderID != "" && opts.Model.ProviderID != codexProviderOpenAI {
			return fmt.Errorf("codex backend cannot run provider %q models", opts.Model.ProviderID)
		}
		model = opts.Model.ModelID
	}

	params, err := buildCodexTurnParams(threadID, mode, model, opts)
	if err != nil {
		return err
	}

	b.setStatus(StatusBusy)
	if _, err := client.Client().TurnStart(ctx, params); err != nil {
		b.setStatus(StatusError)
		return fmt.Errorf("start codex turn: %w", err)
	}
	return nil
}

// buildCodexTurnParams assembles turn/start params. Every turn carries
// explicit approval and sandbox policy — the single policy mechanism (no
// thread-level defaults that could go stale after a mode switch); the unit
// tests pin this invariant.
func buildCodexTurnParams(threadID string, mode ClaudePermissionMode, model string, opts SendMessageOpts) (protocol.TurnStartParams, error) {
	inputs := make([]protocol.TurnStartParamsInputElem, 0, 1+len(opts.Attachments))
	if opts.Text != "" {
		inputs = append(inputs, codex.TextInput(opts.Text))
	}
	for _, att := range opts.Attachments {
		switch {
		case strings.HasPrefix(att.Source, "http://"), strings.HasPrefix(att.Source, "https://"):
			inputs = append(inputs, codex.ImageInput(att.Source))
		case strings.HasPrefix(att.Source, "file://"):
			inputs = append(inputs, codex.LocalImageInput(strings.TrimPrefix(att.Source, "file://")))
		default:
			return protocol.TurnStartParams{}, fmt.Errorf("codex backend does not support attachment source scheme in %q", att.Source)
		}
	}
	if len(inputs) == 0 {
		return protocol.TurnStartParams{}, fmt.Errorf("empty prompt")
	}

	approval, sandboxKind := codexTurnPolicy(mode)
	params := protocol.TurnStartParams{
		ThreadID:       threadID,
		Input:          inputs,
		ApprovalPolicy: rawJSONString(string(approval)),
		SandboxPolicy:  codexSandboxPolicyJSON(sandboxKind),
	}
	if model != "" {
		params.Model = &model
	}
	return params, nil
}

// OpenAndSend opens the session and dispatches the first prompt.
func (b *CodexBackend) OpenAndSend(ctx context.Context, opts SendMessageOpts) error {
	b.mu.Lock()
	if opts.PermissionMode != "" && opts.PermissionMode.IsValid() {
		b.initialPermMode = opts.PermissionMode
	}
	if opts.Model != nil && opts.Model.ModelID != "" {
		b.initialModel = opts.Model.ModelID
	}
	b.mu.Unlock()

	if err := b.Open(ctx); err != nil {
		return err
	}
	return b.Send(ctx, opts)
}

// Abort interrupts the in-flight turn. Pending permission prompts are failed
// first so the RPC dispatch goroutine parked in the approval handler is freed
// before the interrupt round-trip needs it.
func (b *CodexBackend) Abort(ctx context.Context) error {
	b.failPendingPermissions()

	b.mu.Lock()
	client := b.client
	threadID := b.threadID
	turnID := b.activeTurnID
	b.mu.Unlock()

	if client == nil || turnID == "" {
		return nil
	}
	if _, err := client.Client().TurnInterrupt(ctx, protocol.TurnInterruptParams{
		ThreadID: threadID,
		TurnID:   turnID,
	}); err != nil {
		return fmt.Errorf("interrupt codex turn: %w", err)
	}
	return nil
}

// Stop tears down the subprocess and closes the event channel.
func (b *CodexBackend) Stop() error {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return nil
	}
	b.stopped = true
	client := b.client
	b.client = nil
	b.thread = nil
	b.mu.Unlock()

	b.failPendingPermissions()
	b.cancel()
	if client != nil {
		if err := client.Close(); err != nil {
			log.Printf("[codex] close app-server: %v", err)
		}
	}
	b.closeOnce.Do(func() { close(b.events) })
	return nil
}

// Events returns the event stream for this backend.
func (b *CodexBackend) Events() <-chan Event {
	return b.events
}

// Status returns the current session status snapshot.
func (b *CodexBackend) Status() SessionStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

// SessionID returns the codex thread id, or "" before the thread exists.
func (b *CodexBackend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.threadID
}

// Revert is unsupported: codex has no file-checkpoint rollback analog.
func (b *CodexBackend) Revert(ctx context.Context, messageID string) error {
	return fmt.Errorf("codex backend does not support revert")
}

// Fork branches a new codex thread containing history through messageID.
// Assistant message ids are turn ids (see currentMsgID), which is exactly the
// cutoff thread/fork takes; forking from a user message is rejected.
func (b *CodexBackend) Fork(ctx context.Context, messageID string) (ForkResult, error) {
	if err := b.Open(ctx); err != nil {
		return ForkResult{}, err
	}
	b.mu.Lock()
	client := b.client
	threadID := b.threadID
	workDir := b.projectDir
	b.mu.Unlock()

	// No thread-level policy on the fork either — the forked session's turns
	// carry explicit policy, same as ours (see buildCodexTurnParams).
	params := protocol.ThreadForkParams{
		ThreadID: threadID,
		Cwd:      &workDir,
	}
	if messageID != "" {
		params.LastTurnID = &messageID
	}
	resp, err := client.Client().ThreadFork(ctx, params)
	if err != nil {
		return ForkResult{}, fmt.Errorf("fork codex thread at %q: %w", messageID, err)
	}
	forkedID := codexThreadIDFromResponse(*resp)
	if forkedID == "" {
		return ForkResult{}, fmt.Errorf("fork codex thread: response carries no thread id")
	}
	return ForkResult{ID: forkedID}, nil
}

// codexThreadIDFromResponse digs the new thread id out of a fork response,
// which the SDK types loosely (generated fallback). Re-marshal and pick the
// known field shapes.
func codexThreadIDFromResponse(resp any) string {
	raw, err := json.Marshal(resp)
	if err != nil {
		return ""
	}
	var v struct {
		ID     string `json:"id"`
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	if v.Thread.ID != "" {
		return v.Thread.ID
	}
	return v.ID
}

// setStatus updates the status and emits a StatusChange event.
func (b *CodexBackend) setStatus(s SessionStatus) {
	b.mu.Lock()
	old := b.status
	if old == s {
		b.mu.Unlock()
		return
	}
	b.status = s
	b.mu.Unlock()

	b.emit(Event{
		Type:      EventStatusChange,
		Timestamp: time.Now(),
		Data:      StatusChangeData{OldStatus: old, NewStatus: s},
	})
}

// emit delivers an event to the channel, stamping the thread id as
// ExternalID (the hub persists it from events — the single source of truth).
// Drops events once stopped.
func (b *CodexBackend) emit(ev Event) {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return
	}
	ev.ExternalID = b.threadID
	b.mu.Unlock()

	select {
	case b.events <- ev:
	case <-b.ctx.Done():
	}
}
