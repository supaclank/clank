package analyzer

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/acksell/clank/internal/llm"
	"github.com/acksell/clank/internal/scanner"
	"github.com/acksell/clank/internal/store"
)

type Analyzer struct {
	client *llm.Client
}

func New(client *llm.Client) *Analyzer {
	return &Analyzer{client: client}
}

type extractedTicket struct {
	Type         string   `json:"type"`
	Title        string   `json:"title"`
	Summary      string   `json:"summary"`
	Description  string   `json:"description"`
	SourceQuotes []string `json:"source_quotes"`
	Complexity   int      `json:"complexity_score_1_to_5"`
	Impact       int      `json:"impact_score_1_to_5"`
	Labels       []string `json:"labels"`
}

type extractionResult struct {
	Candidates []extractedTicket `json:"candidates"`
}

func (a *Analyzer) Analyze(session scanner.RawSession, centralContext string) ([]store.Ticket, error) {
	transcript := formatSession(session)
	if len(transcript) < 50 {
		return nil, nil
	}

	const maxChars = 80000
	if len(transcript) > maxChars {
		transcript = transcript[:maxChars] + "\n\n[...truncated]"
	}

	prompt := buildPrompt(transcript, centralContext)

	resp, err := a.client.ChatCompletion([]llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return nil, fmt.Errorf("LLM analysis: %w", err)
	}

	return parseResponse(resp, session)
}

func (a *Analyzer) TriageTicket(ticket store.Ticket, centralContext string) (string, error) {
	prompt := fmt.Sprintf(`Here is a ticket extracted from a coding session. Please suggest:
1. Whether this should be kept (backlog) or discarded
2. Suggested labels (from: bug, feature, refactor, test, documentation, research, security)
3. Complexity score (1-5, where 1=trivial, 5=major effort)
4. Impact score (1-5, where 1=trivial/cosmetic, 5=critical/blocking)
5. Suggested next steps or action items
6. Any connections to the product context provided

Ticket:
- Title: %s
- Type: %s
- Summary: %s
- Description: %s
- Source quotes:
  - %s

Product context:
%s

Respond with a concise, actionable analysis.`,
		ticket.Title, ticket.Type, ticket.Summary, ticket.Description,
		strings.Join(ticket.SourceQuotes, "\n  - "), centralContext)

	return a.client.ChatCompletion([]llm.Message{
		{Role: "system", Content: "You are a senior engineering manager helping triage a developer's backlog. Be concise and actionable."},
		{Role: "user", Content: prompt},
	})
}

