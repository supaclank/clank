package agent

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/pmenglund/codex-sdk-go/rpc"
)

// codexProviderOpenAI is the provider id stamped on codex assistant messages
// and accepted in ModelOverride.ProviderID.
const codexProviderOpenAI = "openai"

// codexToolShell is the clank-facing tool name for codex command executions.
// codexToolFileChange covers apply-patch file edits.
const (
	codexToolShell       = "shell"
	codexToolFileChange  = "fileChange"
	codexToolWebSearch   = "webSearch"
	codexToolMCP         = "mcpToolCall"
	codexToolPermissions = "permissions"
)

// notificationPump drains the app-server notification stream and maps it to
// clank events. Runs on its own goroutine from Open until Stop (iterator
// errors out on ctx cancel / process exit).
func (b *CodexBackend) notificationPump(sub *rpc.NotificationIterator) {
	defer sub.Close()
	for {
		note, err := sub.Next(b.ctx)
		if err != nil {
			b.mu.Lock()
			stopped := b.stopped
			b.mu.Unlock()
			if !stopped {
				// Unexpected pump end: the subprocess died or the stream broke.
				b.setStatus(StatusDead)
			}
			return
		}
		b.handleNotification(note)
	}
}

// codexNotePayload is the superset of notification payload fields the mapping
// reads. Individual notifications populate only their relevant subset.
type codexNotePayload struct {
	ThreadID string          `json:"threadId"`
	ItemID   string          `json:"itemId"`
	Delta    string          `json:"delta"`
	Item     json.RawMessage `json:"item"`
	Turn     struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"turn"`
	Message string `json:"message"`
}

// codexItem is the subset of a thread item the mapping and transcript
// conversion read. Type-specific fields are populated per item type.
type codexItem struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Text    string `json:"text"`  // agentMessage
	Phase   string `json:"phase"` // agentMessage: "commentary" | "final_answer"
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"` // userMessage
	Command          string          `json:"command"` // commandExecution
	Cwd              string          `json:"cwd"`
	AggregatedOutput string          `json:"aggregatedOutput"`
	ExitCode         *int            `json:"exitCode"`
	Status           string          `json:"status"`
	Changes          json.RawMessage `json:"changes"` // fileChange
	Query            string          `json:"query"`   // webSearch
	Server           string          `json:"server"`  // mcpToolCall
	Tool             string          `json:"tool"`
	Summary          []struct {
		Text string `json:"text"`
	} `json:"summary"` // reasoning
}

// handleNotification maps one app-server notification onto clank's event
// model. Notifications tagged with a different threadId are ignored (a fork
// briefly creates a second thread on this process; its session gets its own
// backend). Unknown methods are ignored for forward compatibility with
// codex's release cadence.
func (b *CodexBackend) handleNotification(note rpc.Notification) {
	var p codexNotePayload
	if len(note.Raw) > 0 {
		if err := json.Unmarshal(note.Raw, &p); err != nil {
			return
		}
	}
	b.mu.Lock()
	ourThread := b.threadID
	b.mu.Unlock()
	if p.ThreadID != "" && ourThread != "" && p.ThreadID != ourThread {
		return
	}

	switch note.Method {
	case "turn/started":
		b.mu.Lock()
		b.activeTurnID = p.Turn.ID
		b.currentMsgID = p.Turn.ID
		b.mu.Unlock()
		b.setStatus(StatusBusy)
		// Assistant message shell for the turn; parts attach to it by id.
		// The id is the turn id, which thread/read also keys assistant
		// messages by, so history refetches reconcile with the stream.
		b.emit(Event{
			Type:      EventMessage,
			Timestamp: time.Now(),
			Data: MessageData{
				ID:         p.Turn.ID,
				Role:       "assistant",
				ProviderID: codexProviderOpenAI,
			},
		})

	case "turn/completed":
		b.mu.Lock()
		b.activeTurnID = ""
		b.currentMsgID = ""
		b.mu.Unlock()
		if p.Turn.Status == "failed" {
			msg := "codex turn failed"
			if p.Turn.Error != nil && p.Turn.Error.Message != "" {
				msg = p.Turn.Error.Message
			}
			b.emit(Event{Type: EventError, Timestamp: time.Now(), Data: ErrorData{Message: msg}})
			b.setStatus(StatusError)
			return
		}
		// "completed" and "interrupted" both land on idle.
		b.setStatus(StatusIdle)

	case "turn/failed":
		b.mu.Lock()
		b.activeTurnID = ""
		b.currentMsgID = ""
		b.mu.Unlock()
		msg := "codex turn failed"
		if p.Turn.Error != nil && p.Turn.Error.Message != "" {
			msg = p.Turn.Error.Message
		}
		b.emit(Event{Type: EventError, Timestamp: time.Now(), Data: ErrorData{Message: msg}})
		b.setStatus(StatusError)

	case "item/started", "item/completed":
		var item codexItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return
		}
		b.handleItemUpdate(item, note.Method == "item/completed")

	case "item/agentMessage/delta":
		b.emitPartDelta(p.ItemID, PartText, p.Delta)

	case "item/reasoning/textDelta", "item/reasoning/summaryTextDelta":
		b.emitPartDelta(p.ItemID, PartThinking, p.Delta)

	case "error":
		if p.Message != "" {
			b.emit(Event{Type: EventError, Timestamp: time.Now(), Data: ErrorData{Message: p.Message}})
		}
	}
}

