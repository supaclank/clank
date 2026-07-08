package clankcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

// maxPickerSessions caps the interactive list to the most relevant rows.
const maxPickerSessions = 15

// errPickCanceled reports that the user dismissed the session picker.
var errPickCanceled = errors.New("session pick canceled")

// resolveTargetSession turns the targeting flags into a session id:
// --session wins outright, --to opens the interactive picker, neither
// means "" (create a new session).
func resolveTargetSession(ctx context.Context, client *daemonclient.Client, projectDir string, usePicker bool, sessionID string) (string, error) {
	if sessionID != "" {
		return sessionID, nil
	}
	if !usePicker {
		return "", nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return "", errors.New("the --to picker needs an interactive terminal; use --session <id> instead")
	}

	sessions, err := client.Sessions().List(ctx)
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	prefs, _ := config.LoadPreferences()
	ranked := rankSessionsForPick(sessions, projectDir, prefs.LastSessionByCwd[projectDir])
	if len(ranked) == 0 {
		return "", errors.New("no open sessions to send to; drop --to to start a new one")
	}

	final, err := tea.NewProgram(newSessionPickModel(ranked)).Run()
	if err != nil {
		return "", fmt.Errorf("session picker: %w", err)
	}
	m, ok := final.(sessionPickModel)
	if !ok || m.canceled || m.chosenID == "" {
		return "", errPickCanceled
	}
	return m.chosenID, nil
}

// rankSessionsForPick orders candidates for the picker: the cwd's last
// session first (one Enter = "the session I was just in"), then other
// sessions of this project dir, then everything else — each group by
// recency. Archived sessions are dropped, and the list is capped at
// maxPickerSessions.
func rankSessionsForPick(sessions []agent.SessionInfo, projectDir, lastSessionID string) []agent.SessionInfo {
	kept := make([]agent.SessionInfo, 0, len(sessions))
	for _, s := range sessions {
		if s.Visibility == agent.VisibilityArchived {
			continue
		}
		kept = append(kept, s)
	}
	rank := func(s agent.SessionInfo) int {
		switch {
		case s.ID == lastSessionID:
			return 0
		case projectDir != "" && s.GitRef.LocalPath == projectDir:
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		ri, rj := rank(kept[i]), rank(kept[j])
		if ri != rj {
			return ri < rj
		}
		return kept[i].UpdatedAt.After(kept[j].UpdatedAt)
	})
	if len(kept) > maxPickerSessions {
		kept = kept[:maxPickerSessions]
	}
	return kept
}

// sessionPickModel is a minimal list picker: arrows/j/k move, Enter
// picks, Esc/q cancels.
type sessionPickModel struct {
	sessions []agent.SessionInfo
	cursor   int
	chosenID string
	canceled bool
}

func newSessionPickModel(sessions []agent.SessionInfo) sessionPickModel {
	return sessionPickModel{sessions: sessions}
}

func (m sessionPickModel) Init() tea.Cmd { return nil }

func (m sessionPickModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.sessions)-1 {
			m.cursor++
		}
	case "enter":
		m.chosenID = m.sessions[m.cursor].ID
		return m, tea.Quit
	case "esc", "q", "ctrl+c":
		m.canceled = true
		return m, tea.Quit
	}
	return m, nil
}

func (m sessionPickModel) View() tea.View {
	var b strings.Builder
	b.WriteString("Send to which session? (enter picks, esc cancels)\n\n")
	for i, s := range m.sessions {
		marker := "  "
		if i == m.cursor {
			marker = "› "
		}
		b.WriteString(marker + sessionPickRow(s, time.Now()) + "\n")
	}
	return tea.NewView(b.String())
}

// sessionPickRow renders one candidate line: title (or prompt), short
// id, status, and age.
func sessionPickRow(s agent.SessionInfo, now time.Time) string {
	title := s.Title
	if title == "" {
		title = previewPrompt(s.Prompt)
	}
	if title == "" {
		title = "(untitled)"
	}
	return fmt.Sprintf("%s  [%s, %s, %s]", title, shortSessionID(s.ID), s.Status, humanAge(now.Sub(s.UpdatedAt)))
}

// shortSessionID keeps the row scannable; full ids are ULIDs.
func shortSessionID(id string) string {
	const n = 8
	if len(id) <= n {
		return id
	}
	return id[:n]
}

// humanAge renders a compact "how long ago" for picker rows.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