func formatSession(s scanner.RawSession) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Session: %s\nTitle: %s\nDirectory: %s\nDate: %s\n\n",
		s.ID, s.Title, s.Directory, s.CreatedAt.Format("2006-01-02 15:04"))

	for _, msg := range s.Messages {
		fmt.Fprintf(&sb, "--- %s", msg.Role)
		if msg.Mode != "" {
			fmt.Fprintf(&sb, " [%s]", msg.Mode)
		}
		sb.WriteString(" ---\n")

		for _, part := range msg.Parts {
			switch part.Type {
			case "text":
				sb.WriteString(part.Text)
				sb.WriteString("\n")
			case "tool":
				fmt.Fprintf(&sb, "[tool call]\n")
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

const systemPrompt = `You are an expert at analyzing coding session transcripts to find:

1. "Unfinished threads": Plans or tasks that were discussed but never executed. Look for:
   - Sessions ending with plan-mode responses but no code changes
   - Tasks mentioned as "TODO" or "next steps" that were never completed
   - Ideas that were explored but abandoned

2. "Opportunities": Improvement suggestions or next-step ideas, often found as bullet points at the end of assistant messages.

Return a JSON object with this exact structure:
{
  "candidates": [
    {
      "type": "unfinished_thread" or "opportunity",
      "title": "Short descriptive title",
      "summary": "One or two sentence summary",
      "description": "Detailed description with context",
      "source_quotes": ["Relevant quotes from the session"],
      "complexity_score_1_to_5": 3,
      "impact_score_1_to_5": 3,
      "labels": ["feature", "refactor", etc.]
    }
  ]
}

Complexity measures implementation effort: 1=trivial (minutes), 5=major (days/weeks of work).
Impact measures how much value fixing/implementing this would deliver to the user or product:
  1=trivial/cosmetic, 2=minor convenience, 3=meaningful improvement, 4=significant value, 5=critical/blocking.

Labels must be from: bug, feature, refactor, test, documentation, research, security.
Only include genuinely useful items. Skip trivial or already-completed tasks.
If there are no candidates, return {"candidates": []}.
Return ONLY the JSON, no markdown fences or extra text.`

func buildPrompt(transcript, centralContext string) string {
	var sb strings.Builder
	if centralContext != "" {
		sb.WriteString("Product/company context for reference:\n")
		sb.WriteString(centralContext)
		sb.WriteString("\n\n---\n\n")
	}
	sb.WriteString("Analyze this coding session transcript and extract unfinished threads and opportunities:\n\n")
	sb.WriteString(transcript)
	return sb.String()
}

func parseResponse(resp string, session scanner.RawSession) ([]store.Ticket, error) {
	resp = strings.TrimSpace(resp)
	resp = strings.TrimPrefix(resp, "```json")
	resp = strings.TrimPrefix(resp, "```")
	resp = strings.TrimSuffix(resp, "```")
	resp = strings.TrimSpace(resp)

	var result extractionResult
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		preview := resp
		if len(preview) > 500 {
			preview = preview[:500]
		}
		return nil, fmt.Errorf("parse LLM response: %w\nResponse was: %s", err, preview)
	}

	var tickets []store.Ticket
	for _, c := range result.Candidates {
		t := store.Ticket{
			Type:         store.TicketType(c.Type),
			Status:       store.StatusNew,
			Title:        c.Title,
			Summary:      c.Summary,
			Description:  c.Description,
			RepoPath:     session.Directory,
			SessionID:    session.ID,
			SessionTitle: session.Title,
			SessionDate:  session.CreatedAt,
			SourceQuotes: c.SourceQuotes,
			Labels:       c.Labels,
			Complexity:   c.Complexity,
			Impact:       c.Impact,
		}
		tickets = append(tickets, t)
	}
	return tickets, nil
}

type impactResult struct {
	Impact int `json:"impact_score_1_to_5"`
}

// ScoreImpact asks the LLM to score just the impact of a ticket.
// Used for backfilling existing tickets that have no impact score.
func (a *Analyzer) ScoreImpact(ticket store.Ticket, centralContext string) (int, error) {
	prompt := fmt.Sprintf(`Score the impact of this ticket on a scale of 1-5.

Impact measures how much value fixing/implementing this would deliver to the user or product:
  1=trivial/cosmetic, 2=minor convenience, 3=meaningful improvement, 4=significant value, 5=critical/blocking.

Ticket:
- Title: %s
- Summary: %s
- Description: %s
- Labels: %s

Product context:
%s

Return ONLY a JSON object: {"impact_score_1_to_5": N}`,
		ticket.Title, ticket.Summary, ticket.Description,
		strings.Join(ticket.Labels, ", "), centralContext)

	resp, err := a.client.ChatCompletion([]llm.Message{
		{Role: "system", Content: "You are a senior engineering manager scoring ticket impact. Return only JSON."},
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return 0, fmt.Errorf("LLM impact scoring: %w", err)
	}

	resp = strings.TrimSpace(resp)
	resp = strings.TrimPrefix(resp, "```json")
	resp = strings.TrimPrefix(resp, "```")
	resp = strings.TrimSuffix(resp, "```")
	resp = strings.TrimSpace(resp)

	var result impactResult
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		return 0, fmt.Errorf("parse impact response: %w", err)
	}
	if result.Impact < 1 || result.Impact > 5 {
		return 0, fmt.Errorf("impact score out of range: %d", result.Impact)
	}
	return result.Impact, nil
}
