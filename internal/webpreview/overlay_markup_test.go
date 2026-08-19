package webpreview

import (
	"strings"
	"testing"
)

func TestOverlayLauncherRemainsDiscoverableWhenPromptIsHidden(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	for _, want := range []string{
		`<button class="launcher"`,
		`<div class="coachmark"`,
		"ui.launcher.classList.toggle('visible', store.box === 'hidden')",
		"acknowledgeLauncher",
		"launcherActivity(store.agent, store.aborting)",
		"animateLauncherIntoBox",
		"ui.box.animate(",
		".box.morphing",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("overlay.js launcher wiring missing %q", want)
		}
	}
	if strings.Contains(js, ".box.hidden") {
		t.Error("the prompt box should minimize into the launcher through state, not a second hidden box mode")
	}
}

func TestOverlayWorkingStateHasIndependentProgressIndicator(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	for _, want := range []string{
		`<span class="activity-spinner"`,
		`<div class="agent-progress"`,
		"ui.progress.classList.toggle('visible', busy)",
		"ui.send.innerHTML = busy ? ICONS.stop : ICONS.send",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("overlay.js explicit progress wiring missing %q", want)
		}
	}
}

func TestOverlayChatExpansionIsDiscoverableAndKeyboardAccessible(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	for _, want := range []string{
		`<button class="chat-toggle"`,
		`aria-label="Show conversation"`,
		`Cmd+Shift+E`,
		`e.code === 'KeyE' && (e.metaKey || e.ctrlKey) && e.shiftKey`,
		`e.target.closest('.beta, .scchip, .chat-toggle')`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("overlay.js chat expansion affordance missing %q", want)
		}
	}
}

func TestOverlayCompletedLauncherHasGreenBorder(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	if !strings.Contains(js, `.launcher[data-state="done"] { border-color:#22c55e;`) {
		t.Error("overlay.js completed launcher must carry an explicit green border")
	}
}

func TestOverlayStructuredTranscriptWiring(t *testing.T) {
	t.Parallel()
	js, tx := string(overlayJS), string(transcriptJS)
	for _, want := range []string{"upsertTranscriptPart", "createTranscriptRenderer"} {
		if !strings.Contains(js, want) {
			t.Errorf("overlay.js structured transcript wiring missing %q", want)
		}
	}
	for _, want := range []string{"renderMarkdown", "renderToolCall", "renderThinking"} {
		if !strings.Contains(tx, want) {
			t.Errorf("transcript.js missing %q", want)
		}
	}
	if strings.Contains(js, "streamText") {
		t.Error("overlay.js must project live parts by id instead of maintaining a separate text-only stream")
	}
}

func TestOverlayLauncherUsesCurrentClankMark(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	if got := strings.Count(js, `class="launcher-mark-corner"`); got != 4 {
		t.Errorf("launcher corner blocks = %d, want 4", got)
	}
	if got := strings.Count(js, `class="launcher-mark-dash"`); got != 8 {
		t.Errorf("launcher edge dashes = %d, want 8 (two per edge)", got)
	}
	if !strings.Contains(js, `class="launcher-mark-field"`) {
		t.Error("overlay.js current Clank mark missing its translucent field")
	}
	for _, want := range []string{
		`<svg width="27" height="27" viewBox="2.5 2.5 19 19"`,
		`class="launcher-mark-corner" x="2.5" y="2.5" width="4.5" height="4.5"`,
		`class="launcher-mark-dash" x="8" y="4.25" width="3.5" height="1"`,
		`class="launcher-mark-dash" x="4.25" y="8" width="1" height="3.5"`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("overlay.js current Clank proportions missing %q", want)
		}
	}
	if strings.Contains(js, `stroke-dasharray=`) {
		t.Error("continuous SVG dash phase misaligns the launcher's visible edge segments")
	}
}

