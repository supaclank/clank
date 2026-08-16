package clankcli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
)

// TestResolveAttachSessionByID_RediscoversUnknownExternalID pins the
// --attach=<external-id> escape hatch: a session that exists only in the
// backend's archive (not yet registered with clank) is found by running
// one discovery sweep and matching the freshly-minted registration.
func TestResolveAttachSessionByID_RediscoversUnknownExternalID(t *testing.T) {
	t.Parallel()

	client, stub := newTestHost(t)
	projectDir := t.TempDir()
	stub.SetDiscoverSnapshots([]agent.SessionSnapshot{{
		ID:        "ses_ext123",
		Backend:   agent.BackendOpenCode,
		Title:     "fix the header",
		Directory: projectDir,
		CreatedAt: time.Now().Add(-2 * time.Hour),
		UpdatedAt: time.Now().Add(-1 * time.Hour),
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := resolveAttachSessionByID(ctx, client, "ses_ext123", projectDir)
	if err != nil {
		t.Fatalf("resolveAttachSessionByID: %v", err)
	}
	if session.ExternalID != "ses_ext123" {
		t.Errorf("ExternalID = %q, want ses_ext123", session.ExternalID)
	}
	if session.ID == "" {
		t.Error("discovered session was registered without a clank id")
	}
	if session.Backend != agent.BackendOpenCode {
		t.Errorf("Backend = %q, want %q", session.Backend, agent.BackendOpenCode)
	}

	// The clank id minted by discovery resolves directly on a second call
	// — no further discovery needed (the archive could be gone by then).
	stub.SetDiscoverSnapshots(nil)
	again, err := resolveAttachSessionByID(ctx, client, session.ID, projectDir)
	if err != nil {
		t.Fatalf("resolveAttachSessionByID(clank id): %v", err)
	}
	if again.ID != session.ID {
		t.Errorf("clank-id lookup returned %q, want %q", again.ID, session.ID)
	}
}

// TestResolveAttachSessionByID_UnknownIDFailsWithGuidance pins the fast
// failure: an id neither registered nor discoverable errors with a
// pointer at the interactive picker instead of silently starting fresh.
func TestResolveAttachSessionByID_UnknownIDFailsWithGuidance(t *testing.T) {
	t.Parallel()

	client, _ := newTestHost(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := resolveAttachSessionByID(ctx, client, "ses_missing", t.TempDir())
	if err == nil {
		t.Fatal("resolveAttachSessionByID: want error for unknown id")
	}
	if !strings.Contains(err.Error(), "ses_missing") || !strings.Contains(err.Error(), "--attach") {
		t.Errorf("error %q lacks the id and the picker hint", err)
	}
}

// TestResolveAttachSession_EmptyFlagIsNoop: no --attach, no daemon
// traffic, no session.
func TestResolveAttachSession_EmptyFlagIsNoop(t *testing.T) {
	t.Parallel()

	session, err := resolveAttachSession(context.Background(), nil, "", t.TempDir(), strings.NewReader(""), &strings.Builder{})
	if err != nil {
		t.Fatalf("resolveAttachSession: %v", err)
	}
	if session != nil {
		t.Errorf("session = %+v, want nil", session)
	}
}

// TestResolveAttachSession_PickerNeedsTTY: bare --attach on a piped
// terminal must fail with actionable guidance instead of hanging a UI
// nobody can drive.
func TestResolveAttachSession_PickerNeedsTTY(t *testing.T) {
	t.Parallel()

	_, err := resolveAttachSession(context.Background(), nil, previewAttachSelect, t.TempDir(), strings.NewReader(""), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "--attach=<session-id>") {
		t.Fatalf("err = %v, want the non-interactive guidance", err)
	}
}

func TestAttachedSessionBackend(t *testing.T) {
	t.Parallel()

	session := &agent.SessionInfo{Backend: agent.BackendClaudeCode}

	if bt, err := attachedSessionBackend(session, ""); err != nil || bt != agent.BackendClaudeCode {
		t.Errorf("unset flag: got (%q, %v), want the session's backend", bt, err)
	}
	if bt, err := attachedSessionBackend(session, "claude"); err != nil || bt != agent.BackendClaudeCode {
		t.Errorf("matching flag: got (%q, %v), want the session's backend", bt, err)
	}
	if _, err := attachedSessionBackend(session, "opencode"); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Errorf("conflicting flag: err = %v, want conflict", err)
	}
	if _, err := attachedSessionBackend(session, "not-a-backend"); err == nil {
		t.Error("invalid flag value: want parse error")
	}
}

func TestSessionMatchingID(t *testing.T) {
	t.Parallel()

	sessions := []agent.SessionInfo{
		{ID: "01ABC", ExternalID: "ses_one"},
		{ID: "01DEF"}, // no external id — an empty query must not match it
	}
	if s := sessionMatchingID(sessions, "01ABC"); s == nil || s.ID != "01ABC" {
		t.Errorf("clank id lookup failed: %+v", s)
	}
	if s := sessionMatchingID(sessions, "ses_one"); s == nil || s.ID != "01ABC" {
		t.Errorf("external id lookup failed: %+v", s)
	}
	if s := sessionMatchingID(sessions, ""); s != nil {
		t.Errorf("empty id matched %+v", s)
	}
	if s := sessionMatchingID(sessions, "nope"); s != nil {
		t.Errorf("unknown id matched %+v", s)
	}
}

func TestLooksLikeSessionID(t *testing.T) {
	t.Parallel()

	for _, id := range []string{
		"ses_8gK2mQ",
		"01J8ZJ2M3N4P5Q6R7S8T9V0W1X",           // ULID
		"6ba7b810-9dad-11d1-80b4-00c04fd430c8", // UUID
	} {
		if !looksLikeSessionID(id) {
			t.Errorf("looksLikeSessionID(%q) = false, want true", id)
		}
	}
	for _, arg := range []string{"web-app", "default", "storybook", "my_launch"} {
		if looksLikeSessionID(arg) {
			t.Errorf("looksLikeSessionID(%q) = true, want false (launch names must stay launch names)", arg)
		}
	}
}
