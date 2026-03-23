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
// Threshold: high >= 3, low <= 2.
func (t Ticket) Quadrant() Quadrant {
	if t.Impact == 0 || t.Complexity == 0 {
		return QuadrantUnscored
	}
	highImpact := t.Impact >= 3
	highComplexity := t.Complexity >= 3
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
			conditions = append(conditions, "impact >= 3 AND complexity > 0 AND complexity <= 2")
		case QuadrantValueBet:
			conditions = append(conditions, "impact >= 3 AND complexity >= 3")
		case QuadrantDistraction:
			conditions = append(conditions, "impact > 0 AND impact <= 2 AND complexity >= 3")
		case QuadrantTidyUp:
			conditions = append(conditions, "impact > 0 AND impact <= 2 AND complexity > 0 AND complexity <= 2")
		}
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	// Sort by quadrant priority: valuebet (human attention) first, then quickwin,
	// tidyup, distraction. Unscored tickets last. Within each group, by impact desc.
	query += ` ORDER BY
		CASE
			WHEN impact >= 3 AND complexity >= 3 THEN 1
			WHEN impact >= 3 AND complexity > 0 AND complexity <= 2 THEN 2
			WHEN impact > 0 AND impact <= 2 AND complexity > 0 AND complexity <= 2 THEN 3
			WHEN impact > 0 AND impact <= 2 AND complexity >= 3 THEN 4
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
