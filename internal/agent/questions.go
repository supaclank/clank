package agent

import (
	"fmt"
	"strings"
)

// Claude Code tool names for stop-and-wait interactive tools. These are the
// only provider tool names clank special-cases; everything else flows through
// the generic permission path.
const (
	ClaudeToolAskUserQuestion = "AskUserQuestion"
	ClaudeToolExitPlanMode    = "ExitPlanMode"
)

// questionsDelegatedMessage is sent when the user answered no question at all,
// delegating every decision back to the agent. Mirrors the mobile client's
// template so the model sees identical phrasing regardless of client.
const questionsDelegatedMessage = "Delegating these to you — use your best judgment."

// questionsDismissedMessage is the deny reason when the user dismisses a
// question prompt without answering.
const questionsDismissedMessage = "User dismissed the questions — proceed using your best judgment."

// bypassQuestionIDPrefix + tool_use id forms the request id for a question
// whose tool auto-ran (bypassPermissions). Deterministic so the id is
// recoverable from the transcript alone (see questionFromTranscript).
const bypassQuestionIDPrefix = "q-"

// tagQuestionPart builds the QuestionPrompt tag for an AskUserQuestion tool
// part, addressed by the deterministic bypass request id. Returns nil for
// other tools or unparseable input (the part stays a generic tool card).
func tagQuestionPart(toolUseID, tool string, input map[string]any) *QuestionPrompt {
	if tool != ClaudeToolAskUserQuestion || toolUseID == "" {
		return nil
	}
	questions := parseClaudeQuestions(input)
	if questions == nil {
		return nil
	}
	return &QuestionPrompt{RequestID: bypassQuestionIDPrefix + toolUseID, Questions: questions}
}

// questionHeaderOrDefault returns header, or — when empty — the first 12
// characters of text (mirrors the mobile client's default).
func questionHeaderOrDefault(header, text string) string {
	if header != "" {
		return header
	}
	r := []rune(text)
	if len(r) > 12 {
		r = r[:12]
	}
	return string(r)
}

// parseClaudeQuestions normalizes an AskUserQuestion tool input into clank
// Questions. Returns nil for any unexpected shape (callers then fall back to
// the generic permission flow instead of rendering a broken card). Claude's
// tool always accepts a free-text "Other" answer, so AllowCustom is true.
func parseClaudeQuestions(input map[string]any) []Question {
	raw, ok := input["questions"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make([]Question, 0, len(raw))
	for _, rq := range raw {
		qm, ok := rq.(map[string]any)
		if !ok {
			return nil
		}
		text, _ := qm["question"].(string)
		if text == "" {
			return nil
		}
		header, _ := qm["header"].(string)
		header = questionHeaderOrDefault(header, text)
		multi, _ := qm["multiSelect"].(bool)
		rawOpts, _ := qm["options"].([]any)
		opts := make([]QuestionOption, 0, len(rawOpts))
		for _, ro := range rawOpts {
			om, ok := ro.(map[string]any)
			if !ok {
				continue
			}
			label, _ := om["label"].(string)
			if label == "" {
				continue
			}
			desc, _ := om["description"].(string)
			opts = append(opts, QuestionOption{Label: label, Description: desc})
		}
		if len(opts) == 0 {
			return nil
		}
		out = append(out, Question{
			Text:        text,
			Header:      header,
			MultiSelect: multi,
			AllowCustom: true,
			Options:     opts,
		})
	}
	return out
}

// formatQuestionAnswers renders structured answers as the message the model
// reads. The template is byte-identical to the mobile client's formatAnswers
// so model behavior doesn't depend on which client answered.
func formatQuestionAnswers(questions []Question, answers []QuestionAnswer) string {
	rendered := make([]string, len(questions))
	allEmpty := true
	for i := range questions {
		var a QuestionAnswer
		if i < len(answers) {
			a = answers[i]
		}
		answer := strings.Join(a.Selected, ", ")
		if custom := strings.TrimSpace(a.Custom); custom != "" {
			if answer != "" {
				answer += "  (Other: " + custom + ")"
			} else {
				answer = "Other: " + custom
			}
		}
		if answer != "" {
			allEmpty = false
		}
		rendered[i] = answer
	}
	if allEmpty {
		return questionsDelegatedMessage
	}
	lines := []string{"Answers to your questions:"}
	for i, q := range questions {
		a := rendered[i]
		if a == "" {
			a = "(delegated to you)"
		}
		lines = append(lines, fmt.Sprintf("**%s**: %s", q.Header, a))
	}
	return strings.Join(lines, "\n")
}
