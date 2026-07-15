package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	opencode "github.com/acksell/opencode-go-sdk/sdk"
)

// OpenCodeToolQuestion is OpenCode's interactive question tool name.
const OpenCodeToolQuestion = "question"

// handleQuestionAsked captures a question.asked event: it normalizes the
// prompt, keys it by the tool call id so convertSDKPart tags the tool part
// (Part.Question) on live updates and Messages() reloads alike, and — when
// the part already streamed past untagged — re-emits it with the tag.
// OpenCode's question API is already structured, so this is a straight field
// mapping; no permission event is involved.
func (b *OpenCodeBackend) handleQuestionAsked(props *opencode.GlobalEventPayloadQuestionAskedProperties) {
	if props == nil || props.SessionID != b.SessionID() || props.Tool == nil {
		return
	}
	questions := make([]Question, 0, len(props.Questions))
	for _, qi := range props.Questions {
		if qi == nil || qi.Question == "" {
			continue
		}
		q := Question{
			Text:        qi.Question,
			Header:      questionHeaderOrDefault(qi.Header, qi.Question),
			MultiSelect: qi.Multiple != nil && *qi.Multiple,
			// Missing Custom means the field is absent from OpenCode's
			// payload (older fixtures); default to allowed, matching what
			// clients already assume for a missing allow_custom.
			AllowCustom: qi.Custom, // pass-through; nil = unspecified = allowed
		}
		for _, opt := range qi.Options {
			if opt == nil || opt.Label == "" {
				continue
			}
			q.Options = append(q.Options, QuestionOption{Label: opt.Label, Description: opt.Description})
		}
		if len(q.Options) == 0 {
			continue
		}
		questions = append(questions, q)
	}
	b.registerQuestionPrompt(props.Tool.CallID, props.ID, questions)
}

// handleQuestionV2Asked is the question.v2.asked variant of
// handleQuestionAsked. The payload is structurally identical but uses
// distinct SDK types, hence the parallel mapping.
func (b *OpenCodeBackend) handleQuestionV2Asked(props *opencode.GlobalEventPayloadQuestionV2AskedProperties) {
	if props == nil || props.SessionID != b.SessionID() || props.Tool == nil {
		return
	}
	questions := make([]Question, 0, len(props.Questions))
	for _, qi := range props.Questions {
		if qi == nil || qi.Question == "" {
			continue
		}
		q := Question{
			Text:        qi.Question,
			Header:      questionHeaderOrDefault(qi.Header, qi.Question),
			MultiSelect: qi.Multiple != nil && *qi.Multiple,
			AllowCustom: qi.Custom, // pass-through; nil = unspecified = allowed
		}
		for _, opt := range qi.Options {
			if opt == nil || opt.Label == "" {
				continue
			}
			q.Options = append(q.Options, QuestionOption{Label: opt.Label, Description: opt.Description})
		}
		if len(q.Options) == 0 {
			continue
		}
		questions = append(questions, q)
	}
	b.registerQuestionPrompt(props.Tool.CallID, props.ID, questions)
}

func (b *OpenCodeBackend) registerQuestionPrompt(callID, requestID string, questions []Question) {
	if callID == "" || requestID == "" || len(questions) == 0 {
		return
	}
	prompt := &QuestionPrompt{RequestID: requestID, Questions: questions}

	b.mu.Lock()
	b.questionPrompts[callID] = prompt
	cached, hasPart := b.questionToolParts[callID]
	if hasPart {
		delete(b.questionToolParts, callID) // consumed; avoid unbounded growth
	}
	b.mu.Unlock()

	// question.asked and the tool's part update race on separate streams: if
	// the part already went out untagged, re-emit it with the tag; otherwise
	// convertSDKPart tags it on arrival via questionPrompts.
	if hasPart {
		cached.Question = prompt
		b.emit(Event{
			Type:      EventPartUpdate,
			Timestamp: time.Now(),
			Data:      PartUpdateData{Part: cached},
		})
	}
}

// tagQuestionToolPart stamps a converted tool part with its question prompt
// when one is registered for its call id, and caches question-tool parts so
// a later question.asked can re-emit them tagged (see registerQuestionPrompt).
func (b *OpenCodeBackend) tagQuestionToolPart(callID, tool string, part *Part) {
	if part == nil || callID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if prompt := b.questionPrompts[callID]; prompt != nil {
		part.Question = prompt
		return
	}
	if tool == OpenCodeToolQuestion {
		b.questionToolParts[callID] = *part
	}
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
