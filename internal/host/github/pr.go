package github

// GitHub PR creation. Wraps two go-github calls:
//   client.PullRequests.Create(...) — open a new PR
//   client.PullRequests.List(...)   — fired after a 422 "already
//                                     exists" to surface the existing
//                                     PR URL so the UI can deep-link.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gogithub "github.com/google/go-github/v66/github"
)

// CreatePRInput is the request body for CreatePullRequest. Head is
// just the branch name — same-owner PRs only in v1; cross-fork
// support would require "owner:branch" syntax.
type CreatePRInput struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Draft bool   `json:"draft,omitempty"`
}

// PullRequest mirrors the subset of GitHub's response we surface
// upstream — the broader go-github type leaks through the package
// boundary otherwise.
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
	client, err := m.apiClient(accessToken)
	if err != nil {
		return PullRequest{}, fmt.Errorf("build api client: %w", err)
	}
	req := &gogithub.NewPullRequest{
		Title: gogithub.String(in.Title),
		Body:  gogithub.String(in.Body),
		Head:  gogithub.String(in.Head),
		Base:  gogithub.String(in.Base),
		Draft: gogithub.Bool(in.Draft),
	}
	pr, _, err := client.PullRequests.Create(ctx, owner, repo, req)
	if err != nil {
		return PullRequest{}, m.classifyPRError(ctx, err, client, owner, repo, in.Head)
	}
	return wirePR(pr), nil
}

// classifyPRError maps a *github.ErrorResponse from CreatePullRequest
// to one of the exported sentinel errors. For 422 "already exists",
// does the follow-up GET to capture the existing PR's URL.
//
// The 422 sub-cases are matched off go-github's structured Error
// fields (Resource / Field / Code) rather than the top-level Message
// string — see isAlreadyExists / isBaseInvalid for the GitHub-side
// shapes we discriminate on.
func (m *Manager) classifyPRError(ctx context.Context, err error, client *gogithub.Client, owner, repo, head string) error {
	var er *gogithub.ErrorResponse
	if !errors.As(err, &er) {
		return fmt.Errorf("create pr: %w", err)
	}
	switch er.Response.StatusCode {
	case http.StatusUnauthorized:
		return ErrPRTokenInvalid
	case http.StatusForbidden:
		return ErrPRForbidden
	case http.StatusNotFound:
		return ErrPRForbidden // GitHub returns 404 for "no access" — same UX as 403
	case http.StatusUnprocessableEntity:
		if isAlreadyExists(er) {
			existingURL, _ := m.lookupExistingPR(ctx, client, owner, repo, head)
			return &existingPRError{URL: existingURL}
		}
		if isBaseInvalid(er) {
			return ErrPRBaseNotFound
		}
	}
	return fmt.Errorf("create pr: %w", err)
}

// isAlreadyExists matches the 422 GitHub sends when a PR already
// exists for head:base:
//
//	{"resource": "PullRequest", "code": "custom",
//	 "message": "A pull request already exists for owner:branch."}
//
// GitHub has no dedicated structured code for this case — Code is
// the generic "custom" — so we still string-match the message, but
// gate it on Resource + Code so an unrelated "custom" error can't
// fire a false positive.
func isAlreadyExists(er *gogithub.ErrorResponse) bool {
	for _, e := range er.Errors {
		if e.Resource == "PullRequest" && e.Code == "custom" &&
			strings.Contains(strings.ToLower(e.Message), "pull request already exists") {
			return true
		}
	}
	return false
}

// isBaseInvalid matches the 422 GitHub sends when the base branch
// doesn't exist:
//
//	{"resource": "PullRequest", "field": "base", "code": "invalid"}
//
// Fully structured — no message inspection required.
func isBaseInvalid(er *gogithub.ErrorResponse) bool {
	for _, e := range er.Errors {
		if e.Resource == "PullRequest" && e.Field == "base" && e.Code == "invalid" {
			return true
		}
	}
	return false
}

// lookupExistingPR finds the open PR for owner:head and returns its
// HTML URL. Best-effort — failure here just means the UI shows the
// "already exists" error without a deep-link.
func (m *Manager) lookupExistingPR(ctx context.Context, client *gogithub.Client, owner, repo, head string) (string, error) {
	prs, _, err := client.PullRequests.List(ctx, owner, repo, &gogithub.PullRequestListOptions{
		Head:  owner + ":" + head,
		State: "open",
	})
	if err != nil {
		return "", err
	}
	if len(prs) == 0 {
		return "", errors.New("no open PR found for head")
	}
	return prs[0].GetHTMLURL(), nil
}

// wirePR collapses go-github's PullRequest type to the trimmed
// PullRequest we return upstream.
func wirePR(pr *gogithub.PullRequest) PullRequest {
	out := PullRequest{
		Number:  pr.GetNumber(),
		HTMLURL: pr.GetHTMLURL(),
	}
	if pr.Head != nil {
		out.Head.SHA = pr.GetHead().GetSHA()
	}
	return out
}
