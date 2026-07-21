package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	claudecode "github.com/severity1/claude-agent-sdk-go"
)

// activeToolBlock tracks metadata for an in-progress tool_use block so that
// handleContentBlockStop can emit a PartCompleted event with the correct ID
// and tool name (the TUI replaces the entire toolPart on upsert).
type activeToolBlock struct {
	partID   string
	tool     string
	inputBuf strings.Builder // accumulates input_json_delta chunks
}

// ClaudeCodeBackend manages a single Claude Code session using the
// claude-agent-sdk-go SDK's Client API. The SDK handles CLI discovery,
// subprocess lifecycle, JSON parsing, streaming, and the control protocol.
//
// Architecture:
//   - Each backend instance corresponds to one session.
//   - Connect() spawns a persistent Claude CLI subprocess.
//   - Multi-turn: Query() sends follow-up prompts over the same connection.
//   - Abort uses the SDK's control protocol (Interrupt), not raw SIGINT.
//   - receiveLoop maps SDK messages → clank Event types.
//   - Messages() reads the full history from Claude's on-disk JSONL transcript
//     via the SDK's GetSessionMessages, so history survives daemon restarts.
type ClaudeCodeBackend struct {
	mu         sync.Mutex
	openMu     sync.Mutex // serializes Open() so check-and-create is atomic
	status     SessionStatus
	sessionID  string // Claude's CLI session UUID (from ResultMessage)
	projectDir string
	events     chan Event
	stopped    bool // guards against double-close of events channel
	aborting   bool // set by Abort so the interrupt's error result maps to Idle, not Error
	ctx        context.Context
	cancel     context.CancelFunc

	client claudecode.Client // SDK client (persistent connection)

	// currentMsgID is the Anthropic API message ID (e.g. "msg_01XFD...") extracted
	// from the most recent message_start stream event. It's used to build part IDs
	// for text/thinking blocks as "{msgID}-{blockIndex}".
	//
	// This is naturally unique across both message cycles within a turn (each tool
	// use triggers a new API call with a new message ID) and across turns (each
	// Query() produces new API calls). No synthetic counters needed.
	//
	// Only accessed from receiveLoop goroutine — no lock required.
	currentMsgID string

	// activeToolBlocks maps block index → tool metadata for the current message
	// cycle. Populated by handleContentBlockStart for tool_use blocks, consumed
	// by handleContentBlockStop to emit PartCompleted with the correct ID and
	// tool name (fixing the stuck spinner and blank tool label).
	// Reset on each message_start since block indices restart at 0 per cycle.
	// Only accessed from receiveLoop goroutine — no lock required.
	activeToolBlocks map[int]*activeToolBlock

	// ClientFactory builds a claudecode.Client for a given set of options.
	// Tests inject a factory that returns a client backed by a mock transport.
	// If nil, the default claudecode.NewClient is used.
	ClientFactory func(opts ...claudecode.Option) claudecode.Client

	// ExtraEnv is appended to the spawned claude subprocess's environment.
	// Host plumbing populates this with CLAUDE_CODE_OAUTH_TOKEN (subscription)
	// or ANTHROPIC_API_KEY (Console) when the user has connected an Anthropic
	// provider via AuthManager. Empty when no Anthropic credential is set —
	// claude falls back to its own keychain/OAuth login in that case.
	ExtraEnv map[string]string

	// SystemPrompt, when non-empty, is appended to Claude's base system prompt
	// (via --append-system-prompt) at CLI launch — carrying stack-detected
	// guidance. Set by the host for fresh sessions only; empty on resume, where
	// the guidance already shaped the conversation being continued.
	SystemPrompt string

	// initialModel, if non-empty, is passed to the spawned CLI as
	// --model (via claudecode.WithModel). Claude's CLI takes model
	// at process start, not per-message, so this is the model the
	// whole session runs on. Set from SendMessageOpts.Model on the
	// first OpenAndSend; ignored on subsequent sends in the same
	// session (the CLI's model is fixed for the process's lifetime).
	initialModel string

	// initialPermMode is the permission posture the session should run in once
	// open. Defaults to ClaudePermBypass — the product default for new sessions
	// — and is overwritten from SendMessageOpts.PermissionMode on the first
	// OpenAndSend. NB: the CLI is always *launched* in bypassPermissions (so it
	// carries the "bypass is available" capability — see Open); Open then
	// restricts to this mode at runtime before the first prompt.
	initialPermMode ClaudePermissionMode

	// currentPermMode tracks the mode currently in effect so Send only issues a
	// SetPermissionMode control call when the user actually changed it.
	// Guarded by b.mu.
	currentPermMode ClaudePermissionMode

	// revertMessageID is the user-message UUID the session is currently
	// reverted to — i.e. tracked files were rolled back to that checkpoint via
	// RewindFiles. Empty when not reverted. Set by Revert, cleared by the next
	// Send (mirroring the TUI, which clears RevertMessageID on a new prompt).
	// In-memory only; not persisted. Guarded by b.mu.
	revertMessageID string

	// pendingPerms maps a synthesized permission request ID to the channel that
	// delivers the user's decision. handleCanUseTool registers an entry and
	// blocks on it; RespondPermission resolves it. Guarded by b.mu.
	pendingPerms map[string]chan permissionDecision

	// permSeq generates unique permission request IDs. Guarded by b.mu.
	permSeq uint64

	// lastToolUseID maps a tool name to the id of the most recent tool_use block
	// the assistant emitted for it (from the stream). handleCanUseTool reads it
	// to stamp the permission with the tool_use id its UI card is keyed by.
	// The permission request for a tool always follows its tool_use block, so
	// the latest id per name is the one being gated. Guarded by b.mu.
	lastToolUseID map[string]string

	// pendingQuestions maps a question request ID to the normalized questions
	// awaiting RespondQuestion. A routing cache, not the source of truth: a
	// "q-<tool_use_id>" request that misses here is recovered from the tagged
	// transcript part (questionFromTranscript). Guarded by b.mu.
	pendingQuestions map[string]claudeQuestion

	// aiTitleEmitted is set once the CLI-generated session title has been read
	// from the transcript and published via EventTitleChange. The CLI keeps the
	// title stable for a session's life, so reading stops after the first emit.
	// Guarded by b.mu — Revert can spawn a new receiveLoop before the old one's
	// in-flight handleResult call has returned.
	aiTitleEmitted bool

	// aiTitleRecheckActive is set while a post-turn title recheck goroutine is
	// running, so overlapping handleResult calls schedule at most one. Guarded
	// by b.mu.
	aiTitleRecheckActive bool

	// AITitleRecheckDelay overrides the interval between post-turn ai-title
	// re-reads (see scheduleAITitleRecheck). Test hook like ClientFactory;
	// zero means the production default.
	AITitleRecheckDelay time.Duration
}

