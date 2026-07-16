package agent

import "testing"

// The valid set is the closed family-alias list the claude CLI accepts
// via --model. Aliases pinned here so a refactor that drops one (fable
// in particular — it comes from a clank-local constant, not the SDK)
// fails loudly.
func TestIsValidClaudeModel(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"sonnet", "opus", "haiku", "fable"} {
		if !IsValidClaudeModel(id) {
			t.Errorf("IsValidClaudeModel(%q) = false, want true", id)
		}
	}
	// `inherit` is a passthrough, not a user pick; full model IDs and
	// the empty string are rejected so callers can use this as a
	// "should I pass --model" check.
	for _, id := range []string{"", "inherit", "claude-fable-5", "claude-opus-4.7"} {
		if IsValidClaudeModel(id) {
			t.Errorf("IsValidClaudeModel(%q) = true, want false", id)
		}
	}
}
