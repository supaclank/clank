package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host/store/hostsqlitedb"
)

// ErrSessionNotFound is returned by GetSession when no session
// matches.
var ErrSessionNotFound = errors.New("session not found")

// GetSession returns the persisted session by its daemon ID, or
// ErrSessionNotFound.
func (s *Store) GetSession(ctx context.Context, id string) (agent.SessionInfo, error) {
	row, err := s.q.GetSession(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return agent.SessionInfo{}, ErrSessionNotFound
	}
	if err != nil {
		return agent.SessionInfo{}, fmt.Errorf("get session %s: %w", id, err)
	}
	return sessionFromRow(row), nil
}

// FindSessionByExternalID returns the session whose backend ID
// matches externalID, or ErrSessionNotFound. Used by the discovery
// flow to dedupe historical sessions.
func (s *Store) FindSessionByExternalID(ctx context.Context, externalID string) (agent.SessionInfo, error) {
	row, err := s.q.FindSessionByExternalID(ctx, externalID)
	if errors.Is(err, sql.ErrNoRows) {
		return agent.SessionInfo{}, ErrSessionNotFound
	}
	if err != nil {
		return agent.SessionInfo{}, fmt.Errorf("find by external id %s: %w", externalID, err)
	}
	return sessionFromRow(row), nil
}

// ListSessions returns every persisted session, newest-updated first.
func (s *Store) ListSessions(ctx context.Context) ([]agent.SessionInfo, error) {
	rows, err := s.q.ListSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	out := make([]agent.SessionInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, sessionFromRow(r))
	}
	return out, nil
}

// ListSessionsByWorktree returns every persisted session for the given
// worktree ID, newest-updated first. Empty worktreeID returns an empty
// slice (rather than every session in the DB) — callers operating
// without a worktree context should use ListSessions instead.
func (s *Store) ListSessionsByWorktree(ctx context.Context, worktreeID string) ([]agent.SessionInfo, error) {
	if worktreeID == "" {
		return []agent.SessionInfo{}, nil
	}
	rows, err := s.q.ListSessionsByWorktree(ctx, worktreeID)
	if err != nil {
		return nil, fmt.Errorf("list sessions by worktree %s: %w", worktreeID, err)
	}
	out := make([]agent.SessionInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, sessionFromRow(r))
	}
	return out, nil
}

// SearchParams mirrors the hub-side SearchParams (kept on agent.).
// Empty Q / Visibility means no filter for that field. Zero Since
// or Until means unbounded. Limit ≤ 0 means a default cap.
type SearchParams struct {
	Q          string
	Visibility agent.SessionVisibility
	Since      time.Time
	Until      time.Time
	Limit      int
}

// SearchSessions returns sessions matching the filters.
func (s *Store) SearchSessions(ctx context.Context, p SearchParams) ([]agent.SessionInfo, error) {
	limit := int64(p.Limit)
	if limit <= 0 {
		limit = 500
	}
	args := hostsqlitedb.SearchSessionsParams{
		Q:          p.Q,
		Visibility: string(p.Visibility),
		Lim:        limit,
	}
	// Since/Until columns are unix millis post-v3 migration; sqlc
	// types them as interface{} (because the SQL is "@since IS NULL
	// OR …" which sqlc can't infer). Pass int64 when present, nil
	// otherwise so the NULL-check short-circuits inside SQLite.
	if !p.Since.IsZero() {
		args.Since = p.Since.UnixMilli()
	}
	if !p.Until.IsZero() {
		args.Until = p.Until.UnixMilli()
	}
	rows, err := s.q.SearchSessions(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("search sessions: %w", err)
	}
	out := make([]agent.SessionInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, sessionFromRow(r))
	}
	return out, nil
}

