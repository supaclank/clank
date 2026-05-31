package clankcli

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// Push progress messages forwarded from the upload goroutine into the
// bubbletea program (tea.Program.Send is goroutine-safe).
type (
	pushPhaseMsg string
	pushSizedMsg int64
	pushBytesMsg int64
	pushDoneMsg  struct {
		res *syncclient.CheckpointResult
		err error
	}
)

// teaPushObserver adapts syncclient.PushObserver onto a bubbletea program.
type teaPushObserver struct{ send func(tea.Msg) }

func (o teaPushObserver) Phase(name string)         { o.send(pushPhaseMsg(name)) }
func (o teaPushObserver) UploadSized(total int64)   { o.send(pushSizedMsg(total)) }
func (o teaPushObserver) UploadProgress(done int64) { o.send(pushBytesMsg(done)) }

// pushProgressModel renders a spinner + phase + a byte progress bar and
// names the remote the push is uploading to.
type pushProgressModel struct {
	spinner  spinner.Model
	bar      progress.Model
	remote   string
	phase    string
	total    int64
	uploaded int64
	res      *syncclient.CheckpointResult
	err      error
	finished bool
}

func newPushProgressModel(remote string) pushProgressModel {
	return pushProgressModel{
		spinner: spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		bar:     progress.New(progress.WithWidth(30), progress.WithoutPercentage()),
		remote:  remote,
		phase:   "Preparing",
	}
}

func (m pushProgressModel) Init() tea.Cmd { return m.spinner.Tick }

func (m pushProgressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case pushPhaseMsg:
		m.phase = string(msg)
	case pushSizedMsg:
		m.total = int64(msg)
	case pushBytesMsg:
		m.uploaded = int64(msg)
	case pushDoneMsg:
		m.res, m.err, m.finished = msg.res, msg.err, true
		return m, tea.Quit
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m pushProgressModel) View() tea.View {
	if m.finished {
		return tea.NewView("") // clear once we're done; the caller prints the result.
	}
	dest := m.remote
	if dest == "" {
		dest = "remote"
	}
	header := fmt.Sprintf("%s %s → %s", m.spinner.View(), m.phase, dest)
	if m.total == 0 {
		return tea.NewView(header + "\n")
	}
	pct := float64(m.uploaded) / float64(m.total)
	if pct > 1 {
		pct = 1
	}
	line := fmt.Sprintf("%s  %s / %s", m.bar.ViewAs(pct), humanBytes(m.uploaded), humanBytes(m.total))
	return tea.NewView(header + "\n" + line + "\n")
}

// pushWithProgress runs PushCheckpoint, rendering a live progress UI on a
// TTY (spinner + phase + bytes uploaded / total + remote host) and falling
// back to a silent push otherwise (autopush hooks). The push runs in a
// goroutine that forwards progress into the program; its result returns
// via pushDoneMsg, so there's no shared mutable state across goroutines.
func pushWithProgress(cmd *cobra.Command, ctx context.Context, cli *syncclient.Client, absRepo, worktreeID, base string, interactive bool) (*syncclient.CheckpointResult, error) {
	if !interactive {
		return cli.PushCheckpoint(ctx, worktreeID, absRepo, base, nil)
	}
	p := tea.NewProgram(
		newPushProgressModel(remoteLabel(cli.BaseURL())),
		tea.WithInput(cmd.InOrStdin()),
		tea.WithOutput(cmd.OutOrStdout()),
	)
	go func() {
		res, err := cli.PushCheckpoint(ctx, worktreeID, absRepo, base, teaPushObserver{send: p.Send})
		p.Send(pushDoneMsg{res: res, err: err})
	}()
	final, err := p.Run()
	if errors.Is(err, tea.ErrInterrupted) {
		return nil, fmt.Errorf("push canceled")
	}
	if err != nil {
		return nil, fmt.Errorf("progress UI: %w", err)
	}
	fm, ok := final.(pushProgressModel)
	if !ok || !fm.finished {
		return nil, fmt.Errorf("push canceled")
	}
	return fm.res, fm.err
}

// remoteLabel renders a friendly host label for a gateway URL.
func remoteLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

// humanBytes formats a byte count compactly (e.g. "57.8 MB").
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
