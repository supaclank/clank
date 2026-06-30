package agent

import "testing"

// White-box: buildPromptParams is unexported, and the first-prompt-only CAS is
// the contract we care about — attach guidance to the first prompt of a fresh
// session, then never again (later turns rely on conversation history).
func TestOpenCodeBuildPromptParams_SystemPromptFirstOnly(t *testing.T) {
	t.Parallel()
	b := &OpenCodeBackend{SystemPrompt: "EXPO GUIDANCE", sessionID: "sess-1"}

	first := b.buildPromptParams(SendMessageOpts{Text: "hi"}, nil)
	if first.System == nil {
		t.Fatal("first prompt: System is nil, want guidance attached")
	}
	if *first.System != "EXPO GUIDANCE" {
		t.Errorf("first prompt: System = %q, want EXPO GUIDANCE", *first.System)
	}

	second := b.buildPromptParams(SendMessageOpts{Text: "again"}, nil)
	if second.System != nil {
		t.Errorf("second prompt: System = %q, want nil (already in history)", *second.System)
	}
}

// A resumed session is constructed with an empty SystemPrompt (the host only
// assembles guidance for fresh sessions), so nothing is attached.
func TestOpenCodeBuildPromptParams_NoSystemPromptWhenEmpty(t *testing.T) {
	t.Parallel()
	b := &OpenCodeBackend{sessionID: "sess-1"}

	p := b.buildPromptParams(SendMessageOpts{Text: "hi"}, nil)
	if p.System != nil {
		t.Errorf("System = %q, want nil when SystemPrompt empty", *p.System)
	}
}
