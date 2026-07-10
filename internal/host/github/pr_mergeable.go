package github

// Mergeability of one PR, via GET /repos/{owner}/{repo}/pulls/{number}
// — the list endpoint never carries the mergeable bit, so this is a
// per-PR call.

import (
	"context"
	"fmt"
)

// MergeableState is whether a PR can merge cleanly into its base.
type MergeableState string

const (
	MergeableStateMergeable   MergeableState = "mergeable"
	MergeableStateConflicting MergeableState = "conflicting"
	// MergeableStateUnknown means GitHub hasn't computed the test merge
	// yet. The GET itself kicks the computation off, so a later refresh
	// resolves it — callers should treat unknown as "not yet", not retry
	// inline.
	MergeableStateUnknown MergeableState = "unknown"
)

// PRMergeable reports whether PR number merges cleanly into its base.
func (m *Manager) PRMergeable(ctx context.Context, token, owner, repo string, number int) (MergeableState, error) {
	client, err := m.apiClient(token)
	if err != nil {
		return MergeableStateUnknown, fmt.Errorf("build api client: %w", err)
	}
	pr, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return MergeableStateUnknown, fmt.Errorf("get pull request: %w", err)
	}
	if pr.Mergeable == nil {
		return MergeableStateUnknown, nil
	}
	if pr.GetMergeable() {
		return MergeableStateMergeable, nil
	}
	return MergeableStateConflicting, nil
}
