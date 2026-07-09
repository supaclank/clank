package github

// CI status for a commit, aggregated from the Checks API (one call per
// ref). GitHub Actions and modern CI apps report through check runs;
// the legacy commit-status API is a separate feed we don't consume, so
// status-only integrations won't show up here.

import (
	"context"
	"fmt"

	gogithub "github.com/google/go-github/v66/github"
)

// checkRunsPerPage is the page size for the check-run listing pagination.
const checkRunsPerPage = 100

// CheckRollupState is the one-glance CI verdict for a commit.
type CheckRollupState string

const (
	CheckStatePassing CheckRollupState = "passing"
	CheckStateFailing CheckRollupState = "failing"
	CheckStatePending CheckRollupState = "pending"
)

// Check-run status/conclusion values from the GitHub Checks API.
const (
	checkRunStatusCompleted = "completed"

	checkRunConclusionSuccess = "success"
	checkRunConclusionNeutral = "neutral"
	checkRunConclusionSkipped = "skipped"
)

// CheckRollup aggregates a commit's check runs: the overall State plus
// the per-bucket counts (Passed + Failed + Pending == Total).
type CheckRollup struct {
	State   CheckRollupState `json:"state"`
	Passed  int              `json:"passed"`
	Failed  int              `json:"failed"`
	Pending int              `json:"pending"`
	Total   int              `json:"total"`
}

// CheckRollupForRef aggregates the latest check runs on ref (a SHA or
// branch). Returns nil when the commit has no check runs at all — "no
// CI configured" renders as nothing, not as green.
func (m *Manager) CheckRollupForRef(ctx context.Context, token, owner, repo, ref string) (*CheckRollup, error) {
	client, err := m.apiClient(token)
	if err != nil {
		return nil, fmt.Errorf("build api client: %w", err)
	}

	opts := &gogithub.ListCheckRunsOptions{
		ListOptions: gogithub.ListOptions{PerPage: checkRunsPerPage},
	}
	var runs []*gogithub.CheckRun
	for {
		results, resp, err := client.Checks.ListCheckRunsForRef(ctx, owner, repo, ref, opts)
		if err != nil {
			return nil, fmt.Errorf("list check runs: %w", err)
		}
		runs = append(runs, results.CheckRuns...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if len(runs) == 0 {
		return nil, nil
	}
	rollup := rollupCheckRuns(runs)
	return &rollup, nil
}

// rollupCheckRuns buckets check runs the way GitHub's PR list does:
// success/neutral/skipped pass, any other completed conclusion fails,
// anything not completed is pending. Failing dominates pending so a red
// verdict shows as soon as one check fails, even mid-run.
func rollupCheckRuns(runs []*gogithub.CheckRun) CheckRollup {
	rollup := CheckRollup{Total: len(runs)}
	for _, run := range runs {
		switch {
		case run.GetStatus() != checkRunStatusCompleted:
			rollup.Pending++
		case isPassingConclusion(run.GetConclusion()):
			rollup.Passed++
		default:
			rollup.Failed++
		}
	}
	switch {
	case rollup.Failed > 0:
		rollup.State = CheckStateFailing
	case rollup.Pending > 0:
		rollup.State = CheckStatePending
	default:
		rollup.State = CheckStatePassing
	}
	return rollup
}

// isPassingConclusion reports whether a completed check run counts as
// passed. Everything else (failure, timed_out, cancelled,
// action_required, stale, startup_failure) counts as failed.
func isPassingConclusion(conclusion string) bool {
	switch conclusion {
	case checkRunConclusionSuccess, checkRunConclusionNeutral, checkRunConclusionSkipped:
		return true
	}
	return false
}
