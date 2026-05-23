package github

// GitHub PR creation. Two API calls live here:
//   POST /repos/{owner}/{repo}/pulls  — open a new PR
//   GET  /repos/{owner}/{repo}/pulls  — used after a 422 "already
//                                       exists" to surface the
//                                       existing PR URL in the
//                                       error, so the UI can deep-
//                                       link instead of just showing
//                                       a generic conflict.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CreatePRInput is the request body for CreatePullRequest. Title and
// body come from the user (the mobile/TUI client prefills with commit
// messages, but the host stays presentation-agnostic). Head is just
// the branch name — same-owner PRs only in v1; cross-fork support
// would require "owner:branch" syntax.
type CreatePRInput struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Draft bool   `json:"draft,omitempty"`
}

// PullRequest mirrors the subset of GitHub's response we care about.
type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// Errors returned by CreatePullRequest. ErrPRAlreadyExists carries
// the existing PR URL so the mux handler can surface it in the
// response body — the UI uses it to deep-link instead of showing a
// dead-end "conflict" error.
var (
	// ErrPRAlreadyExists is GitHub's 422 "A pull request already
	// exists for X:Y" error. ExistingURL is the URL of the open PR
	// we found via the follow-up search.
	ErrPRAlreadyExists = errors.New("github pr: already exists for this head")

	// ErrPRBaseNotFound covers 422 errors where the base branch
	// doesn't exist on the remote. Distinct from "already exists" so
	// the UI can show the right hint.
	ErrPRBaseNotFound = errors.New("github pr: base branch not found")

	// ErrPRTokenInvalid is GitHub's 401 — typically means the token
	// was revoked or expired. UI should prompt the user to reconnect.
	ErrPRTokenInvalid = errors.New("github pr: token invalid or revoked")

	// ErrPRForbidden is GitHub's 403 — token can't write to this
	// repo (e.g. lacks `repo` scope, or repo doesn't accept PRs from
	// this account).
	ErrPRForbidden = errors.New("github pr: forbidden")
)

// existingPRError wraps ErrPRAlreadyExists with the URL of the open
// PR. The mux handler unwraps to render the URL in the 409 body.
type existingPRError struct {
	URL string
}

func (e *existingPRError) Error() string {
	return fmt.Sprintf("%s (existing: %s)", ErrPRAlreadyExists.Error(), e.URL)
}

func (e *existingPRError) Unwrap() error { return ErrPRAlreadyExists }

// ExistingURLFromError extracts the existing-PR URL from an error
// returned by CreatePullRequest. Returns "" when the error isn't
// the "already exists" case or carries no URL.
func ExistingURLFromError(err error) string {
	var e *existingPRError
	if errors.As(err, &e) {
		return e.URL
	}
	return ""
}

// CreatePullRequest opens a PR on the named repo with the given
// token. On 422 "already exists", looks up the existing PR via a
// follow-up GET and returns ErrPRAlreadyExists with the URL embedded.
func (m *Manager) CreatePullRequest(ctx context.Context, accessToken, owner, repo string, in CreatePRInput) (PullRequest, error) {
	buf, err := json.Marshal(in)
	if err != nil {
		return PullRequest{}, fmt.Errorf("marshal pr body: %w", err)
	}
	endpoint := m.apiBaseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/pulls"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return PullRequest{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	var pr PullRequest
	if err := m.doJSON(req, &pr); err != nil {
		return PullRequest{}, m.classifyPRError(ctx, err, accessToken, owner, repo, in.Head)
	}
	return pr, nil
}

// classifyPRError maps an HTTPError from CreatePullRequest to one
// of the exported sentinel errors. For 422 "already exists", does
// the follow-up GET to capture the existing PR's URL.
func (m *Manager) classifyPRError(ctx context.Context, err error, accessToken, owner, repo, head string) error {
	var he *HTTPError
	if !errors.As(err, &he) {
		return fmt.Errorf("create pr: %w", err)
	}
	switch he.Status {
	case http.StatusUnauthorized:
		return ErrPRTokenInvalid
	case http.StatusForbidden:
		return ErrPRForbidden
	case http.StatusNotFound:
		return ErrPRForbidden // GitHub returns 404 for "no access" — same UX as 403
	case http.StatusUnprocessableEntity:
		// Two cases live under 422: already-exists and base-not-found.
		// GitHub's body has `errors[].message` we can switch on.
		low := strings.ToLower(he.Body)
		switch {
		case strings.Contains(low, "pull request already exists"):
			existingURL, _ := m.lookupExistingPR(ctx, accessToken, owner, repo, head)
			return &existingPRError{URL: existingURL}
		case strings.Contains(low, "base") && (strings.Contains(low, "invalid") || strings.Contains(low, "not exist")):
			return ErrPRBaseNotFound
		}
	}
	return fmt.Errorf("create pr: %w", err)
}

// lookupExistingPR finds the open PR for owner:head and returns its
// HTML URL. Best-effort — failure here just means the UI shows the
// "already exists" error without a deep-link.
func (m *Manager) lookupExistingPR(ctx context.Context, accessToken, owner, repo, head string) (string, error) {
	q := url.Values{}
	q.Set("head", owner+":"+head)
	q.Set("state", "open")
	endpoint := m.apiBaseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/pulls?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	var out []PullRequest
	if err := m.doJSON(req, &out); err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "", errors.New("no open PR found for head")
	}
	return out[0].HTMLURL, nil
}
