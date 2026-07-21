package agent

import (
	"encoding/json"
	"testing"
)

// codexThreadReadFixture is a trimmed thread/read response captured from a
// live codex app-server 0.144.6 (IncludeTurns=true): a turn with a user
// message, a commentary + final agent message, and a command execution.
const codexThreadReadFixture = `{
 "thread": {
  "id": "019f84de-fbda-72f0-bc01-d9be4dfdb235",
  "name": null,
  "preview": "Run exactly this command: ./spike-echo.sh hello",
  "turns": [
   {
    "id": "turn-A",
    "status": "completed",
    "items": [
     {"id": "item-1", "type": "userMessage", "content": [{"type": "text", "text": "Run exactly this command: ./spike-echo.sh hello"}]},
     {"id": "item-2", "type": "agentMessage", "phase": "commentary", "text": "BANANA. I will run it."},
     {"id": "item-3", "type": "commandExecution", "command": "./spike-echo.sh hello", "cwd": "/tmp/work", "status": "completed", "aggregatedOutput": "spike-approved: hello\n", "exitCode": 0},
     {"id": "item-4", "type": "agentMessage", "phase": "final_answer", "text": "BANANA. spike-approved: hello"}
    ]
   },
   {
    "id": "turn-B",
    "status": "completed",
    "items": [
     {"id": "item-1", "type": "userMessage", "content": [{"type": "text", "text": "thanks"}]},
     {"id": "item-2", "type": "reasoning", "summary": [{"text": "quick ack"}]},
     {"id": "item-3", "type": "agentMessage", "phase": "final_answer", "text": "BANANA. You are welcome."}
    ]
   }
  ]
 }
}`

func TestCodexTranscriptMessages(t *testing.T) {
	t.Parallel()
	var view codexThreadView
	if err := json.Unmarshal([]byte(codexThreadReadFixture), &view); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	msgs := codexTranscriptMessages(view)
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4 (user, assistant, user, assistant): %+v", len(msgs), msgs)
	}

	if msgs[0].Role != "user" || msgs[0].Content != "Run exactly this command: ./spike-echo.sh hello" {
		t.Errorf("msg[0] = %+v", msgs[0])
	}

	a1 := msgs[1]
	if a1.Role != "assistant" || a1.ID != "turn-A" || a1.ProviderID != codexProviderOpenAI {
		t.Errorf("assistant identity = %+v, want id=turn-A provider=openai", a1)
	}
	// Final answer wins Content over commentary.
	if a1.Content != "BANANA. spike-approved: hello" {
		t.Errorf("assistant content = %q", a1.Content)
	}
	if len(a1.Parts) != 3 {
		t.Fatalf("assistant parts = %d, want 3 (text, tool, text): %+v", len(a1.Parts), a1.Parts)
	}
	tool := a1.Parts[1]
	if tool.Type != PartToolCall || tool.Tool != codexToolShell || tool.Status != PartCompleted {
		t.Errorf("tool part = %+v", tool)
	}
	if tool.Output != "spike-approved: hello\n" {
		t.Errorf("tool output = %q", tool.Output)
	}

	a2 := msgs[3]
	if a2.ID != "turn-B" {
		t.Errorf("second assistant id = %q, want turn-B", a2.ID)
	}
	if len(a2.Parts) != 2 || a2.Parts[0].Type != PartThinking || a2.Parts[0].Text != "quick ack" {
		t.Errorf("second assistant parts = %+v, want thinking+text", a2.Parts)
	}
}

func TestCodexTranscriptSkipsEmptyTurns(t *testing.T) {
	t.Parallel()
	var view codexThreadView
	fixture := `{"thread":{"id":"t","turns":[{"id":"turn-empty","status":"interrupted","items":[
		{"id":"item-1","type":"userMessage","content":[{"type":"text","text":"go"}]}
	]}]}}`
	if err := json.Unmarshal([]byte(fixture), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := codexTranscriptMessages(view)
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("interrupted-before-output turn: got %+v, want just the user message", msgs)
	}
}
