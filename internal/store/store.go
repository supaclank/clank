package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite"
)

// SessionState represents the lifecycle state of a coding session.
type SessionState string

const (
	SessionBusy     SessionState = "busy"     // Agent is actively working (set by plugin)
	SessionIdle     SessionState = "idle"     // Agent finished, awaiting human review (set by plugin)
	SessionError    SessionState = "error"    // Session errored (set by plugin)
	SessionApproved SessionState = "approved" // User tested/QA'd, confirmed good
	SessionArchived SessionState = "archived" // User dismissed without testing
	SessionFollowup SessionState = "followup" // Needs more work, will revisit
)

// SessionStatus tracks a coding session's lifecycle state in Clank.
type SessionStatus struct {
	SessionID string       `json:"session_id"`
	Status    SessionState `json:"status"`
	Source    string       `json:"source"` // "opencode", "claude-code", etc.
	Unread    bool         `json:"unread"` // true if agent responded and user hasn't opened it
	UpdatedAt time.Time    `json:"updated_at"`
}

type TicketType string

const (
	TicketTypeUnfinishedThread TicketType = "unfinished_thread"
	TicketTypeOpportunity      TicketType = "opportunity"
)

type TicketStatus string

const (
	StatusNew       TicketStatus = "new"
	StatusTriaged   TicketStatus = "triaged"
	StatusBacklog   TicketStatus = "backlog"
	StatusDoing     TicketStatus = "doing"
	StatusDone      TicketStatus = "done"
	StatusDiscarded TicketStatus = "discarded"
)

type Quadrant string

const (
	QuadrantQuickWin    Quadrant = "quickwin"    // high impact, low complexity
	QuadrantValueBet    Quadrant = "valuebet"    // high impact, high complexity
	QuadrantDistraction Quadrant = "distraction" // low impact, high complexity
	QuadrantTidyUp      Quadrant = "tidyup"      // low impact, low complexity
	QuadrantUnscored    Quadrant = ""            // impact or complexity not yet scored
)

