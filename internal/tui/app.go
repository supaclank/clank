package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/acksell/clank/internal/analyzer"
	"github.com/acksell/clank/internal/store"
)

type screen int

const (
	screenList screen = iota
	screenDetail
	screenTriage
)

type App struct {
	store    *store.Store
	analyzer *analyzer.Analyzer
	context  string

	screen screen
	list   listModel
	detail detailModel
	triage triageModel

	tickets []store.Ticket
	width   int
	height  int
}

func NewApp(s *store.Store, a *analyzer.Analyzer, centralCtx string) *App {
	return &App{
		store:    s,
		analyzer: a,
		context:  centralCtx,
		screen:   screenList,
	}
}

func (a *App) Init() tea.Cmd {
	return tea.WindowSize()
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.loadTickets()

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return a, tea.Quit
		case "q":
			if a.screen == screenList {
				return a, tea.Quit
			}
		case "a":
			if a.screen == screenList {
				idx := a.list.table.Cursor()
				if idx >= 0 && idx < len(a.tickets) && a.analyzer != nil {
					t := &a.tickets[idx]
					a.triage = newTriageModel(t, a.analyzer, a.context, a.width, a.height)
					a.screen = screenTriage
					return a, a.triage.Init()
				}
			}
		}

	case selectTicketMsg:
		if msg.index >= 0 && msg.index < len(a.tickets) {
			a.detail = newDetailModel(&a.tickets[msg.index], a.width, a.height)
			a.screen = screenDetail
		}

	case backToListMsg:
		a.screen = screenList
		a.list = newListModel(a.tickets, a.width, a.height)

	case backToDetailMsg:
		a.screen = screenDetail

	case ticketUpdatedMsg:
		if err := a.store.SaveTicket(&msg.ticket); err == nil {
			for i, t := range a.tickets {
				if t.ID == msg.ticket.ID {
					a.tickets[i] = msg.ticket
					break
				}
			}
		}
		if a.screen == screenTriage {
			a.detail = newDetailModel(&msg.ticket, a.width, a.height)
			a.screen = screenDetail
		}
	}

	var cmd tea.Cmd
	switch a.screen {
	case screenList:
		a.list, cmd = a.list.Update(msg)
	case screenDetail:
		a.detail, cmd = a.detail.Update(msg)
	case screenTriage:
		a.triage, cmd = a.triage.Update(msg)
	}
	return a, cmd
}

func (a *App) View() string {
	switch a.screen {
	case screenDetail:
		return a.detail.View()
	case screenTriage:
		return a.triage.View()
	default:
		return a.list.View()
	}
}

func (a *App) loadTickets() {
	tickets, err := a.store.ListTickets(store.TicketFilter{})
	if err != nil {
		return
	}
	a.tickets = tickets
	a.list = newListModel(tickets, a.width, a.height)
}
