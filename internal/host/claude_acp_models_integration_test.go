package host_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent/acptools"
	"github.com/supaclank/clank/internal/host"
)

// Probe the real bundled CLI's model menu without sending a prompt.
func TestIntegration_ClaudeACP_OffersOpus5(t *testing.T) {
	if os.Getenv("CLANK_TEST_CLAUDE_ACP") == "" {
		t.Skip("set CLANK_TEST_CLAUDE_ACP=1 to verify the pinned Claude model catalog")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CLAUDECODE", "")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	dirs := acpDirs(t)
	if _, err := acptools.Ensure(ctx, dirs.Tools); err != nil {
		t.Fatalf("provision Claude adapter: %v", err)
	}
	mgr, err := host.NewClaudeACPManager(dirs)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown()
	if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	opts, err := mgr.ConfigOptions(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("probe Claude model options: %v", err)
	}
	hasOpus5 := false
	for _, opt := range opts {
		if opt.ID != "model" {
			continue
		}
		for _, model := range opt.Values {
			t.Logf("Claude model: %s (%s): %s", model.Value, model.Name, model.Description)
			if strings.Contains(strings.ToLower(model.Value), "opus") && strings.Contains(strings.ToLower(model.Name+" "+model.Description), "opus 5") {
				hasOpus5 = true
			}
		}
	}
	if !hasOpus5 {
		t.Fatalf("pinned Claude runtime does not advertise Opus 5: %+v", opts)
	}
}
