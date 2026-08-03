package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/supaclank/clank/internal/agent"
	daemonclient "github.com/supaclank/clank/internal/daemonclient"
)

// ErrSessionTaskTimeout classifies an inline task that exceeded its bound.
var ErrSessionTaskTimeout = errors.New("agent task timed out")

// TaskOptions defines the bounded inline presentation for one agent turn.
type TaskOptions struct {
	Title           string
	Timeout         time.Duration
	MaxVisibleLines int
}

// TaskResult describes why an inline session task stopped.
type TaskResult struct {
	Status agent.SessionStatus
	Err    error
}

// SessionTaskModel is a non-fullscreen, non-composing presentation of the
// existing session model. It retains the session event and permission paths.
type SessionTaskModel struct {
	session *SessionViewModel
	options TaskOptions
	result  TaskResult

	infoLoaded        bool
	historyLoaded     bool
	permissionsLoaded bool
	isStopping        bool
}

type sessionTaskTimeoutMsg struct{}

type sessionTaskAbortResultMsg struct {
	err error
}

// NewSessionTaskModel creates an inline view that exits when one turn settles.
func NewSessionTaskModel(client *daemonclient.Client, sessionID string, options TaskOptions) (*SessionTaskModel, error) {
	if strings.TrimSpace(options.Title) == "" {
		return nil, fmt.Errorf("session task title is required")
	}
	if options.Timeout <= 0 {
		return nil, fmt.Errorf("session task timeout must be positive")
	}
	if options.MaxVisibleLines <= 0 {
		return nil, fmt.Errorf("session task visible line count must be positive")
	}
	return &SessionTaskModel{
		session: NewSessionViewModel(client, sessionID),
		options: options,
	}, nil
}

// SetEventChannel provides the same race-free preconnected event stream used
// by the full session view.
func (m *SessionTaskModel) SetEventChannel(ch <-chan agent.Event, cancel context.CancelFunc) {
	m.session.SetEventChannel(ch, cancel)
}

// Result returns the terminal status after the program exits.
func (m *SessionTaskModel) Result() TaskResult {
	return m.result
}

func (m *SessionTaskModel) Init() tea.Cmd {
	return tea.Batch(
		m.session.Init(),
		tea.Tick(m.options.Timeout, func(time.Time) tea.Msg { return sessionTaskTimeoutMsg{} }),
	)
}

func (m *SessionTaskModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case sessionTaskTimeoutMsg:
		if m.isStopping {
			return m, nil
		}
		m.isStopping = true
		m.result.Err = fmt.Errorf("%w after %s", ErrSessionTaskTimeout, m.options.Timeout)
		return m, m.abortSession()
	case sessionTaskAbortResultMsg:
		if msg.err != nil {
			m.result.Err = errors.Join(m.result.Err, fmt.Errorf("abort session: %w", msg.err))
		}
		m.stopEvents()
		return m, tea.Quit
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.isStopping {
				return m, nil
			}
			m.isStopping = true
			m.result.Err = errors.New("agent task cancelled")
			return m, m.abortSession()
		case "y", "Y", "n", "N":
			// The existing session model owns permission decisions.
		default:
			// Task mode has no composer, menus, or session-management keys.
			return m, nil
		}
	}

	switch msg.(type) {
	case sessionInfoMsg:
		m.infoLoaded = true
	case sessionMessagesMsg:
		m.historyLoaded = true
	case pendingPermissionMsg:
		m.permissionsLoaded = true
	case pendingPermissionErrMsg:
		m.permissionsLoaded = true
	}

	_, cmd := m.session.Update(msg)
	if taskErr, ok := msg.(sessionEventsErrMsg); ok {
		m.result.Err = taskErr.err
		m.stopEvents()
		return m, tea.Quit
	}
	if m.isSettled() {
		m.result.Status = m.session.info.Status
		if m.result.Status == agent.StatusError || m.result.Status == agent.StatusDead {
			m.result.Err = fmt.Errorf("agent session ended with status %s", m.result.Status)
		}
		m.stopEvents()
		return m, tea.Quit
	}
	return m, cmd
}

func (m *SessionTaskModel) isSettled() bool {
	if !m.infoLoaded || !m.historyLoaded || !m.permissionsLoaded || m.session.info == nil || len(m.session.pendingPerms) != 0 {
		return false
	}
	switch m.session.info.Status {
	case agent.StatusIdle, agent.StatusError, agent.StatusDead:
		return true
	default:
		return false
	}
}

func (m *SessionTaskModel) abortSession() tea.Cmd {
	client := m.session.client
	sessionID := m.session.sessionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return sessionTaskAbortResultMsg{err: client.Session(sessionID).Abort(ctx)}
	}
}

func (m *SessionTaskModel) stopEvents() {
	if m.session.cancelEvents != nil {
		m.session.cancelEvents()
		m.session.cancelEvents = nil
	}
}