type Ticket struct {
	ID           string       `json:"id"`
	Type         TicketType   `json:"type"`
	Status       TicketStatus `json:"status"`
	Title        string       `json:"title"`
	Summary      string       `json:"summary"`
	Description  string       `json:"description"`
	RepoPath     string       `json:"repo_path"`
	SessionID    string       `json:"session_id"`
	SessionTitle string       `json:"session_title"`
	SessionDate  time.Time    `json:"session_date"`
	SourceQuotes []string     `json:"source_quotes"`
	Labels       []string     `json:"labels"`
	Complexity   int          `json:"complexity"`
	Impact       int          `json:"impact"`
	AINotes      string       `json:"ai_notes"`
	UserNotes    string       `json:"user_notes"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
}

// Quadrant returns the ticket's quadrant based on impact and complexity scores.
// Returns QuadrantUnscored if either score is 0 (not yet scored).
// Threshold: high >= 6, low <= 5.
func (t Ticket) Quadrant() Quadrant {
	if t.Impact == 0 || t.Complexity == 0 {
		return QuadrantUnscored
	}
	highImpact := t.Impact >= 6
	highComplexity := t.Complexity >= 6
	switch {
	case highImpact && !highComplexity:
		return QuadrantQuickWin
	case highImpact && highComplexity:
		return QuadrantValueBet
	case !highImpact && highComplexity:
		return QuadrantDistraction
	default:
		return QuadrantTidyUp
	}
}

type Repo struct {
	Path          string    `json:"path"`
	Name          string    `json:"name"`
	LastScanAt    time.Time `json:"last_scan_at"`
	LastSessionID string    `json:"last_session_id"`
}

type TicketFilter struct {
	RepoPath string
	Status   TicketStatus
	Label    string
	Type     TicketType
	Quadrant Quadrant
}

type Store struct {
	db *sql.DB
}

func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// Add impact column to existing databases (idempotent).
	s.db.Exec("ALTER TABLE ticket ADD COLUMN impact INTEGER NOT NULL DEFAULT 0")
	// Add unread column to existing session_status tables (idempotent).
	s.db.Exec("ALTER TABLE session_status ADD COLUMN unread INTEGER NOT NULL DEFAULT 1")
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS ticket (
	id            TEXT PRIMARY KEY,
	type          TEXT NOT NULL,
	status        TEXT NOT NULL DEFAULT 'new',
	title         TEXT NOT NULL,
	summary       TEXT NOT NULL DEFAULT '',
	description   TEXT NOT NULL DEFAULT '',
	repo_path     TEXT NOT NULL DEFAULT '',
	session_id    TEXT NOT NULL DEFAULT '',
	session_title TEXT NOT NULL DEFAULT '',
	session_date  INTEGER NOT NULL DEFAULT 0,
	source_quotes TEXT NOT NULL DEFAULT '[]',
	labels        TEXT NOT NULL DEFAULT '[]',
	complexity    INTEGER NOT NULL DEFAULT 0,
	impact        INTEGER NOT NULL DEFAULT 0,
	ai_notes      TEXT NOT NULL DEFAULT '',
	user_notes    TEXT NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS repo (
	path            TEXT PRIMARY KEY,
	name            TEXT NOT NULL DEFAULT '',
	last_scan_at    INTEGER NOT NULL DEFAULT 0,
	last_session_id TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_ticket_status ON ticket(status);
CREATE INDEX IF NOT EXISTS idx_ticket_repo ON ticket(repo_path);
CREATE INDEX IF NOT EXISTS idx_ticket_session ON ticket(session_id);

CREATE TABLE IF NOT EXISTS session_status (
	session_id  TEXT PRIMARY KEY,
	status      TEXT NOT NULL DEFAULT 'idle',
	source      TEXT NOT NULL DEFAULT 'opencode',
	unread      INTEGER NOT NULL DEFAULT 1,
	updated_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_session_status_status ON session_status(status);
`

func (s *Store) SaveTicket(t *Ticket) error {
	if t.ID == "" {
		t.ID = ulid.Make().String()
	}
	now := time.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now

	quotes, _ := json.Marshal(t.SourceQuotes)
	labels, _ := json.Marshal(t.Labels)

	_, err := s.db.Exec(`
		INSERT INTO ticket (id, type, status, title, summary, description, repo_path,
			session_id, session_title, session_date, source_quotes, labels, complexity,
			impact, ai_notes, user_notes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			type=excluded.type, status=excluded.status, title=excluded.title,
			summary=excluded.summary, description=excluded.description,
			repo_path=excluded.repo_path, session_id=excluded.session_id,
			session_title=excluded.session_title, session_date=excluded.session_date,
			source_quotes=excluded.source_quotes, labels=excluded.labels,
			complexity=excluded.complexity, impact=excluded.impact,
			ai_notes=excluded.ai_notes,
			user_notes=excluded.user_notes, updated_at=excluded.updated_at
	`, t.ID, t.Type, t.Status, t.Title, t.Summary, t.Description,
		t.RepoPath, t.SessionID, t.SessionTitle, t.SessionDate.UnixMilli(),
		string(quotes), string(labels), t.Complexity, t.Impact,
		t.AINotes, t.UserNotes, t.CreatedAt.UnixMilli(), t.UpdatedAt.UnixMilli())
	return err
}

func (s *Store) GetTicket(id string) (*Ticket, error) {
	row := s.db.QueryRow(`SELECT id, type, status, title, summary, description, repo_path,
		session_id, session_title, session_date, source_quotes, labels, complexity,
		impact, ai_notes, user_notes, created_at, updated_at FROM ticket WHERE id=?`, id)
	return scanTicketFromRow(row)
}

func (s *Store) ListTickets(f TicketFilter) ([]Ticket, error) {
	query := `SELECT id, type, status, title, summary, description, repo_path,
		session_id, session_title, session_date, source_quotes, labels, complexity,
		impact, ai_notes, user_notes, created_at, updated_at FROM ticket`
	var conditions []string
	var args []any

	if f.RepoPath != "" {
		conditions = append(conditions, "repo_path=?")
		args = append(args, f.RepoPath)
	}
	if f.Status != "" {
		conditions = append(conditions, "status=?")
		args = append(args, f.Status)
	}
	if f.Type != "" {
		conditions = append(conditions, "type=?")
		args = append(args, f.Type)
	}
	if f.Label != "" {
		conditions = append(conditions, "labels LIKE ?")
		args = append(args, "%\""+f.Label+"\"%")
	}
	if f.Quadrant != "" {
		switch f.Quadrant {
		case QuadrantQuickWin:
			conditions = append(conditions, "impact >= 6 AND complexity > 0 AND complexity <= 5")
		case QuadrantValueBet:
			conditions = append(conditions, "impact >= 6 AND complexity >= 6")
		case QuadrantDistraction:
			conditions = append(conditions, "impact > 0 AND impact <= 5 AND complexity >= 6")
		case QuadrantTidyUp:
			conditions = append(conditions, "impact > 0 AND impact <= 5 AND complexity > 0 AND complexity <= 5")
		}
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	// Sort by quadrant priority: valuebet (human attention) first, then quickwin,
	// tidyup, distraction. Unscored tickets last. Within each group, by impact desc.
	query += ` ORDER BY
		CASE
			WHEN impact >= 6 AND complexity >= 6 THEN 1
			WHEN impact >= 6 AND complexity > 0 AND complexity <= 5 THEN 2
			WHEN impact > 0 AND impact <= 5 AND complexity > 0 AND complexity <= 5 THEN 3
			WHEN impact > 0 AND impact <= 5 AND complexity >= 6 THEN 4
			ELSE 5
		END,
		impact DESC,
		created_at DESC`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tickets []Ticket
	for rows.Next() {
		t, err := scanTicketFromRows(rows)
		if err != nil {
			return nil, err
		}
		tickets = append(tickets, *t)
	}
	return tickets, rows.Err()
}

func (s *Store) DeleteTicket(id string) error {
	_, err := s.db.Exec("DELETE FROM ticket WHERE id=?", id)
	return err
}

func (s *Store) SaveRepo(r *Repo) error {
	_, err := s.db.Exec(`
		INSERT INTO repo (path, name, last_scan_at, last_session_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			name=excluded.name, last_scan_at=excluded.last_scan_at,
			last_session_id=excluded.last_session_id
	`, r.Path, r.Name, r.LastScanAt.UnixMilli(), r.LastSessionID)
	return err
}

func (s *Store) GetRepo(path string) (*Repo, error) {
	var r Repo
	var lastScanAt int64
	err := s.db.QueryRow("SELECT path, name, last_scan_at, last_session_id FROM repo WHERE path=?", path).
		Scan(&r.Path, &r.Name, &lastScanAt, &r.LastSessionID)
	if err != nil {
		return nil, err
	}
	r.LastScanAt = time.UnixMilli(lastScanAt)
	return &r, nil
}

func (s *Store) ListRepos() ([]Repo, error) {
	rows, err := s.db.Query("SELECT path, name, last_scan_at, last_session_id FROM repo ORDER BY path")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var repos []Repo
	for rows.Next() {
		var r Repo
		var lastScanAt int64
		if err := rows.Scan(&r.Path, &r.Name, &lastScanAt, &r.LastSessionID); err != nil {
			return nil, err
		}
		r.LastScanAt = time.UnixMilli(lastScanAt)
		repos = append(repos, r)
	}
	return repos, rows.Err()
}

type scannable interface {
	Scan(dest ...any) error
}

func scanTicketFromScannable(s scannable) (*Ticket, error) {
	var t Ticket
	var sessionDate, createdAt, updatedAt int64
	var quotesJSON, labelsJSON string

	err := s.Scan(&t.ID, &t.Type, &t.Status, &t.Title, &t.Summary, &t.Description,
		&t.RepoPath, &t.SessionID, &t.SessionTitle, &sessionDate,
		&quotesJSON, &labelsJSON, &t.Complexity, &t.Impact,
		&t.AINotes, &t.UserNotes, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	t.SessionDate = time.UnixMilli(sessionDate)
	t.CreatedAt = time.UnixMilli(createdAt)
	t.UpdatedAt = time.UnixMilli(updatedAt)
	json.Unmarshal([]byte(quotesJSON), &t.SourceQuotes)
	json.Unmarshal([]byte(labelsJSON), &t.Labels)
	return &t, nil
}

func scanTicketFromRow(row *sql.Row) (*Ticket, error) {
	return scanTicketFromScannable(row)
}

func scanTicketFromRows(rows *sql.Rows) (*Ticket, error) {
	return scanTicketFromScannable(rows)
}

// SetSessionStatus upserts a session's status in Clank's DB.
// Does not change the unread flag — use MarkSessionRead for that.
func (s *Store) SetSessionStatus(sessionID string, status SessionState, source string) error {
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(`
		INSERT INTO session_status (session_id, status, source, unread, updated_at)
		VALUES (?, ?, ?, 1, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			status=excluded.status, source=excluded.source, updated_at=excluded.updated_at
	`, sessionID, status, source, now)
	return err
}

// MarkSessionRead marks a session as read (user has seen the agent response).
func (s *Store) MarkSessionRead(sessionID string) error {
	_, err := s.db.Exec(
		"UPDATE session_status SET unread=0 WHERE session_id=?", sessionID)
	return err
}

// GetSessionStatus returns the status for a single session, or nil if not tracked.
func (s *Store) GetSessionStatus(sessionID string) (*SessionStatus, error) {
	var ss SessionStatus
	var updatedAt int64
	var unread int
	err := s.db.QueryRow(
		"SELECT session_id, status, source, unread, updated_at FROM session_status WHERE session_id=?",
		sessionID,
	).Scan(&ss.SessionID, &ss.Status, &ss.Source, &unread, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ss.Unread = unread != 0
	ss.UpdatedAt = time.UnixMilli(updatedAt)
	return &ss, nil
}

// ListSessionStatuses returns all tracked session statuses, optionally filtered.
func (s *Store) ListSessionStatuses(statuses ...SessionState) (map[string]*SessionStatus, error) {
	query := "SELECT session_id, status, source, unread, updated_at FROM session_status"
	var args []any
	if len(statuses) > 0 {
		placeholders := make([]string, len(statuses))
		for i, st := range statuses {
			placeholders[i] = "?"
			args = append(args, string(st))
		}
		query += " WHERE status IN (" + strings.Join(placeholders, ",") + ")"
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]*SessionStatus)
	for rows.Next() {
		var ss SessionStatus
		var updatedAt int64
		var unread int
		if err := rows.Scan(&ss.SessionID, &ss.Status, &ss.Source, &unread, &updatedAt); err != nil {
			return nil, err
		}
		ss.Unread = unread != 0
		ss.UpdatedAt = time.UnixMilli(updatedAt)
		result[ss.SessionID] = &ss
	}
	return result, rows.Err()
}

// TopTicketsByImpact returns the top N tickets sorted by impact DESC.
func (s *Store) TopTicketsByImpact(limit int) ([]Ticket, error) {
	query := `SELECT id, type, status, title, summary, description, repo_path,
		session_id, session_title, session_date, source_quotes, labels, complexity,
		impact, ai_notes, user_notes, created_at, updated_at FROM ticket
		WHERE status NOT IN ('done', 'discarded')
		ORDER BY impact DESC, created_at DESC
		LIMIT ?`
	rows, err := s.db.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tickets []Ticket
	for rows.Next() {
		t, err := scanTicketFromRows(rows)
		if err != nil {
			return nil, err
		}
		tickets = append(tickets, *t)
	}
	return tickets, rows.Err()
}
