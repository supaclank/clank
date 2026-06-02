package syncclient

import (
	"errors"
	"fmt"
)

// ErrWorktreeNotRegistered means the remote has no worktree row for the
// id we pushed — typically a worktree deleted on the remote while the
// laptop's cached id went stale. clank push self-heals by re-registering
// and retrying, so the user never has to `rm -r .git/clank` + re-init.
var ErrWorktreeNotRegistered = errors.New("syncclient: worktree not registered with the remote")

// httpError carries a non-2xx response from a control-plane JSON call so
// callers with semantic context (e.g. "this was the checkpoint-create
// path, so a 404 means the worktree is gone") can branch on the status
// code. Error() preserves the prior "post <path>: <status>: <body>"
// wording so existing string-matching callers/logs are unaffected.
type httpError struct {
	Path   string
	Status int
	Body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("post %s: %d: %s", e.Path, e.Status, e.Body)
}
