package clankcli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/host"
)

func TestParseGitHubPullRequestURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		rawURL  string
		want    daemonclient.GitHubPullRequestLocator
		wantErr string
	}{
		{
			name:   "canonical URL",
			rawURL: "https://github.com/Acksell/supaclank/pull/66",
			want:   daemonclient.GitHubPullRequestLocator{Owner: "Acksell", Repo: "supaclank", Number: 66},
		},
		{
			name:   "trailing slash",
			rawURL: "https://github.com/Acksell/supaclank/pull/66/",
			want:   daemonclient.GitHubPullRequestLocator{Owner: "Acksell", Repo: "supaclank", Number: 66},
		},
		{name: "wrong host", rawURL: "https://example.com/Acksell/supaclank/pull/66", wantErr: "github.com"},
		{name: "userinfo rejected", rawURL: "https://someone@github.com/Acksell/supaclank/pull/66", wantErr: "user information"},
		{name: "query rejected", rawURL: "https://github.com/Acksell/supaclank/pull/66?diff=split", wantErr: "query"},
		{name: "fragment rejected", rawURL: "https://github.com/Acksell/supaclank/pull/66#discussion", wantErr: "fragment"},
		{name: "files suffix rejected", rawURL: "https://github.com/Acksell/supaclank/pull/66/files", wantErr: "pull request URL"},
		{name: "zero number", rawURL: "https://github.com/Acksell/supaclank/pull/0", wantErr: "number"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseGitHubPullRequestURL(tc.rawURL)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGitHubPullRequestURL: %v", err)
			}
			if got != tc.want {
				t.Errorf("locator = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestStartHostedPreviewUsesManagedWorktree(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/worktrees/01WT/preview/start" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"web","service_name":"default","state":"starting"}`))
	}))
	t.Cleanup(srv.Close)
	client := daemonclient.NewTCPClient(srv.URL, "")

	status, sessionID, err := startHostedPreview(
		context.Background(), client, client.Preview("01WT"), agent.BackendOpenCode, "01WT", strings.NewReader(""), io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	if sessionID != "" || status.Kind != "web" || status.ServiceName != "default" {
		t.Errorf("status = %+v, sessionID = %q", status, sessionID)
	}
}

func TestNewHostedPreviewSetupRequestTargetsManagedWorktree(t *testing.T) {
	t.Parallel()
	req := newHostedPreviewSetupRequest(agent.BackendOpenCode, "01JV7T7F9Y6XQ1R6M8R2W4K3NZ", "configure preview")
	if req.Hostname != host.HostLocal {
		t.Errorf("Hostname = %q", req.Hostname)
	}
	if req.GitRef.WorktreeID != "01JV7T7F9Y6XQ1R6M8R2W4K3NZ" || req.GitRef.LocalPath != "" {
		t.Errorf("GitRef = %+v", req.GitRef)
	}
	if req.Prompt != "configure preview" {
		t.Errorf("Prompt = %q", req.Prompt)
	}
	if err := req.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestConfirmGitHubPullRequestTrust(t *testing.T) {
	t.Parallel()
	inspection := daemonclient.GitHubPullRequestInspection{
		GitHubPullRequestLocator: daemonclient.GitHubPullRequestLocator{Owner: "acme", Repo: "api", Number: 7},
		Title:                    "Run untrusted code",
		Author:                   "octocat",
		HeadSHA:                  "0123456789abcdef0123456789abcdef01234567",
	}

	for _, tc := range []struct {
		name      string
		input     string
		wantTrust bool
	}{
		{name: "yes", input: "yes\n", wantTrust: true},
		{name: "short yes", input: "Y\n", wantTrust: true},
		{name: "default no", input: "\n", wantTrust: false},
		{name: "explicit no", input: "no\n", wantTrust: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			trusted, err := confirmGitHubPullRequestTrust(strings.NewReader(tc.input), &out, inspection)
			if err != nil {
				t.Fatalf("confirmGitHubPullRequestTrust: %v", err)
			}
			if trusted != tc.wantTrust {
				t.Errorf("trusted = %t, want %t", trusted, tc.wantTrust)
			}
			printed := out.String()
			for _, want := range []string{"acme/api#7", "@octocat", inspection.HeadSHA, "files and credentials"} {
				if !strings.Contains(printed, want) {
					t.Errorf("prompt %q does not contain %q", printed, want)
				}
			}
		})
	}
}

func TestConfirmGitHubPullRequestTrustSanitizesTerminalControlCharacters(t *testing.T) {
	t.Parallel()
	inspection := daemonclient.GitHubPullRequestInspection{
		GitHubPullRequestLocator: daemonclient.GitHubPullRequestLocator{Owner: "acme", Repo: "api", Number: 7},
		Title:                    "safe\x1b[2J\nspoofed",
		Author:                   "octo\rbot",
		HeadSHA:                  "0123456789abcdef0123456789abcdef01234567",
	}
	var out bytes.Buffer
	if _, err := confirmGitHubPullRequestTrust(strings.NewReader("no\n"), &out, inspection); err != nil {
		t.Fatal(err)
	}
	printed := out.String()
	if strings.ContainsAny(printed, "\x1b\r") || !strings.Contains(printed, "safe [2J spoofed") || !strings.Contains(printed, "@octo bot") {
		t.Errorf("sanitized prompt = %q", printed)
	}
}

func TestPrintGitHubPullRequestCheckoutPrintsWorkingDirectoryOnOwnLine(t *testing.T) {
	t.Parallel()
	inspection := daemonclient.GitHubPullRequestInspection{
		GitHubPullRequestLocator: daemonclient.GitHubPullRequestLocator{Owner: "acme", Repo: "api", Number: 7},
		HeadSHA:                  "0123456789abcdef0123456789abcdef01234567",
	}
	launched := host.CreateWorktreeResult{
		Branch:      "feature",
		WorktreeDir: "/Users/alice/src/api-feature",
	}
	var out bytes.Buffer
	printGitHubPullRequestCheckout(&out, inspection, launched)

	printed := out.String()
	if !strings.Contains(printed, "Branch: feature\nWorking directory:\n/Users/alice/src/api-feature\n") {
		t.Fatalf("checkout output = %q", printed)
	}
}

func TestPreviewOverlayNamePrefersPullRequestDisplayName(t *testing.T) {
	t.Parallel()
	if got := previewOverlayName("/Users/alice/.clank/work/01KZ1EE2ZGSDW2Z970WCS8CDM5", "api#7"); got != "api#7" {
		t.Errorf("previewOverlayName = %q, want api#7", got)
	}
	if got := previewOverlayName("/Users/alice/src/api", ""); got != "api" {
		t.Errorf("default previewOverlayName = %q, want api", got)
	}
}