// emitPartDelta streams an incremental text/thinking chunk for a part.
func (b *CodexBackend) emitPartDelta(itemID string, typ PartType, delta string) {
	if delta == "" {
		return
	}
	b.mu.Lock()
	msgID := b.currentMsgID
	b.mu.Unlock()
	b.emit(Event{
		Type:      EventPartUpdate,
		Timestamp: time.Now(),
		Data: PartUpdateData{
			MessageID: msgID,
			Part:      Part{ID: itemID, Type: typ, Text: delta},
			IsDelta:   true,
		},
	})
}

// handleItemUpdate emits part/message events for an item lifecycle change.
func (b *CodexBackend) handleItemUpdate(item codexItem, completed bool) {
	b.mu.Lock()
	msgID := b.currentMsgID
	b.mu.Unlock()

	switch item.Type {
	case "userMessage":
		if !completed {
			return
		}
		b.emit(Event{
			Type:      EventMessage,
			Timestamp: time.Now(),
			Data:      MessageData{ID: item.ID, Role: "user", Content: codexUserText(item)},
		})

	case "agentMessage":
		if !completed {
			return
		}
		// Full snapshot replaces the accumulated deltas.
		b.emit(Event{
			Type:      EventPartUpdate,
			Timestamp: time.Now(),
			Data: PartUpdateData{
				MessageID: msgID,
				Part:      Part{ID: item.ID, Type: PartText, Text: item.Text},
			},
		})

	case "reasoning":
		if !completed {
			return
		}
		if text := codexReasoningText(item); text != "" {
			b.emit(Event{
				Type:      EventPartUpdate,
				Timestamp: time.Now(),
				Data: PartUpdateData{
					MessageID: msgID,
					Part:      Part{ID: item.ID, Type: PartThinking, Text: text},
				},
			})
		}

	default:
		part, ok := codexToolPart(item, completed)
		if !ok {
			return
		}
		b.emit(Event{
			Type:      EventPartUpdate,
			Timestamp: time.Now(),
			Data:      PartUpdateData{MessageID: msgID, Part: part},
		})
	}
}

// codexToolPart converts a tool-shaped item (commandExecution, fileChange,
// webSearch, mcpToolCall) into a clank tool-call Part. Returns ok=false for
// item types that don't render as tool cards (todoList, contextCompaction…).
func codexToolPart(item codexItem, completed bool) (Part, bool) {
	part := Part{
		ID:     item.ID,
		Type:   PartToolCall,
		Status: PartRunning,
	}
	switch item.Type {
	case "commandExecution":
		part.Tool = codexToolShell
		part.Input = map[string]any{"command": item.Command, "cwd": item.Cwd}
		if completed {
			part.Output = item.AggregatedOutput
		}
	case "fileChange":
		part.Tool = codexToolFileChange
		input := map[string]any{}
		if len(item.Changes) > 0 {
			var changes any
			if err := json.Unmarshal(item.Changes, &changes); err == nil {
				input["changes"] = changes
			}
		}
		part.Input = input
	case "webSearch":
		part.Tool = codexToolWebSearch
		part.Input = map[string]any{"query": item.Query}
	case "mcpToolCall":
		part.Tool = codexToolMCP
		part.Input = map[string]any{"server": item.Server, "tool": item.Tool}
	default:
		return Part{}, false
	}
	if completed {
		part.Status = PartCompleted
		if item.Status == "failed" || (item.ExitCode != nil && *item.ExitCode != 0) {
			part.Status = PartFailed
		}
	}
	return part, true
}

// codexUserText joins a userMessage item's text content blocks.
func codexUserText(item codexItem) string {
	var out strings.Builder
	for _, c := range item.Content {
		if c.Type == "text" {
			out.WriteString(c.Text)
		}
	}
	return out.String()
}

// codexReasoningText joins a reasoning item's summary blocks (content is
// typically encrypted/absent in persisted threads; summaries are what codex
// exposes).
func codexReasoningText(item codexItem) string {
	var out strings.Builder
	for _, s := range item.Summary {
		if s.Text == "" {
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(s.Text)
	}
	return out.String()
}