// NewClaudeCodeBackend creates a new Claude Code backend. workDir is
// the host-resolved working directory (worktree or repo root) the
// claude CLI will be launched in.
func NewClaudeCodeBackend(workDir string) *ClaudeCodeBackend {
	return NewClaudeCodeBackendForSession(workDir, "")
}

// NewClaudeCodeBackendForSession is the resume variant. It pre-seeds the
// SDK session ID so that Messages() can read the on-disk JSONL transcript
// immediately, and so that Open() launches the CLI with --resume to
// reattach the persistent subprocess for the existing conversation.
// resumeSessionID may be empty for fresh sessions. A pending revert is set
// live by Revert (and conveyed to clients via EventRevertChange); it is not
// seeded here, since truncation bakes the reverted state into the transcript.
func NewClaudeCodeBackendForSession(workDir, resumeSessionID string) *ClaudeCodeBackend {
	ctx, cancel := context.WithCancel(context.Background())
	return &ClaudeCodeBackend{
		status:           StatusStarting,
		projectDir:       workDir,
		sessionID:        resumeSessionID,
		events:           make(chan Event, 128),
		activeToolBlocks: make(map[int]*activeToolBlock),
		pendingPerms:     make(map[string]chan permissionDecision),
		pendingQuestions: make(map[string]claudeQuestion),
		lastToolUseID:    make(map[string]string),
		initialPermMode:  ClaudePermBypass,
		ctx:              ctx,
		cancel:           cancel,
	}
}

// Open spawns the Claude CLI subprocess (via the SDK) and starts the
// receiveLoop. If the constructor was given a resumeSessionID, the CLI
// is launched with --resume so the JSONL transcript is reattached.
//
// Idempotent: a second call while already connected returns nil. openMu
// serializes the check-and-create so concurrent callers can't both spawn
// a CLI subprocess and orphan one of them.
func (b *ClaudeCodeBackend) Open(ctx context.Context) error {
	b.openMu.Lock()
	defer b.openMu.Unlock()

	b.mu.Lock()
	if b.client != nil {
		b.mu.Unlock()
		return nil
	}
	workDir := b.projectDir
	resumeID := b.sessionID
	factory := b.ClientFactory
	extraEnv := b.ExtraEnv
	model := b.initialModel
	permMode := b.initialPermMode
	systemPrompt := b.SystemPrompt
	b.mu.Unlock()

	// Defensive guard: an empty workDir would silently inherit the
	// daemon's cwd via claudecode.WithCwd(""), which usually means a
	// caller forgot to populate ProjectDir before constructing the
	// backend. Fail fast instead of running the agent against the
	// wrong tree.
	if workDir == "" {
		return fmt.Errorf("claude backend: project dir is empty; refuse to inherit daemon cwd")
	}

	opts := []claudecode.Option{
		claudecode.WithCwd(workDir),
		claudecode.WithPartialStreaming(),
		// Track file changes per user message so Revert can roll them back via
		// the SDK's RewindFiles. Sets CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING=1
		// on the CLI. Enabled unconditionally: checkpoints must exist from
		// session start for any later turn to be revertible, and the flag is
		// inert until RewindFiles is called.
		claudecode.WithFileCheckpointing(),
		// Always launch with --dangerously-skip-permissions so the session
		// carries the "bypass is available" capability. The CLI refuses a
		// runtime switch to bypassPermissions unless the process was launched
		// with this flag ("Cannot set permission mode to bypassPermissions
		// because the session was not launched with --dangerously-skip-
		// permissions"). Without it a session started in plan mode could never
		// switch to build: the switch fast-errored and flipped the session to
		// StatusError. The flag only grants the capability and sets the *launch*
		// mode to bypass — we restrict to the user's actual mode just below,
		// before any prompt runs, so plan/default/acceptEdits sessions never
		// execute a turn with bypass active.
		claudecode.WithExtraArgs(map[string]*string{"dangerously-skip-permissions": nil}),
		// Route tool-permission requests through clank's prompt UI. Without a
		// callback the SDK auto-denies every request, so this is required for
		// the default/acceptEdits/plan modes to work at all.
		claudecode.WithCanUseTool(b.handleCanUseTool),
	}
	if resumeID != "" {
		// Plain --resume: Revert physically truncates the transcript, so the
		// resumed conversation is already a single linear chain — no
		// --resume-session-at pin needed to select a branch.
		opts = append(opts, claudecode.WithResume(resumeID))
	}
	extraEnv = buildExtraEnv(os.Geteuid(), extraEnv)
	if len(extraEnv) > 0 {
		opts = append(opts, claudecode.WithEnv(extraEnv))
	}
	if model != "" {
		opts = append(opts, claudecode.WithModel(model))
	}
	// Append (not replace) so Claude's base system prompt + tool behavior stay
	// intact. Fresh sessions only — resumeID != "" means the guidance already
	// shaped the conversation being resumed.
	if resumeID == "" && systemPrompt != "" {
		opts = append(opts, claudecode.WithAppendSystemPrompt(systemPrompt))
	}

	// Build the client — use the test factory if provided.
	var client claudecode.Client
	if factory != nil {
		client = factory(opts...)
	} else {
		client = claudecode.NewClient(opts...)
	}

	// Only commit b.client after a successful Connect, so a failed Open
	// leaves the backend retryable instead of stuck in a half-open state.
	// Connect spawns the CLI subprocess — on a cold machine this is the
	// dominant session-open cost, so its duration is always logged.
	connectStart := time.Now()
	if err := client.Connect(b.ctx); err != nil {
		log.Printf("[claude-open] connect failed after %s (resume=%t dir=%s): %v",
			time.Since(connectStart).Round(time.Millisecond), resumeID != "", workDir, err)
		b.setStatus(StatusError)
		return fmt.Errorf("connect to claude CLI: %w", err)
	}
	connectDur := time.Since(connectStart)

	// The launch flag forces the active mode to bypassPermissions. Restrict to
	// the user's actual mode now — before OpenAndSend issues the first Query —
	// so a plan/default/acceptEdits session never runs a prompt with bypass
	// active. No tool runs between Connect and here, so the momentary bypass
	// window is safe. A failed restrict is fatal: better to fail than to run a
	// plan-requested session with full permissions. bypass needs no restrict —
	// it is already the launch mode.
	//
	// Commit b.client only after the restrict succeeds. A non-nil b.client makes
	// the next Open early-return nil (session looks ready) while the CLI keeps
	// running, so committing before the restrict would both leak the subprocess
	// and wedge the backend half-open on failure. Disconnect on failure so the
	// retry starts from a clean slate.
	var restrictDur time.Duration
	if permMode != ClaudePermBypass {
		restrictStart := time.Now()
		if err := client.SetPermissionMode(b.ctx, claudecode.PermissionMode(permMode)); err != nil {
			log.Printf("[claude-open] restrict to %q failed after %s: %v",
				permMode, time.Since(restrictStart).Round(time.Millisecond), err)
			b.setStatus(StatusError)
			client.Disconnect()
			return fmt.Errorf("restrict to %q permission mode: %w", permMode, err)
		}
		restrictDur = time.Since(restrictStart)
	}
	log.Printf("[claude-open] connect=%s restrict=%s resume=%t dir=%s",
		connectDur.Round(time.Millisecond), restrictDur.Round(time.Millisecond), resumeID != "", workDir)

	b.mu.Lock()
	b.client = client
	b.currentPermMode = permMode
	b.mu.Unlock()

	// Connection established. Transition out of StatusStarting so the
	// TUI's spinner clears for re-attached sessions (those that only
	// call Open without a follow-up Send/OpenAndSend). For the create
	// path, OpenAndSend has already flipped status to Busy before
	// calling us, so this conditional leaves it alone.
	b.mu.Lock()
	if b.status == StatusStarting {
		b.mu.Unlock()
		b.setStatus(StatusIdle)
	} else {
		b.mu.Unlock()
	}

	// Start receiving messages from the SDK in the background.
	go b.receiveLoop()

	// Catch-up: a resumed transcript may already carry a CLI-generated
	// ai-title that was never surfaced — the turn that would have read it in
	// handleResult died first (daemon restart, machine idle-exit mid-turn) or
	// finished before the CLI's concurrent titling landed. Goroutine keeps
	// transcript I/O off the Open critical path.
	if resumeID != "" {
		go b.maybeEmitAITitle()
	}
	return nil
}

