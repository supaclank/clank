package host

// Hardcoded commit-message templates for remote-sync auto-commits. v1
// keeps these fixed (no AI/user input). The "clank:" prefix lets users
// recognise commits the remote-sync flow made on their behalf; the branch
// name keeps the message meaningful in `git log`. Upgrading to an
// AI-suggested or user-supplied message is a later, additive change.
const (
	pushCommitMessagePrefix  = "clank: update "
	mergeCommitMessagePrefix = "clank: merge origin/"
)

func pushCommitMessage(branch string) string  { return pushCommitMessagePrefix + branch }
func mergeCommitMessage(branch string) string { return mergeCommitMessagePrefix + branch }
