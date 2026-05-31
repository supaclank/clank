package clankcli

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestHumanBytes(t *testing.T) {
	t.Parallel()
	cases := map[int64]string{
		512:                    "512 B",
		1024:                   "1.0 KB",
		1536:                   "1.5 KB",
		58 * 1024 * 1024:       "58.0 MB",
		3 * 1024 * 1024 * 1024: "3.0 GB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestRemoteLabel(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"http://localhost:7878":         "localhost:7878",
		"https://gateway.example.com":   "gateway.example.com",
		"https://gw.example.com:8443/x": "gw.example.com:8443",
	}
	for raw, want := range cases {
		if got := remoteLabel(raw); got != want {
			t.Errorf("remoteLabel(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestPushProgressModel_Transitions pins the model's reaction to the
// forwarded push events: phase/size/bytes update state, and pushDoneMsg
// finishes (quits) and surfaces the result.
func TestPushProgressModel_Transitions(t *testing.T) {
	t.Parallel()
	var m tea.Model = newPushProgressModel("gw.example.com")

	step := func(msg tea.Msg) (pushProgressModel, tea.Cmd) {
		next, cmd := m.Update(msg)
		m = next
		return next.(pushProgressModel), cmd
	}

	if pm, _ := step(pushPhaseMsg("Uploading")); pm.phase != "Uploading" {
		t.Fatalf("phase = %q, want Uploading", pm.phase)
	}
	if pm, _ := step(pushSizedMsg(100)); pm.total != 100 {
		t.Fatalf("total = %d, want 100", pm.total)
	}
	if pm, _ := step(pushBytesMsg(40)); pm.uploaded != 40 {
		t.Fatalf("uploaded = %d, want 40", pm.uploaded)
	}
	// Mid-flight view shows the remote and a progress line.
	if pm := m.(pushProgressModel); pm.View().Content == "" {
		t.Fatal("mid-flight view should not be empty")
	}

	pm, cmd := step(pushDoneMsg{res: nil, err: nil})
	if !pm.finished {
		t.Fatal("pushDoneMsg should finish the model")
	}
	if cmd == nil {
		t.Fatal("pushDoneMsg should return a quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("done command should be tea.Quit, got %T", cmd())
	}
	// Finished view clears so the caller's result line stands alone.
	if pm.View().Content != "" {
		t.Errorf("finished view should be empty, got %q", pm.View().Content)
	}
}