// AgentModelFable is the fable family alias (Claude Fable 5). The
// pinned claude-agent-sdk-go doesn't ship this constant yet;
// AgentModel is a plain string the SDK forwards to `claude --model`
// unvalidated, and the CLI accepts the alias since 2.1.170 (below
// PinnedClaudeVersion). Drop this once the SDK adds AgentModelFable.
const AgentModelFable claudecode.AgentModel = "fable"

// validClaudeModels is the closed set the claude CLI accepts via
// --model. `inherit` is omitted on purpose — see ClaudeBackendManager
// in internal/host/backends.go for the same rationale: it's a "use
// whatever the caller's default is" passthrough, not a user pick.
//
// Duplicated rather than imported from internal/host to keep the
// dependency direction host → agent. Bump claude-agent-sdk-go to add
// new tiers; this set must stay in sync with claudeModelCatalog.
var validClaudeModels = map[string]struct{}{
	string(claudecode.AgentModelSonnet): {},
	string(claudecode.AgentModelOpus):   {},
	string(claudecode.AgentModelHaiku):  {},
	string(AgentModelFable):             {},
}

// IsValidClaudeModel reports whether id is one of the family aliases
// the claude CLI's --model flag accepts. Returns false for the empty
// string so callers can use it as a "should I pass --model at all"
// check without a separate empty guard.
func IsValidClaudeModel(id string) bool {
	_, ok := validClaudeModels[id]
	return ok
}

// OpenAndSend opens the session and dispatches the initial prompt. The
// CLI subprocess is connected before the prompt is queued so the
// receiveLoop is in place to observe SystemMessage{init} (which carries
// the external session ID).
//
// Unlike Send, OpenAndSend does NOT emit an EventMessage{Role:user} for
// the prompt: the hub already persists the initial prompt out-of-band
// when creating the session, so emitting one here would duplicate it in
// the TUI history. Follow-up prompts go through Send and DO emit the
// user message because there's no other place that records them.
//
// Query is dispatched on b.ctx (not the caller's ctx) so the prompt is
// tied to backend lifetime, not request lifetime, mirroring OpenCode's
// Send. The actual LLM response streams via receiveLoop on b.ctx, so
// honoring the caller's ctx for the synchronous handoff would only
// surface "cancel mid-enqueue" as a spurious StatusError without
// stopping the work that has already started.
func (b *ClaudeCodeBackend) OpenAndSend(ctx context.Context, opts SendMessageOpts) error {
	// Validate the requested model up front. Without this check, a
	// stale opencode-flavored ModelID (e.g. "claude-opus-4.7" from
	// the github-copilot provider) would be forwarded blindly as
	// `claude --model claude-opus-4.7` and the CLI would crash with
	// a cryptic upstream error. Fail fast with a clear message
	// instead. Per AGENTS.md §2: no fallbacks for missing/invalid
	// parameters — clear API contracts over silent corrections.
	if opts.Model != nil && opts.Model.ModelID != "" && !IsValidClaudeModel(opts.Model.ModelID) {
		b.setStatus(StatusError)
		return fmt.Errorf("claude backend: model %q is not a valid claude CLI model (valid: sonnet, opus, haiku, fable)", opts.Model.ModelID)
	}

	// Mark Busy before Open so Open's "Starting → Idle" conditional
	// is bypassed on the create path; we go directly Starting → Busy.
	b.setStatus(StatusBusy)

	// Capture the picked model before Open spawns the CLI — claude
	// takes --model at process start. Subsequent Send calls within
	// the same session can't change the model (the CLI's flag is
	// fixed for the process's lifetime); we ignore opts.Model there
	// rather than restart the subprocess silently.
	if opts.Model != nil && opts.Model.ModelID != "" {
		b.mu.Lock()
		b.initialModel = opts.Model.ModelID
		b.mu.Unlock()
	}

	// Capture the initial permission mode before Open spawns the CLI. Unlike
	// model, the mode can change later (Send issues SetPermissionMode), but the
	// launch value still comes from this first OpenAndSend.
	if opts.PermissionMode != "" {
		if !opts.PermissionMode.IsValid() {
			b.setStatus(StatusError)
			return fmt.Errorf("claude backend: %q is not a valid permission mode", opts.PermissionMode)
		}
		b.mu.Lock()
		b.initialPermMode = opts.PermissionMode
		b.mu.Unlock()
	}

	if err := b.Open(ctx); err != nil {
		return err
	}

	b.mu.Lock()
	client := b.client
	b.mu.Unlock()
	if client == nil {
		return fmt.Errorf("session not open after Open")
	}

	imgs, err := resolveAttachments(ctx, opts.Attachments)
	if err != nil {
		b.setStatus(StatusIdle)
		return fmt.Errorf("resolve attachments: %w", err)
	}

	if err := b.dispatchClaudeQuery(client, opts.Text, imgs); err != nil {
		// Surface a reason so the client can show a recoverable error banner
		// ([VIEW-ERROR-001]); a bare setStatus(StatusError) leaves the session
		// red with no explanation.
		b.emitError(fmt.Sprintf("send initial prompt: %v", err))
		b.setStatus(StatusError)
		return fmt.Errorf("send initial prompt: %w", err)
	}
	return nil
}

