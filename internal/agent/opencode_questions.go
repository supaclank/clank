package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	opencode "github.com/acksell/opencode-go-sdk/sdk"
)

// handleQuestionAsked emits a normalized EventQuestion from a question.asked
// global event. OpenCode's question API is already structured, so this is a
// straight field mapping (no permission event is involved — OpenCode gates
// questions on its own question endpoints, not the permission ones).
func (b *OpenCodeBackend) handleQuestionAsked(props *opencode.GlobalEventPayloadQuestionAskedProperties) {
	if props == nil || props.SessionID != b.SessionID() {
		return
	}
	questions := make([]Question, 0, len(props.Questions))
	for _, qi := range props.Questions {
		if qi == nil {
			continue
		}
		q := Question{
			Text:        qi.Question,
			Header:      qi.Header,
			MultiSelect: qi.Multiple != nil && *qi.Multiple,
			AllowCustom: qi.Custom != nil && *qi.Custom,
		}
		for _, opt := range qi.Options {
			if opt == nil || opt.Label == "" {
				continue
			}
			q.Options = append(q.Options, QuestionOption{Label: opt.Label, Description: opt.Description})
		}
		questions = append(questions, q)
	}
	toolUseID := ""
	if props.Tool != nil {
		toolUseID = props.Tool.CallID
	}
	b.emitQuestion(props.ID, toolUseID, questions)
}

// handleQuestionV2Asked is the question.v2.asked variant of
// handleQuestionAsked. The payload is structurally identical but uses
// distinct SDK types, hence the parallel mapping.
func (b *OpenCodeBackend) handleQuestionV2Asked(props *opencode.GlobalEventPayloadQuestionV2AskedProperties) {
	if props == nil || props.SessionID != b.SessionID() {
		return
	}
	questions := make([]Question, 0, len(props.Questions))
	for _, qi := range props.Questions {
		if qi == nil {
			continue
		}
		q := Question{
			Text:        qi.Question,
			Header:      qi.Header,
			MultiSelect: qi.Multiple != nil && *qi.Multiple,
			AllowCustom: qi.Custom != nil && *qi.Custom,
		}
		for _, opt := range qi.Options {
			if opt == nil || opt.Label == "" {
				continue
			}
			q.Options = append(q.Options, QuestionOption{Label: opt.Label, Description: opt.Description})
		}
		questions = append(questions, q)
	}
	toolUseID := ""
	if props.Tool != nil {
		toolUseID = props.Tool.CallID
	}
	b.emitQuestion(props.ID, toolUseID, questions)
}

func (b *OpenCodeBackend) emitQuestion(requestID, toolUseID string, questions []Question) {
	if requestID == "" || len(questions) == 0 {
		return
	}
	b.emit(Event{
		Type:      EventQuestion,
		Timestamp: time.Now(),
		Data: QuestionData{
			RequestID: requestID,
			ToolUseID: toolUseID,
			Questions: questions,
		},
	})
}

// handleQuestionResolved relays question.replied / question.rejected so every
// client clears the prompt, including ones that didn't answer it.
func (b *OpenCodeBackend) handleQuestionResolved(sessionID, requestID string) {
	if sessionID != b.SessionID() || requestID == "" {
		return
	}
	b.emit(Event{
		Type:      EventQuestionResolved,
		Timestamp: time.Now(),
		Data:      QuestionResolvedData{RequestID: requestID},
	})
}

// RespondQuestion replies to a pending OpenCode question via its structured
// question API. A custom free-text answer is delivered as an extra label —
// OpenCode's reply schema is an array of selected labels per question.
func (b *OpenCodeBackend) RespondQuestion(ctx context.Context, requestID string, answers []QuestionAnswer, reject bool) error {
	if b.SessionID() == "" {
		return fmt.Errorf("session not started")
	}

	if reject {
		req := &opencode.QuestionRejectRequest{RequestID: requestID}
		_, err := b.client.Question.Reject(ctx, req)
		if err != nil && isConnectionError(err) && b.resolver != nil {
			if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
				_, err = b.client.Question.Reject(ctx, req)
			}
		}
		if err != nil {
			return fmt.Errorf("reject question: %w", err)
		}
		return nil
	}

	converted := make([]opencode.QuestionAnswer, len(answers))
	for i, a := range answers {
		labels := append([]string(nil), a.Selected...)
		if custom := strings.TrimSpace(a.Custom); custom != "" {
			labels = append(labels, custom)
		}
		if labels == nil {
			labels = []string{}
		}
		converted[i] = labels
	}
	req := &opencode.QuestionReplyRequest{RequestID: requestID, Answers: converted}
	_, err := b.client.Question.Reply(ctx, req)
	if err != nil && isConnectionError(err) && b.resolver != nil {
		if _, resolveErr := b.refreshServerURL(); resolveErr == nil {
			_, err = b.client.Question.Reply(ctx, req)
		}
	}
	if err != nil {
		return fmt.Errorf("reply question: %w", err)
	}
	return nil
}