// TestOverlayBusyDerivesFromActivityNotRawAgent guards against a regression
// where busy was computed from store.agent alone: the Stop control and
// progress row would then flip back to idle mid-abort, before abort()'s
// request resolves, because launcherActivity's 'stopping' state (aborting
// === true, agent already 'done') was never consulted.
func TestOverlayBusyDerivesFromActivityNotRawAgent(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	if !strings.Contains(js, "const busy = activity.isBusy;") {
		t.Error("overlay.js render() must derive busy from activity.isBusy, not store.agent alone")
	}
	if strings.Contains(js, "const busy = store.agent === 'thinking' || store.agent === 'working';") {
		t.Error("overlay.js must not reintroduce the raw store.agent busy check that drops aborting")
	}
}

// TestOverlayLauncherMorphDoesNotReplayEntryAnimation guards the morph→box
// transition: removing .morphing without a follow-up suppressor lets
// .box.visible's boxIn animation restart from opacity:0, so the box
// flashes invisible right after the 420ms morph settles.
func TestOverlayLauncherMorphDoesNotReplayEntryAnimation(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	for _, want := range []string{
		".box.morphing, .box.morphed",
		"classList.replace('morphing', 'morphed')",
		"classList.remove('morphed')",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("overlay.js morph-settle wiring missing %q", want)
		}
	}
}

// TestOverlayLauncherRegainsFocusOnHide guards keyboard access: once the
// persistent launcher shipped, hiding the box (Escape, ⌘E) left focus on a
// now-invisible element instead of the launcher that replaces it.
func TestOverlayLauncherRegainsFocusOnHide(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	if !strings.Contains(js, "setTimeout(() => ui.launcher.focus({ preventScroll: true }), 0);") {
		t.Error("overlay.js setBox('hidden') must restore focus to ui.launcher")
	}
}

// TestOverlayLauncherAcknowledgementRetriesOnFailure guards the client-side
// mirror of the server-side persist-then-flip fix: clearing
// store.launcherCoachmark before the POST resolves would drop the retry on
// a failed save just like the Go handler did.
func TestOverlayLauncherAcknowledgementRetriesOnFailure(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	if !strings.Contains(js, "store.launcherCoachmark = false; // only clear once persisted") {
		t.Error("overlay.js acknowledgeLauncher must clear launcherCoachmark only after the POST succeeds")
	}
}

// TestOverlayComposerSelectorIsClassScoped guards the composer wiring:
// ui.input was once selected with a bare $('textarea'), and adding the
// plan-notes textarea earlier in the markup silently captured it —
// Enter-to-send and the send button both read the wrong (hidden, empty)
// element. The composer must carry its own class and be selected by it.
func TestOverlayComposerSelectorIsClassScoped(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	if strings.Contains(js, "$('textarea')") {
		t.Error("overlay.js selects a bare $('textarea'); scope it to the composer class — the first textarea in the markup is not the composer")
	}
	if !strings.Contains(js, `<textarea class="compose"`) {
		t.Error(`overlay.js composer textarea must carry class="compose"`)
	}
	if !strings.Contains(js, "$('.compose')") {
		t.Error("overlay.js must select the composer via $('.compose')")
	}
}

