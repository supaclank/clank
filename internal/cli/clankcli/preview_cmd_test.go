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

func TestPreviewCmdRejectsAttachWithPullRequest(t *testing.T) {
	t.Parallel()

	cmd := previewCmd()
	cmd.SetArgs([]string{"--attach=ses_abc", "https://github.com/acme/api/pull/7"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--attach cannot be used") {
		t.Fatalf("Execute: err = %v, want --attach conflict", err)
	}
}

// `--attach <id>` (space, not =) reaches RunE as a bare --attach plus a
// positional, because pflag binds optional-value flags only with `=`.
// The id-shaped positional must become the session, and everything else
// must stay a launch name.
func TestRouteAttachSessionArg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		attachFlag string
		args       []string
		wantFlag   string
		wantArgs   []string
	}{
		{
			name:       "spaced session id becomes the attach value",
			attachFlag: previewAttachSelect,
			args:       []string{"ses_8gK2mQ"},
			wantFlag:   "ses_8gK2mQ",
		},
		{
			name:       "launch name stays a launch name and keeps the picker",
			attachFlag: previewAttachSelect,
			args:       []string{"web-app"},
			wantFlag:   previewAttachSelect,
			wantArgs:   []string{"web-app"},
		},
		{
			name:       "no --attach leaves an id-shaped launch name alone",
			attachFlag: "",
			args:       []string{"ses_8gK2mQ"},
			wantFlag:   "",
			wantArgs:   []string{"ses_8gK2mQ"},
		},
		{
			name:       "explicit --attach=<id> keeps its launch name",
			attachFlag: "ses_explicit",
			args:       []string{"web-app"},
			wantFlag:   "ses_explicit",
			wantArgs:   []string{"web-app"},
		},
		{
			name:       "server-attach form is untouched",
			attachFlag: previewAttachSelect,
			args:       []string{".", ":5173"},
			wantFlag:   previewAttachSelect,
			wantArgs:   []string{".", ":5173"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotFlag, gotArgs := routeAttachSessionArg(tt.attachFlag, tt.args)
			if gotFlag != tt.wantFlag {
				t.Errorf("attach flag = %q, want %q", gotFlag, tt.wantFlag)
			}
			if strings.Join(gotArgs, " ") != strings.Join(tt.wantArgs, " ") {
				t.Errorf("args = %v, want %v", gotArgs, tt.wantArgs)
			}
		})
	}
}