// Send dispatches a prompt to an already-Open session. Query runs on
// b.ctx (not the caller's ctx) — see OpenAndSend's doc for the rationale.
func (b *ClaudeCodeBackend) Send(ctx context.Context, opts SendMessageOpts) error {
	b.mu.Lock()
	client := b.client
	b.mu.Unlock()

	if client == nil {
		return fmt.Errorf("session not open: client not connected")
	}

	// A pending permission prompt has parked handleCanUseTool on the SDK's
	// single read goroutine, so no control round-trip (SetPermissionMode) or
	// follow-up Query can be serviced until it is answered — issuing one would
	// block for the control timeout and then flip the session to StatusError.
	// Fast-fail instead: while a prompt is open the only valid actions are
	// RespondPermission or Abort. The TUI already locks out sends here; this
	// guards clients that don't (e.g. the mobile app).
	b.mu.Lock()
	pending := len(b.pendingPerms)
	b.mu.Unlock()
	if pending > 0 {
		return fmt.Errorf("cannot send while %d permission prompt(s) pending: answer or abort first", pending)
	}

	// Apply a mode change before dispatching the prompt.
	if opts.PermissionMode != "" {
		if !opts.PermissionMode.IsValid() {
			return fmt.Errorf("claude backend: %q is not a valid permission mode", opts.PermissionMode)
		}
		b.mu.Lock()
		changed := opts.PermissionMode != b.currentPermMode
		b.mu.Unlock()
		if changed {
			if err := client.SetPermissionMode(b.ctx, claudecode.PermissionMode(opts.PermissionMode)); err != nil {
				// A failed runtime mode switch must NOT kill the session. The
				// read goroutine isn't parked (the pending-prompt guard above
				// already returned), so the conversation can still continue in
				// the current mode. Surface the error so the client can bounce
				// the send, but leave status intact so the session stays usable.
				// With the launch-time --dangerously-skip-permissions flag, all
				// four modes are now switchable, so this path is not expected.
				return fmt.Errorf("set permission mode: %w", err)
			}
			b.mu.Lock()
			b.currentPermMode = opts.PermissionMode
			b.mu.Unlock()
		}
	}

	// Download attachments before emitting the user event / flipping to busy,
	// so a bad image fails the send cleanly (status intact, session usable)
	// rather than surfacing a phantom user message followed by an error.
	imgs, err := resolveAttachments(ctx, opts.Attachments)
	if err != nil {
		return fmt.Errorf("resolve attachments: %w", err)
	}

	// A new prompt supersedes any active revert: drop the reverted boundary and
	// announce the unrevert so non-TUI clients un-hide the messages (the TUI
	// already clears its own copy locally on send). Only emit when state changed.
	b.mu.Lock()
	wasReverted := b.revertMessageID != ""
	b.revertMessageID = ""
	b.mu.Unlock()
	if wasReverted {
		b.emit(Event{
			Type:      EventRevertChange,
			Timestamp: time.Now(),
			Data:      RevertChangeData{MessageID: ""},
		})
	}

	// Emit user message event so the TUI sees it.
	b.emit(Event{
		Type:      EventMessage,
		Timestamp: time.Now(),
		Data: MessageData{
			Role:    "user",
			Content: opts.Text,
		},
	})

	// A new turn clears any pending abort so an earlier interrupt that produced
	// no result can't reinterpret this turn's genuine error as a clean Idle.
	b.mu.Lock()
	b.aborting = false
	b.mu.Unlock()

	b.setStatus(StatusBusy)

	if err := b.dispatchClaudeQuery(client, opts.Text, imgs); err != nil {
		// Surface a reason so the client can show a recoverable error banner
		// ([VIEW-ERROR-001]) instead of a silent red status with the user's text
		// bounced back.
		b.emitError(fmt.Sprintf("send prompt: %v", err))
		b.setStatus(StatusError)
		return fmt.Errorf("send prompt: %w", err)
	}

	return nil
}

func (b *ClaudeCodeBackend) Abort(ctx context.Context) error {
	b.mu.Lock()
	client := b.client
	b.mu.Unlock()

	if client == nil {
		return fmt.Errorf("session not started")
	}

	// Mark the turn user-aborted so handleResult reads the interrupt's error
	// result as a normal turn end (Idle) rather than a session failure.
	b.mu.Lock()
	b.aborting = true
	b.mu.Unlock()

	// Free any parked permission prompt so the SDK read goroutine unblocks
	// immediately instead of waiting for the interrupt to propagate.
	b.failPendingPermissions()

	if err := client.Interrupt(ctx); err != nil {
		b.mu.Lock()
		b.aborting = false
		b.mu.Unlock()
		return err
	}
	return nil
}

func (b *ClaudeCodeBackend) Stop() error {
	b.cancel()

	b.mu.Lock()
	client := b.client
	alreadyStopped := b.stopped
	b.stopped = true
	// Close under b.mu, mutually exclusive with emit()'s non-blocking send, so
	// an in-flight emit can never send on the closed channel. Setting stopped
	// before the close means any emit that grabs the lock after this returns
	// early instead of sending.
	if !alreadyStopped {
		close(b.events)
	}
	b.mu.Unlock()

	if client != nil {
		client.Disconnect()
	}

	return nil
}

func (b *ClaudeCodeBackend) Events() <-chan Event {
	return b.events
}

func (b *ClaudeCodeBackend) Status() SessionStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

