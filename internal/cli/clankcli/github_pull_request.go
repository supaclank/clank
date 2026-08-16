package clankcli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/host"
)

// TODO(ai-review): duplicated with gateway's githubPullRequestLaunchTimeout; centralize if they need to diverge or a third copy appears. https://github.com/supaclank/clank/pull/217
const githubPullRequestLaunchTimeout = 10 * time.Minute

func parseGitHubPullRequestURL(rawURL string) (daemonclient.GitHubPullRequestLocator, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return daemonclient.GitHubPullRequestLocator{}, fmt.Errorf("parse GitHub pull request URL: %w", err)
	}
	if u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") {
		return daemonclient.GitHubPullRequestLocator{}, fmt.Errorf("pull request URL must use https://github.com")
	}
	if u.User != nil {
		return daemonclient.GitHubPullRequestLocator{}, fmt.Errorf("GitHub pull request URL must not contain user information")
	}
	if u.RawQuery != "" {
		return daemonclient.GitHubPullRequestLocator{}, fmt.Errorf("GitHub pull request URL must not contain a query")
	}
	if u.Fragment != "" {
		return daemonclient.GitHubPullRequestLocator{}, fmt.Errorf("GitHub pull request URL must not contain a fragment")
	}
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return daemonclient.GitHubPullRequestLocator{}, fmt.Errorf("expected a GitHub pull request URL like https://github.com/owner/repo/pull/123")
	}
	owner, err := url.PathUnescape(parts[0])
	if err != nil {
		return daemonclient.GitHubPullRequestLocator{}, fmt.Errorf("decode GitHub owner: %w", err)
	}
	repo, err := url.PathUnescape(parts[1])
	if err != nil {
		return daemonclient.GitHubPullRequestLocator{}, fmt.Errorf("decode GitHub repo: %w", err)
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number < 1 {
		return daemonclient.GitHubPullRequestLocator{}, fmt.Errorf("GitHub pull request number must be a positive integer")
	}
	if !validGitHubURLName(owner) || !validGitHubURLName(repo) {
		return daemonclient.GitHubPullRequestLocator{}, fmt.Errorf("GitHub pull request URL contains an invalid owner or repo")
	}
	return daemonclient.GitHubPullRequestLocator{Owner: owner, Repo: repo, Number: number}, nil
}

func validGitHubURLName(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func isWebURLArg(arg string) bool {
	return strings.HasPrefix(arg, "https://") || strings.HasPrefix(arg, "http://")
}

func confirmGitHubPullRequestTrust(in io.Reader, out io.Writer, pr daemonclient.GitHubPullRequestInspection) (bool, error) {
	_, err := fmt.Fprintf(out, `
GitHub pull request: %s/%s#%d — %s
Author: @%s
Commit: %s

This will run code from that exact commit with access to the files and credentials
available on this host. Only continue if you trust the author and this revision.

Trust and run this pull request? [y/N] `,
		pr.Owner, pr.Repo, pr.Number, terminalSafeGitHubText(pr.Title), terminalSafeGitHubText(pr.Author), pr.HeadSHA)
	if err != nil {
		return false, err
	}
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, fmt.Errorf("read trust confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func terminalSafeGitHubText(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

func runGitHubPullRequestPreview(locator daemonclient.GitHubPullRequestLocator, launchName, backend string, port int, in io.Reader, out io.Writer) error {
	client, _, startedDaemon, err := ensurePreviewDaemon()
	if err != nil {
		return err
	}
	if startedDaemon {
		defer func() {
			_, _ = fmt.Fprintln(out, "Stopping the daemon clank preview started…")
			stopLocalDaemon()
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	inspection, err := client.GitHubPullRequestInspect(ctx, locator)
	if err != nil {
		return fmt.Errorf("inspect GitHub pull request: %w", err)
	}
	trusted, err := confirmGitHubPullRequestTrust(in, out, inspection)
	if err != nil {
		return err
	}
	if !trusted {
		_, _ = fmt.Fprintln(out, "Canceled; no code was downloaded or run.")
		return nil
	}

	launchCtx, cancel := context.WithTimeout(ctx, githubPullRequestLaunchTimeout)
	defer cancel()
	launched, err := client.GitHubPullRequestLaunch(launchCtx, daemonclient.GitHubPullRequestLaunchRequest{
		GitHubPullRequestLocator: locator,
		ExpectedHeadSHA:          inspection.HeadSHA,
	})
	if err != nil {
		return fmt.Errorf("launch GitHub pull request: %w", err)
	}
	printGitHubPullRequestCheckout(out, inspection, launched)
	_, statErr := os.Stat(launched.WorktreeDir)
	switch {
	case statErr == nil:
		return runPreviewWithDisplayName(launched.WorktreeDir, launchName, backend, port, launched.DisplayName, "")
	case !os.IsNotExist(statErr):
		return fmt.Errorf("inspect launched worktree: %w", statErr)
	case port != 0:
		return fmt.Errorf("--port only applies to previews running on this machine")
	default:
		return runHostedGitHubPullRequestPreview(ctx, client, launched.WorktreeID, launchName, backend, in, out)
	}
}

func printGitHubPullRequestCheckout(out io.Writer, inspection daemonclient.GitHubPullRequestInspection, launched host.CreateWorktreeResult) {
	_, _ = fmt.Fprintf(out, "Checked out %s/%s#%d at %s.\nBranch: %s\nWorking directory:\n%s\n",
		inspection.Owner, inspection.Repo, inspection.Number, inspection.HeadSHA, launched.Branch, launched.WorktreeDir)
}