// UpsertSession inserts or replaces the session row by ID.
func (s *Store) UpsertSession(ctx context.Context, info agent.SessionInfo) error {
	if info.ID == "" {
		return fmt.Errorf("upsert session: ID is required")
	}
	now := info.UpdatedAt
	if now.IsZero() {
		now = time.Now()
	}
	createdAt := info.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	followUp := int64(0)
	if info.FollowUp {
		followUp = 1
	}
	var lastReadAt sql.NullInt64
	if !info.LastReadAt.IsZero() {
		lastReadAt = sql.NullInt64{Int64: timeToMs(info.LastReadAt), Valid: true}
	}
	return s.q.UpsertSession(ctx, hostsqlitedb.UpsertSessionParams{
		ID:             info.ID,
		ExternalID:     info.ExternalID,
		Backend:        string(info.Backend),
		Status:         string(info.Status),
		Visibility:     string(info.Visibility),
		FollowUp:       followUp,
		ProjectDir:     info.GitRef.LocalPath,
		WorktreeID:     info.GitRef.WorktreeID,
		WorktreeBranch: info.GitRef.WorktreeBranch,
		Prompt:         info.Prompt,
		Title:          info.Title,
		TicketID:       info.TicketID,
		Agent:          info.Agent,
		Draft:          info.Draft,
		CreatedAt:      timeToMs(createdAt),
		UpdatedAt:      timeToMs(now),
		LastReadAt:     lastReadAt,
	})
}

// DeleteSession removes a session by ID. No-op if it doesn't exist.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	if err := s.q.DeleteSession(ctx, id); err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	return nil
}

// LoadPrimaryAgents returns the cached agent list for (backend, ref),
// or nil + nil if no entry exists.
func (s *Store) LoadPrimaryAgents(ctx context.Context, backend agent.BackendType, ref agent.GitRef) ([]agent.AgentInfo, error) {
	jsonBytes, err := s.q.ListPrimaryAgents(ctx, hostsqlitedb.ListPrimaryAgentsParams{
		Backend:    string(backend),
		ProjectDir: ref.LocalPath,
		WorktreeID:     ref.WorktreeID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load primary agents: %w", err)
	}
	if jsonBytes == "" {
		return nil, nil
	}
	var agents []agent.AgentInfo
	if err := json.Unmarshal([]byte(jsonBytes), &agents); err != nil {
		return nil, fmt.Errorf("decode primary agents: %w", err)
	}
	return agents, nil
}

// UpsertPrimaryAgents stores the agent list for (backend, ref).
func (s *Store) UpsertPrimaryAgents(ctx context.Context, backend agent.BackendType, ref agent.GitRef, agents []agent.AgentInfo) error {
	if ref.LocalPath == "" && ref.WorktreeID == "" {
		return fmt.Errorf("upsert primary agents: git ref required")
	}
	data, err := json.Marshal(agents)
	if err != nil {
		return fmt.Errorf("encode primary agents: %w", err)
	}
	return s.q.UpsertPrimaryAgents(ctx, hostsqlitedb.UpsertPrimaryAgentsParams{
		Backend:           string(backend),
		ProjectDir:        ref.LocalPath,
		WorktreeID:        ref.WorktreeID,
		PrimaryAgentsJson: string(data),
		UpdatedAt:         time.Now().UnixMilli(),
	})
}

func sessionFromRow(r hostsqlitedb.Session) agent.SessionInfo {
	info := agent.SessionInfo{
		ID:         r.ID,
		ExternalID: r.ExternalID,
		Backend:    agent.BackendType(r.Backend),
		Status:     agent.SessionStatus(r.Status),
		Visibility: agent.SessionVisibility(r.Visibility),
		FollowUp:   r.FollowUp != 0,
		GitRef: agent.GitRef{
			LocalPath:      r.ProjectDir,
			WorktreeID:     r.WorktreeID,
			WorktreeBranch: r.WorktreeBranch,
		},
		Prompt:    r.Prompt,
		Title:     r.Title,
		TicketID:  r.TicketID,
		Agent:     r.Agent,
		Draft:     r.Draft,
		CreatedAt: msToTime(r.CreatedAt),
		UpdatedAt: msToTime(r.UpdatedAt),
	}
	if r.LastReadAt.Valid {
		info.LastReadAt = msToTime(r.LastReadAt.Int64)
	}
	return info
}