func (b *ClaudeCodeBackend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

// Messages returns the conversation history for this session by reading
// Claude Code's on-disk JSONL transcript via the SDK's GetSessionMessages.
// History therefore survives daemon restarts and matches what `claude --resume`
// would replay.
//
// Returns nil, nil before the SDK has assigned a session ID (i.e. before the
// first ResultMessage / system init has landed). The caller can call this
// again once a session ID is available.
func (b *ClaudeCodeBackend) Messages(ctx context.Context) ([]MessageData, error) {
	b.mu.Lock()
	sessionID := b.sessionID
	workDir := b.projectDir
	revertID := b.revertMessageID
	b.mu.Unlock()

	msgs, err := ReadClaudeTranscript(ctx, workDir, sessionID)
	if err != nil {
		return nil, err
	}
	// While a revert is active but the user hasn't re-prompted yet, the CLI hasn't
	// branched the transcript, so the on-disk JSONL still holds the reverted tail.
	// Truncate the reload at the revert target so the hidden messages don't
	// reappear on a fresh fetch. Once the next Send branches the transcript,
	// revertMessageID is cleared and GetSessionMessages returns the active branch.
	if revertID != "" {
		for i, m := range msgs {
			if m.ID == revertID {
				msgs = msgs[:i]
				break
			}
		}
	}
	return msgs, nil
}

// ReadClaudeTranscript returns the conversation recorded in Claude Code's
// on-disk JSONL transcript for sessionID, coalesced into clank MessageData.
// A pure disk read — no CLI subprocess, no SDK client — so the host can
// serve history for sessions with no live backend. An empty sessionID
// returns (nil, nil): the session has no transcript yet.
func ReadClaudeTranscript(_ context.Context, workDir, sessionID string) ([]MessageData, error) {
	if sessionID == "" {
		return nil, nil
	}
	opts := []claudecode.SessionOption{}
	if workDir != "" {
		opts = append(opts, claudecode.WithSessionDirectory(workDir))
	}
	sdkMsgs, err := claudecode.GetSessionMessages(sessionID, opts...)
	if err != nil {
		return nil, fmt.Errorf("read claude session %s: %w", sessionID, err)
	}
	return coalesceSessionMessages(sdkMsgs), nil
}

// Revert undoes a turn: it rolls tracked files back to their state at the given
// user message AND truncates the conversation there, so the model no longer has
// the reverted turns in context.
//
// messageID is the user message's transcript UUID (== MessageData.ID for user
// rows; see coalesceSessionMessages). Files are restored via RewindFiles
// (control subtype "rewind_files"). The conversation is truncated by relaunching
// the CLI resumed at the assistant message preceding messageID via
// --resume-session-at, which keeps messages up to that turn and branches the
// transcript; the reverted tail stays in the JSONL as an orphaned branch that
// GetSessionMessages no longer follows, and the next prompt continues from the
// kept point.
//
// The conversation truncation is best-effort: if the transcript can't be read or
// messageID is the first turn (no prior assistant message to resume at), the
// file rollback and display truncation still stand. Requires the session to be
// Open (RewindFiles needs the live streaming client). Service.RevertSession
// opens the backend first.
func (b *ClaudeCodeBackend) Revert(ctx context.Context, messageID string) error {
	if messageID == "" {
		return fmt.Errorf("claude backend: revert requires a message id")
	}

	b.mu.Lock()
	client := b.client
	sessionID := b.sessionID
	workDir := b.projectDir
	b.mu.Unlock()
	if client == nil {
		return fmt.Errorf("session not open: cannot revert")
	}

	// Roll the files back first, while the live (full-context) client is up.
	if err := client.RewindFiles(ctx, messageID); err != nil {
		return fmt.Errorf("rewind files to %s: %w", messageID, err)
	}

	// Find the assistant turn to keep, physically truncate the transcript there
	// (dropping the reverted turns), then relaunch with a plain --resume. We do
	// NOT use --resume-session-at: in streaming mode it branches the transcript
	// but does not set the active leaf, so a later plain --resume resumes the
	// wrong branch. A physically-linear transcript resumes unambiguously and is
	// durable across restarts. Best-effort: a first-turn revert (no prior
	// assistant) leaves keepUUID empty and we skip truncation.
	if keepUUID := resumeTargetUUID(readSessionMessages(sessionID, workDir), messageID); keepUUID != "" {
		// Disconnect the live client before rewriting its transcript file, then
		// reopen on the truncated transcript with a plain --resume.
		b.mu.Lock()
		old := b.client
		b.client = nil
		b.mu.Unlock()
		if old != nil {
			old.Disconnect()
		}
		if err := truncateTranscriptAt(sessionID, keepUUID); err != nil {
			return fmt.Errorf("truncate transcript at %s: %w", keepUUID, err)
		}
		if err := b.Open(ctx); err != nil {
			return fmt.Errorf("reopen after truncation: %w", err)
		}
	}

	// Commit the revert state and announce it only after the rewind succeeds.
	b.mu.Lock()
	b.revertMessageID = messageID
	b.mu.Unlock()

	b.emit(Event{
		Type:      EventRevertChange,
		Timestamp: time.Now(),
		Data:      RevertChangeData{MessageID: messageID},
	})
	return nil
}

// truncateTranscriptAt physically rewrites the session's on-disk JSONL
// transcript to keep only the records up to and including the keepUUID record,
// dropping everything after it. This is how a revert drops the reverted turns
// durably: streaming --resume-session-at branches the transcript but does not
// set the active leaf, so a plain --resume would resume the wrong branch.
// Physically truncating leaves a linear transcript that plain --resume resumes
// unambiguously.
func truncateTranscriptAt(sessionID, keepUUID string) error {
	path, err := sessionTranscriptPath(sessionID)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read transcript: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	keepIdx := -1
	for i, line := range lines {
		var rec struct {
			UUID string `json:"uuid"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.UUID == keepUUID {
			keepIdx = i
		}
	}
	if keepIdx < 0 {
		return fmt.Errorf("keep uuid %s not found in transcript %s", keepUUID, path)
	}
	kept := strings.Join(lines[:keepIdx+1], "\n") + "\n"
	return os.WriteFile(path, []byte(kept), 0o644)
}

// sessionTranscriptPath locates a session's JSONL transcript by globbing the
// projects dir, which avoids depending on how the CLI encodes the cwd into the
// project-dir name (it can normalize cwd differently than a naive encoding).
func sessionTranscriptPath(sessionID string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(claudeConfigHome(), "projects", "*", sessionID+".jsonl"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("transcript not found for session %s", sessionID)
	}
	return matches[0], nil
}

// claudeConfigHome returns the base dir for the CLI's projects/ store
// (~/.claude, or $CLAUDE_CONFIG_DIR when set).
func claudeConfigHome() string {
	if cfg := os.Getenv("CLAUDE_CONFIG_DIR"); cfg != "" {
		return cfg
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// readSessionMessages reads the on-disk transcript for a session, returning nil
// on any error (callers treat the conversation-truncation step as best-effort).
func readSessionMessages(sessionID, workDir string) []claudecode.SessionMessage {
	if sessionID == "" {
		return nil
	}
	opts := []claudecode.SessionOption{}
	if workDir != "" {
		opts = append(opts, claudecode.WithSessionDirectory(workDir))
	}
	msgs, err := claudecode.GetSessionMessages(sessionID, opts...)
	if err != nil {
		return nil
	}
	return msgs
}

// resumeTargetUUID returns the transcript UUID of the assistant message that
// --resume-session-at should keep when reverting to the user message userUUID —
// the last assistant message before it. Returns "" when userUUID is the first
// turn or is absent from msgs. It reads the assistant UUID directly (not via
// coalesceSessionMessages, which maps assistant ids to the Anthropic API message
// id) because --resume-session-at matches on the transcript UUID.
func resumeTargetUUID(msgs []claudecode.SessionMessage, userUUID string) string {
	idx := -1
	for i, m := range msgs {
		if m.UUID == userUUID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ""
	}
	for i := idx - 1; i >= 0; i-- {
		if msgs[i].IsMeta {
			continue
		}
		if msgs[i].Type == "assistant" {
			return msgs[i].UUID
		}
	}
	return ""
}

func (b *ClaudeCodeBackend) Fork(ctx context.Context, messageID string) (ForkResult, error) {
	return ForkResult{}, fmt.Errorf("fork is not supported by Claude Code backend")
}

// RespondPermission lives in claude_permissions.go alongside the CanUseTool
// bridge it resolves.

// --- Internal helpers ---

func (b *ClaudeCodeBackend) setStatus(s SessionStatus) {
	b.mu.Lock()
	old := b.status
	b.status = s
	b.mu.Unlock()

	if old != s {
		b.emit(Event{
			Type:      EventStatusChange,
			Timestamp: time.Now(),
			Data: StatusChangeData{
				OldStatus: old,
				NewStatus: s,
			},
		})
	}
}

// TODO(ai-review): emit/Stop TOCTOU — stopped is read under lock but the channel send happens after unlock; Stop can close b.events in that window causing "send on closed channel". Fix with sync.Once close + recover guard. https://github.com/Acksell/clank/pull/68#discussion_r3446660408
// emit reports whether evt was actually sent — false if the backend was
// stopped or the events buffer was full, so callers with resolve-once
// semantics (e.g. maybeEmitAITitle) can retry instead of marking done.
func (b *ClaudeCodeBackend) emit(evt Event) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.stopped {
		return false
	}

	// Stamp the backend's native session ID on every event so the
	// host→hub HTTP boundary can propagate it without bespoke signalling.
	// Empty until the first SystemMessage{init} arrives; once set it
	// rides every subsequent event for free.
	if evt.ExternalID == "" {
		evt.ExternalID = b.sessionID
	}

	// Send under b.mu so it can't interleave with Stop()'s close(b.events).
	// The select's default keeps the send non-blocking, so holding the lock
	// across it never stalls — and holding it makes send and close mutually
	// exclusive, which is what prevents the send-on-closed-channel race.
	select {
	case b.events <- evt:
		return true
	default:
		// Drop if buffer full — avoids blocking the receive loop.
		return false
	}
}

func (b *ClaudeCodeBackend) emitError(msg string) {
	b.emit(Event{
		Type:      EventError,
		Timestamp: time.Now(),
		Data:      ErrorData{Message: msg},
	})
}

// receiveLoop reads messages from the SDK's ReceiveMessages channel and
// translates them into clank Event types. It runs for the lifetime of the
// client connection.
func (b *ClaudeCodeBackend) receiveLoop() {
	msgChan := b.client.ReceiveMessages(b.ctx)

	for msg := range msgChan {
		if msg == nil {
			continue
		}

		switch m := msg.(type) {
		case *claudecode.SystemMessage:
			b.handleSystemMessage(m)
		case *claudecode.AssistantMessage:
			b.handleAssistantMessage(m)
		case *claudecode.ResultMessage:
			b.handleResult(m)
		case *claudecode.StreamEvent:
			b.handleStreamEvent(m)
		}
	}

	// Channel closed — the connection ended. The backend is no longer usable
	// regardless of the last turn's status: an Idle session whose transport
	// dropped (e.g. the CLI exited as fallout from an instant interrupt) is dead,
	// not idle. Marking it Dead lets the host rehydrate it on the next op instead
	// of dispatching a follow-up into a dead transport and silently wedging the
	// session in StatusError (the "needs attention" dead-end). Skip only an
	// intentional Stop() — the backend is being torn down, no status update needed.
	b.mu.Lock()
	stopped := b.stopped
	status := b.status
	b.mu.Unlock()
	if !stopped && status != StatusDead {
		b.setStatus(StatusDead)
	}
}

func (b *ClaudeCodeBackend) handleSystemMessage(m *claudecode.SystemMessage) {
	// The init message carries the session ID in SystemMessage.Data.
	// Once stored, every subsequent emit() stamps it onto Event.ExternalID
	// so the hub captures it the moment any event flows.
	if m.Subtype == "init" {
		if sid, ok := m.Data["session_id"].(string); ok && sid != "" {
			b.mu.Lock()
			b.sessionID = sid
			b.mu.Unlock()
		}
	}
}

// markModelActive flips an idle (or error) session back to Busy when model
// output arrives outside a host-initiated turn. Claude Code re-invokes itself
// when a background task (run_in_background Bash/Agent, Workflow) completes:
// the turn that spawned the task already ended (result → StatusIdle) and no
// Send precedes the follow-up turn — so without this flip the entire
// re-invoked turn streams while the session still reads idle (setStatus
// dedupes the terminating idle→idle, so clients get ZERO status frames:
// no spinner, no Stop, and Send stays enabled against a working agent).
//
// Safe by construction: model output is always terminated by a ResultMessage
// (handleResult restores idle/error), so the flip can't strand the session
// Busy. Only assistant-origin signals call this — user-role tool_result
// echoes must NOT, since a post-abort straggler would then flip Busy with no
// result to settle it. Starting is left alone (Open's handshake owns it) and
// Dead is unreachable here (receiveLoop has exited by then).
func (b *ClaudeCodeBackend) markModelActive() {
	b.mu.Lock()
	st := b.status
	b.mu.Unlock()
	if st == StatusIdle || st == StatusError {
		b.setStatus(StatusBusy)
	}
}

func (b *ClaudeCodeBackend) handleAssistantMessage(m *claudecode.AssistantMessage) {
	// Model output while the session reads idle = a self-initiated turn
	// (background-task re-invocation). Normally a no-op — Send already set
	// Busy — and when streaming is on, message_start beat us to it.
	b.markModelActive()
	// Emit a content-less shell — matching the OpenCode pattern.
	// The TUI ignores EventMessage content after history loads, and new
	// content arrives exclusively via EventPartUpdate from streaming deltas
	// (handleContentBlockStart/Delta). Emitting parts here would duplicate
	// what the streaming path already delivered.
	//
	// Full message content (including parts) is reconstructed on demand by
	// Messages() reading the on-disk JSONL transcript via the SDK.
	b.emit(Event{
		Type:      EventMessage,
		Timestamp: time.Now(),
		Data: MessageData{
			Role: "assistant",
		},
	})
}

func (b *ClaudeCodeBackend) handleResult(m *claudecode.ResultMessage) {
	// The result carries the authoritative CLI session UUID.
	if m.SessionID != "" {
		b.mu.Lock()
		b.sessionID = m.SessionID
		b.mu.Unlock()
	}

	// A user-initiated Abort interrupts the current turn; the CLI then ends it
	// with an error result (often a nil Result, i.e. the generic "unknown
	// error"). That is the expected fallout of the interrupt, not a session
	// failure — return to Idle so the session stays usable instead of wedging in
	// StatusError. Consume the flag so only the interrupt's own result is
	// reinterpreted; a genuine error on a later turn still surfaces.
	b.mu.Lock()
	aborted := b.aborting
	b.aborting = false
	b.mu.Unlock()
	if aborted {
		b.setStatus(StatusIdle)
		return
	}

	// The CLI writes its generated session title to the transcript, not the
	// stdout stream the SDK surfaces — publish it as EventTitleChange once it
	// lands so clients stop showing the first prompt as the title. Titling runs
	// concurrently with the turn, so a fast turn's result can beat the title to
	// the transcript; the recheck picks it up without waiting for another turn.
	if !b.maybeEmitAITitle() {
		b.scheduleAITitleRecheck()
	}

	if m.IsError {
		errMsg := "unknown error"
		if m.Result != nil {
			errMsg = *m.Result
		}
		b.emitError(errMsg)
		b.setStatus(StatusError)
	} else {
		b.setStatus(StatusIdle)
	}
}

// handleStreamEvent processes partial streaming updates (content_block_start,
// content_block_delta, content_block_stop) and tracks the current Anthropic
// message ID from message_start events.
func (b *ClaudeCodeBackend) handleStreamEvent(m *claudecode.StreamEvent) {
	eventType, _ := m.Event["type"].(string)

	switch eventType {
	case claudecode.StreamEventTypeMessageStart:
		// Earliest wire signal that the model is producing output. For a
		// turn the CLI started on its own (background task finished →
		// harness re-invocation) this is what flips idle → Busy; for a
		// host-initiated turn Send already set Busy and this is a no-op.
		b.markModelActive()
		// Extract the Anthropic API message ID (e.g. "msg_01XFD...") from the
		// nested message object. Each API call produces a unique message ID,
		// so this changes on every message cycle (including within a single turn
		// when tool use triggers additional API calls).
		if msgData, ok := m.Event["message"].(map[string]any); ok {
			if msgID, ok := msgData["id"].(string); ok {
				b.currentMsgID = msgID
			}
		}
		// Reset activeToolBlocks — block indices restart at 0 in each message cycle.
		b.activeToolBlocks = make(map[int]*activeToolBlock)
	case claudecode.StreamEventTypeContentBlockStart:
		b.handleContentBlockStart(m.Event)
	case claudecode.StreamEventTypeContentBlockDelta:
		b.handleContentBlockDelta(m.Event)
	case claudecode.StreamEventTypeContentBlockStop:
		b.handleContentBlockStop(m.Event)
	}
}

// blockID returns a part ID scoped to the current Anthropic message and block index.
// The message ID (from message_start) is naturally unique across message cycles
// and turns, so no synthetic counters are needed. This mirrors how OpenCode uses
// server-assigned part IDs — we use the API's own message ID as the scope.
func (b *ClaudeCodeBackend) blockID(index int) string {
	// currentMsgID is only read/written from receiveLoop (single goroutine),
	// so no lock is needed here.
	return fmt.Sprintf("%s-%d", b.currentMsgID, index)
}

// emitPart emits an EventPartUpdate stamped with the current Anthropic message
// id. Stamping MessageID lets clients route a part to its owning message
// directly, instead of reverse-engineering it from the part id (text/thinking
// ids embed {apiMsgID}-{blockIdx}, but tool parts use the raw tool_use id with
// no such prefix). currentMsgID == the assistant message's transcript id, so
// streamed parts and the reloaded transcript reconcile by the same key. This
// mirrors what the OpenCode backend already does. Called only from receiveLoop
// (single goroutine), so reading currentMsgID is lock-free.
func (b *ClaudeCodeBackend) emitPart(part Part, isDelta bool) {
	b.emit(Event{
		Type:      EventPartUpdate,
		Timestamp: time.Now(),
		Data: PartUpdateData{
			MessageID: b.currentMsgID,
			Part:      part,
			IsDelta:   isDelta,
		},
	})
}

func (b *ClaudeCodeBackend) handleContentBlockStart(event map[string]any) {
	block, ok := event["content_block"].(map[string]any)
	if !ok {
		return
	}

	index := intFromAny(event["index"])
	blockType, _ := block["type"].(string)

	switch blockType {
	case "text":
		text, _ := block["text"].(string)
		b.emitPart(Part{
			ID:   b.blockID(index),
			Type: PartText,
			Text: text,
		}, false)
	case "tool_use":
		id, _ := block["id"].(string)
		name, _ := block["name"].(string)
		// Track this tool_use block so handleContentBlockStop can emit PartCompleted
		// with the correct tool name.
		b.activeToolBlocks[index] = &activeToolBlock{partID: id, tool: name}
		// Remember the id so a permission prompt for this tool (which arrives on
		// the SDK read goroutine, later) can be attributed to it. Guarded
		// because handleCanUseTool reads it from a different goroutine.
		if id != "" && name != "" {
			b.mu.Lock()
			b.lastToolUseID[name] = id
			b.mu.Unlock()
		}
		b.emitPart(Part{
			ID:     id,
			Type:   PartToolCall,
			Tool:   name,
			Status: PartRunning,
		}, false)
	case "thinking":
		text, _ := block["thinking"].(string)
		b.emitPart(Part{
			ID:   b.blockID(index),
			Type: PartThinking,
			Text: text,
		}, false)
	}
}

func (b *ClaudeCodeBackend) handleContentBlockDelta(event map[string]any) {
	delta, ok := event["delta"].(map[string]any)
	if !ok {
		return
	}

	index := intFromAny(event["index"])
	deltaType, _ := delta["type"].(string)

	switch deltaType {
	case "text_delta":
		text, _ := delta["text"].(string)
		if text != "" {
			b.emitPart(Part{
				ID:   b.blockID(index),
				Type: PartText,
				Text: text,
			}, true)
		}
	case "thinking_delta":
		text, _ := delta["thinking"].(string)
		if text != "" {
			b.emitPart(Part{
				ID:   b.blockID(index),
				Type: PartThinking,
				Text: text,
			}, true)
		}
	case "input_json_delta":
		// Accumulate tool input JSON incrementally so it's available at
		// content_block_stop (and ultimately in the Part.Input field).
		partial, _ := delta["partial_json"].(string)
		if partial != "" {
			if tb, ok := b.activeToolBlocks[index]; ok {
				tb.inputBuf.WriteString(partial)
			}
		}
	}
}

// handleContentBlockStop transitions tool call parts to completed status.
// This fixes the "spinner stuck" bug where tool calls stayed in PartRunning
// indefinitely because the tool_result arrives as a separate message cycle.
func (b *ClaudeCodeBackend) handleContentBlockStop(event map[string]any) {
	index := intFromAny(event["index"])

	// Only tool_use blocks are tracked in activeToolBlocks. If the index is
	// present, emit a PartCompleted update so the TUI replaces the spinner with ✓.
	tb, ok := b.activeToolBlocks[index]
	if !ok {
		return
	}
	delete(b.activeToolBlocks, index)

	// Parse accumulated input JSON into a map for the Part.Input field.
	var inputMap map[string]any
	if raw := tb.inputBuf.String(); raw != "" {
		_ = json.Unmarshal([]byte(raw), &inputMap)
	}

	part := Part{
		ID:     tb.partID,
		Type:   PartToolCall,
		Tool:   tb.tool,
		Status: PartCompleted,
		Input:  inputMap,
	}
	// Tag an AskUserQuestion with its normalized prompt, addressed by the
	// deterministic bypass id. In bypassPermissions the tool auto-ran and this
	// tag is what clients render/answer from (via a follow-up message); in
	// gated modes the prompt was already answered through the parked
	// permission by the time this stop arrives, so the tag is only ever used
	// to render the historical card.
	if part.Question = tagQuestionPart(tb.partID, tb.tool, inputMap); part.Question != nil {
		b.mu.Lock()
		if _, exists := b.pendingQuestions[part.Question.RequestID]; !exists {
			b.pendingQuestions[part.Question.RequestID] = claudeQuestion{
				toolUseID: tb.partID,
				questions: part.Question.Questions,
			}
		}
		b.mu.Unlock()
	}
	b.emitPart(part, false)
}

// --- Type mapping helpers ---

// coalesceSessionMessages converts the SDK's on-disk JSONL records into clank
// MessageData — one per Anthropic message, with its content blocks grouped and
// ordered. This is the same shape the OpenCode backend produces and the shape
// the gateway and clients rely on (unique message ids, parts in order).
//
// Claude Code does NOT write one record per message: it splits an assistant
// turn into several JSONL records that each carry a single content block but
// all share the turn's message.id (a thinking record, a text record, then one
// record per tool_use). Mapping each record 1:1 — as this used to — emits
// several MessageData with the SAME id, and because each record's block index
// restarts at 0, the thinking and text parts both get id "{msgID}-0". Clients
// that group parts under a message keyed by id (the mobile app) then collapse
// those same-id messages last-wins and dedupe the colliding part ids, which
// surfaces as thinking bundled away from its tool calls, or dropped entirely.
//
// The cure: append consecutive records that share an assistant message id into
// one MessageData, concatenating their blocks under a running block index so
// text/thinking ids become "{msgID}-0", "{msgID}-1", … — matching what the
// streaming path emits via blockID(), so a streamed turn and its reloaded
// transcript reconcile by the same keys. A user record (its own UUID; tool
// results live here) always breaks the run, so only one assistant turn ever
// coalesces at a time.
//
// Part IDs are scoped to mirror the streaming handlers so live deltas and
// reloaded history match up:
//   - tool_use / tool_result blocks use the tool_use_id (same as
//     handleContentBlockStart / handleContentBlockStop).
//   - text / thinking blocks use "{apiMsgID}-{blockIdx}", apiMsgID being the
//     Anthropic message id under msg.message.id (matches blockID()), falling
//     back to the JSONL-level UUID when absent.
func coalesceSessionMessages(sdkMsgs []claudecode.SessionMessage) []MessageData {
	out := make([]MessageData, 0, len(sdkMsgs))
	// blockBase is the API block-index offset for the group currently at the
	// tail of out — i.e. how many blocks its earlier records already consumed.
	// It counts every block (even ones sessionBlockToPart skips) so indices stay
	// aligned with the API's own block numbering, like the streaming path.
	blockBase := 0
	for _, m := range sdkMsgs {
		if m.IsMeta {
			continue
		}

		var role string
		switch m.Type {
		case "user":
			role = "user"
		case "assistant":
			role = "assistant"
		default:
			continue
		}

		// Anthropic API message id lives inside the nested "message" object;
		// fall back to the transcript-level UUID when missing (user messages
		// have no API id, so each stays its own MessageData).
		msgID, _ := m.RawMessage["id"].(string)
		if msgID == "" {
			msgID = m.UUID
		}

		// Continue the previous group when this record extends the same
		// assistant API message; otherwise open a new MessageData.
		n := len(out)
		merge := n > 0 && role == "assistant" && msgID != "" &&
			out[n-1].Role == "assistant" && out[n-1].ID == msgID
		if !merge {
			md := MessageData{ID: msgID, Role: role}
			if model, ok := m.RawMessage["model"].(string); ok {
				md.ModelID = model
			}
			out = append(out, md)
			blockBase = 0
		}
		cur := &out[len(out)-1]

		if m.Content == nil {
			continue
		}
		switch m.Content.Kind {
		case claudecode.SessionContentTypeString:
			cur.Content = m.Content.String
		case claudecode.SessionContentTypeBlocks:
			for i, block := range m.Content.Blocks {
				if p, ok := sessionBlockToPart(block, msgID, blockBase+i); ok {
					cur.Parts = append(cur.Parts, p)
				}
			}
			blockBase += len(m.Content.Blocks)
		}
	}
	return out
}

// sessionBlockToPart converts an SDK session ContentBlock to a clank Part.
// The msgID/index pair scopes text/thinking IDs to match the streaming path.
func sessionBlockToPart(block claudecode.SessionContentBlock, msgID string, index int) (Part, bool) {
	switch block.Type {
	case claudecode.SessionBlockTypeText:
		return Part{
			ID:   fmt.Sprintf("%s-%d", msgID, index),
			Type: PartText,
			Text: block.Text,
		}, true
	case claudecode.SessionBlockTypeThinking:
		return Part{
			ID:   fmt.Sprintf("%s-%d", msgID, index),
			Type: PartThinking,
			Text: block.Thinking,
		}, true
	case claudecode.SessionBlockTypeToolUse, claudecode.SessionBlockTypeServerToolUse:
		return Part{
			ID:     block.ID,
			Type:   PartToolCall,
			Tool:   block.Name,
			Status: PartCompleted,
			Input:  block.Input,
			// Question prompts stay renderable/answerable from history alone.
			Question: tagQuestionPart(block.ID, block.Name, block.Input),
		}, true
	case claudecode.SessionBlockTypeToolResult:
		status := PartCompleted
		if block.IsError != nil && *block.IsError {
			status = PartFailed
		}
		return Part{
			ID:     block.ToolUseID,
			Type:   PartToolResult,
			Status: status,
			Output: toolResultOutput(block.Content),
		}, true
	default:
		return Part{}, false
	}
}

// toolResultOutput extracts the human-readable text from a tool_result's
// content field. The SDK leaves it as `any` because the JSONL format admits
// either a string or an array of nested blocks (typically text blocks).
func toolResultOutput(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t == claudecode.SessionBlockTypeText {
				if s, ok := m["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// intFromAny extracts an int from a JSON-decoded value (usually float64).
func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

// buildExtraEnv injects IS_SANDBOX=1 when running as root so claude accepts
// --dangerously-skip-permissions (bypassPermissions mode) in container hosts.
func buildExtraEnv(euid int, env map[string]string) map[string]string {
	if euid != 0 || env["IS_SANDBOX"] != "" {
		return env
	}
	merged := make(map[string]string, len(env)+1)
	for k, v := range env {
		merged[k] = v
	}
	merged["IS_SANDBOX"] = "1"
	return merged
}