// TestOverlayInlineCommentWiring pins the inline-comment feature's
// contact points: the popover exists in the markup, and send() decides
// sendability via composerTextForSend — a raw ui.input.value guard
// would silently break comment-only submits (empty composer, comment
// chips attached).
func TestOverlayInlineCommentWiring(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	// A textarea, not an input: the comment must wrap and grow as you
	// type past the popover's width.
	if !strings.Contains(js, `<div class="cpop">`) || !strings.Contains(js, `<textarea class="cpop-in"`) {
		t.Error("overlay.js must carry the comment popover markup (.cpop with a .cpop-in textarea)")
	}
	if !strings.Contains(js, "$('.cpop')") || !strings.Contains(js, "$('.cpop-in')") {
		t.Error("overlay.js must select the comment popover via $('.cpop') / $('.cpop-in')")
	}
	if !strings.Contains(js, "composerTextForSend(ui.input.value, store.chips)") {
		t.Error("send paths must gate on composerTextForSend(ui.input.value, store.chips) so comment-only submits work")
	}
	// Focusing the popover input deactivates the browser's own selection
	// highlight; without the pending mark the selection visibly vanishes
	// the moment the popover opens.
	if !strings.Contains(js, "'clank-pending'") || !strings.Contains(js, "'clank-comment'") {
		t.Error("overlay.js must register both the clank-comment and clank-pending highlights")
	}
	// Chips are the edit surface (clickable text can't work reliably —
	// highlight marks receive no events and ranges die on live reload).
	if !strings.Contains(js, "editChipComment(c, el.getBoundingClientRect())") {
		t.Error("overlay.js chips must open the prefilled comment editor on click")
	}
	// One chip per ⌘-selected element: a re-click deselects (toggle),
	// never duplicates.
	if !strings.Contains(js, "store.chips.findIndex((c) => c.node === hoverEl)") {
		t.Error("overlay.js inspector clicks must dedupe by anchored element node")
	}
	// The pending mark adopts the page's own ::selection color (with the
	// blue fallback), and ⌘C in the focused popover copies the anchor
	// text — both were user-reported papercuts.
	if !strings.Contains(js, "var(--clank-pending-bg") || !strings.Contains(js, "'::selection'") {
		t.Error("overlay.js pending mark must adopt the page's ::selection color via --clank-pending-bg")
	}
	if !strings.Contains(js, "navigator.clipboard.writeText") {
		t.Error("overlay.js popover must bridge ⌘C to copy the anchor text")
	}
	// Scrolling must reposition the popover along its anchor, not dismiss
	// it — highlight + input surviving a scroll is the point.
	if strings.Contains(js, "window.addEventListener('scroll', () => hideCommentPopover()") {
		t.Error("overlay.js must not dismiss the comment popover on scroll")
	}
	if !strings.Contains(js, "positionCommentPopover(r)") {
		t.Error("overlay.js must reposition the comment popover on scroll")
	}
}

// TestOverlaySelectModeWiring pins the select-mode affordances: elements
// already in context outline their bounding boxes while ⌘ is held
// (repositioned on scroll), and chips cue their page anchor on hover
// and while their comment editor is open.
func TestOverlaySelectModeWiring(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	for _, want := range []string{
		"syncAttachedBoxes",
		"repositionAttachedBoxes",
		`<div class="hla chiphl"`,
		"hoverChipCue(c)",
		"showChipBox(c.node)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("overlay.js select-mode wiring missing %q", want)
		}
	}
}

// TestOverlayChipCueClearedBeforeRerender guards against a stuck hover
// cue: removing a hovered chip node never fires mouseleave, so render()
// rebuilding ui.chips without first clearing the cue leaves a page
// outline (or pending mark) pointing at a chip that no longer exists.
func TestOverlayChipCueClearedBeforeRerender(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	if !strings.Contains(js, "clearChipCue();\n    ui.chips.innerHTML = '';") {
		t.Error("overlay.js render() must call clearChipCue() before wiping ui.chips, or a hovered chip's cue outlives its DOM node")
	}
}

// TestOverlaySourceControlWiring pins the shipping surface: the header
// chip sits immediately left of the beta pill, opens the in-box panel,
// and the flows go through sourcecontrol.js request builders (both
// hosted {id} routes and local GitRef routes) with structured error
// codes — no error-string parsing.
func TestOverlaySourceControlWiring(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	for _, want := range []string{
		`<button class="scchip`,
		`<div class="sc" style="display:none"></div>`,
		"$('.scchip')",
		"$('.sc')",
		"scRequest(op, scGitRef(), extra)",
		"refreshSourceControl",
		"renderSourceControl",
		"branch_already_has_pr",
		"scStartConnect",
		"mergeInProgressPrompt",
		"prConflictsPrompt",
		"actionLayout(actionsFor(st))",            // ≤2 verbs side by side, 3+ collapse to ⋯
		`node('div', 'sc-menu')`,                  // the overflow menu grows vertically
		`closest('.beta, .scchip, .chat-toggle')`, // header controls must not arm a drag
	} {
		if !strings.Contains(js, want) {
			t.Errorf("overlay.js source-control wiring missing %q", want)
		}
	}
	if !strings.Contains(js, `${ICONS.branch}<span class="sctext"></span></button><a class="beta"`) {
		t.Error("overlay.js source-control chip must sit immediately left of the beta pill")
	}
	if !strings.Contains(js, "from './sourcecontrol.js'") {
		t.Error("overlay.js must import the pure sourcecontrol.js module (node-tested logic stays out of the DOM file)")
	}
}

