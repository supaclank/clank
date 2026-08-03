package github

// Draft → ready-for-review flip for one PR. GitHub's REST API cannot
// change a PR's draft state — only the GraphQL mutation
// markPullRequestReadyForReview can (the same call `gh pr ready`
// makes) — so this file speaks one hand-rolled GraphQL request over
// the Manager's HTTP client. REST supplies the PR's GraphQL node id.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gogithub "github.com/google/go-github/v66/github"
)

// markReadyMutation flips a draft PR to ready-for-review. $id is the
// PR's GraphQL node id (`node_id` in REST responses).
const markReadyMutation = `mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { isDraft } } }`

// MarkPRReadyForReview flips PR `number` from draft to
// ready-for-review. Idempotent: an already-ready PR is a successful
// no-op (no mutation is sent).
func (m *Manager) MarkPRReadyForReview(ctx context.Context, accessToken, owner, repo string, number int) error {
	client, err := m.apiClient(accessToken)
	if err != nil {
		return fmt.Errorf("build api client: %w", err)
	}
	pr, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return classifyPRFetchError(err)
	}
	if !pr.GetDraft() {
		return nil
	}
	nodeID := pr.GetNodeID()
	if nodeID == "" {
		return fmt.Errorf("pr %s/%s#%d: response carried no node_id", owner, repo, number)
	}
	// TODO(ai-review): a PR flipped to ready between this GET and the mutation below surfaces as a hard error instead of the idempotent no-op the doc comment promises. https://github.com/supaclank/clank/pull/165#discussion_r3598342520 https://github.com/supaclank/clank/pull/165#discussion_r3598342551
	return m.graphQL(ctx, accessToken, markReadyMutation, map[string]any{"id": nodeID})
}

// classifyPRFetchError maps REST GET /pulls/{n} failures onto the
// sentinels writePRError already understands.
func classifyPRFetchError(err error) error {
	var er *gogithub.ErrorResponse
	if !errors.As(err, &er) {
		return fmt.Errorf("get pull request: %w", err)
	}
	switch er.Response.StatusCode {
	case http.StatusUnauthorized:
		return ErrPRTokenInvalid
	case http.StatusForbidden, http.StatusNotFound:
		return ErrPRForbidden // GitHub 404s "no access" — same UX as 403
	}
	return fmt.Errorf("get pull request: %w", err)
}

// graphQL POSTs one operation to the API's /graphql endpoint.
// GraphQL-level failures arrive with HTTP 200 — the errors array is
// the real signal; FORBIDDEN maps onto ErrPRForbidden so callers get
// the same sentinel as REST-side permission failures.
func (m *Manager) graphQL(ctx context.Context, accessToken, query string, variables map[string]any) error {
	payload, err := json.Marshal(struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}{Query: query, Variables: variables})
	if err != nil {
		return fmt.Errorf("marshal graphql payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.apiBaseURL+"/graphql", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build graphql request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("graphql request: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return ErrPRTokenInvalid
	case http.StatusForbidden:
		return ErrPRForbidden
	default:
		return fmt.Errorf("graphql: unexpected status %d", resp.StatusCode)
	}
	var body struct {
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("decode graphql response: %w", err)
	}
	if len(body.Errors) == 0 {
		return nil
	}
	var messages []string
	for _, gqlErr := range body.Errors {
		if gqlErr.Type == "FORBIDDEN" {
			return ErrPRForbidden
		}
		messages = append(messages, gqlErr.Message)
	}
	return fmt.Errorf("graphql: %s", strings.Join(messages, "; "))
}
