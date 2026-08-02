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

func TestPreviewCmdRecognizesWebURLAsPullRequestInput(t *testing.T) {
	t.Parallel()

	cmd := previewCmd()
	cmd.SetArgs([]string{"https://example.com/acme/api/pull/7"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "github.com") {
		t.Fatalf("Execute: err = %v, want canonical GitHub URL guidance", err)
	}
}

func TestPreviewCmdRejectsProjectFlagWithPullRequest(t *testing.T) {
	t.Parallel()

	cmd := previewCmd()
	cmd.SetArgs([]string{"--project", t.TempDir(), "https://github.com/acme/api/pull/7"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--project cannot be used") {
		t.Fatalf("Execute: err = %v, want --project conflict", err)
	}
}