// TestOverlayAgentSettingsWiring pins the mobile-parity settings surface:
// the footer chip opens a profile/knob editor, and both create and follow-up
// sends carry the staged config rather than silently reverting to Build.
func TestOverlayAgentSettingsWiring(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	for _, want := range []string{
		`<div class="settings"`,
		`<div class="save-profile"`,
		`class="profile"`,
		"$('.settings')",
		"$('.profile')",
		"loadConfigOptions",
		"saveProfile",
		"config: createConfig",
		"config: pendingConfig",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("overlay.js agent settings wiring missing %q", want)
		}
	}
	if !strings.Contains(js, `<span class="sp"></span>
    <button class="profile"`) || !strings.Contains(js, `</button>
    <button class="ib mic"`) {
		t.Error("overlay.js profile selector must sit in the right action cluster immediately left of the microphone")
	}
}

// TestOverlaySendClickDerivesFromActivityNotRawAgent guards against a
// regression where the Stop/Send click handler read store.agent alone: once
// aborting flips isBusy true while agent has already moved on from
// thinking/working (INV: server can settle agent before the abort request
// resolves), the button still shows Stop but a click fell through to send(),
// firing a new prompt mid-abort instead of leaving the in-flight abort alone.
func TestOverlaySendClickDerivesFromActivityNotRawAgent(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	if !strings.Contains(js, "ui.send.onclick = () => { launcherActivity(store.agent, store.aborting).isBusy ? abort() : send(); };") {
		t.Error("overlay.js send/stop click handler must derive its abort-vs-send branch from launcherActivity(...).isBusy, not a raw store.agent check")
	}
	if strings.Contains(js, "(store.agent === 'thinking' || store.agent === 'working') ? abort() : send();") {
		t.Error("overlay.js must not reintroduce the raw store.agent check on the send/stop click handler")
	}
}

// TestOverlayAcknowledgeLauncherRendersAfterPersist guards against a
// regression where store.launcherCoachmark was cleared inside the
// acknowledgement fetch's .then() but nothing re-rendered afterward: the
// synchronous render() a caller (setBox, coachDismiss.onclick) ran before
// the fetch settled still shows the coachmark, and it stays visually stuck
// until an unrelated store mutation happens to trigger the next render().
func TestOverlayAcknowledgeLauncherRendersAfterPersist(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	ackStart := strings.Index(js, "const acknowledgeLauncher = ()")
	if ackStart < 0 {
		t.Fatal("overlay.js acknowledgeLauncher definition not found")
	}
	ackEnd := strings.Index(js[ackStart:], "};")
	if ackEnd < 0 {
		t.Fatal("overlay.js acknowledgeLauncher definition not terminated")
	}
	ack := js[ackStart : ackStart+ackEnd]
	clearIdx := strings.Index(ack, "store.launcherCoachmark = false;")
	renderIdx := strings.Index(ack, "render();")
	if clearIdx < 0 {
		t.Fatal("overlay.js acknowledgeLauncher must clear store.launcherCoachmark once persisted")
	}
	if renderIdx < 0 || renderIdx < clearIdx {
		t.Error("overlay.js acknowledgeLauncher must call render() after clearing store.launcherCoachmark, so the dismissed coachmark disappears as soon as the save succeeds")
	}
}
