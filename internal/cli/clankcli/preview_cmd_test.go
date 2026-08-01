package clankcli

import (
	"strings"
	"testing"
)

func TestPreviewCmdDoesNotExposePromptFlag(t *testing.T) {
	t.Parallel()

	cmd := previewCmd()
	cmd.SetArgs([]string{"--prompt", "change-the-header"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --prompt") {
		t.Fatalf("Execute: err = %v, want unknown prompt flag", err)
	}
}

func TestPreviewCmdRejectsProjectFlagWithAttachFolder(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	cmd := previewCmd()
	cmd.SetArgs([]string{"--project", projectDir, projectDir, ":5173"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "both positionally and with --project") {
		t.Fatalf("Execute: err = %v, want duplicate-folder guidance", err)
	}
}
