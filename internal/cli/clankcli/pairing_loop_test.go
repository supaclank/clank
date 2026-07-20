package clankcli

import "testing"

// TestPendingPromptResetsWhenQueueEmpties pins a bug flagged in review:
// once shown, the prompt never reappeared for a later phone if the
// first attempt disappeared (expired or cancelled) before the user
// typed anything, because `prompted` only cleared on user input.
func TestPendingPromptResetsWhenQueueEmpties(t *testing.T) {
	t.Parallel()

	show, prompted := pendingPrompt(false, 1)
	if !show || !prompted {
		t.Fatalf("first arrival: show=%v prompted=%v, want true,true", show, prompted)
	}

	show, prompted = pendingPrompt(true, 1)
	if show || !prompted {
		t.Fatalf("still pending, already shown: show=%v prompted=%v, want false,true", show, prompted)
	}

	show, prompted = pendingPrompt(true, 0)
	if show || prompted {
		t.Fatalf("attempt disappeared: show=%v prompted=%v, want false,false", show, prompted)
	}

	show, prompted = pendingPrompt(prompted, 1)
	if !show || !prompted {
		t.Fatalf("new arrival after reset: show=%v prompted=%v, want true,true", show, prompted)
	}
}
