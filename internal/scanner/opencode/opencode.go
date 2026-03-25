package opencode

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/acksell/clank/internal/scanner"
	_ "modernc.org/sqlite"
)

// SessionInfo is a lightweight session record (no messages/parts loaded).
type SessionInfo struct {
	ID        string
	Title     string
	Directory string
	RepoName  string // basename of the project worktree
	Worktree  string // full path to the project worktree
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Scanner struct {
	dbPath string
}

func New(dbPath string) *Scanner {
	return &Scanner{dbPath: dbPath}
}

func (s *Scanner) Name() string { return "opencode" }

func (s *Scanner) open() (*sql.DB, error) {
	return sql.Open("sqlite", s.dbPath+"?mode=ro&_journal_mode=WAL")
}

func (s *Scanner) Scan(repoPath string, afterSessionID string) ([]scanner.RawSession, error) {
	db, err := s.open()
	if err != nil {
		return nil, fmt.Errorf("open opencode db: %w", err)
	}
	defer db.Close()

	query := `
		SELECT s.id, s.title, s.directory, s.time_created, s.time_updated
		FROM session s
		JOIN project p ON s.project_id = p.id
		WHERE p.worktree = ?
	`
	args := []any{repoPath}

	if afterSessionID != "" {
		query += ` AND s.time_created > (SELECT time_created FROM session WHERE id = ?)`
		args = append(args, afterSessionID)
	}
	query += ` ORDER BY s.time_created ASC`

	return s.querySessions(db, query, args...)
}

// ListProjects returns all distinct worktree paths from the opencode DB.
func (s *Scanner) ListProjects() ([]string, error) {
	db, err := s.open()
	if err != nil {
		return nil, fmt.Errorf("open opencode db: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT DISTINCT worktree FROM project ORDER BY worktree`)
	if err != nil {
		return nil, fmt.Errorf("query projects: %w", err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// ListRecentSessions returns lightweight session info (no messages/parts).
// Results are ordered by time_updated DESC. Pass limit=0 for no limit.
func (s *Scanner) ListRecentSessions(limit int) ([]SessionInfo, error) {
	db, err := s.open()
	if err != nil {
		return nil, fmt.Errorf("open opencode db: %w", err)
	}
	defer db.Close()

	query := `
		SELECT s.id, s.title, s.directory, p.worktree, s.time_created, s.time_updated
		FROM session s
		JOIN project p ON s.project_id = p.id
		WHERE (s.parent_id IS NULL OR s.parent_id = '')
		ORDER BY s.time_updated DESC
	`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query recent sessions: %w", err)
	}
	defer rows.Close()

	var sessions []SessionInfo
	for rows.Next() {
		var si SessionInfo
		var createdAt, updatedAt int64
		if err := rows.Scan(&si.ID, &si.Title, &si.Directory, &si.Worktree, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		si.CreatedAt = time.UnixMilli(createdAt)
		si.UpdatedAt = time.UnixMilli(updatedAt)
		si.RepoName = filepath.Base(si.Worktree)
		sessions = append(sessions, si)
	}
	return sessions, rows.Err()
}

func (s *Scanner) querySessions(db *sql.DB, query string, args ...any) ([]scanner.RawSession, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	var sessions []scanner.RawSession
	for rows.Next() {
		var sess scanner.RawSession
		var createdAt, updatedAt int64
		if err := rows.Scan(&sess.ID, &sess.Title, &sess.Directory, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		sess.CreatedAt = time.UnixMilli(createdAt)
		sess.UpdatedAt = time.UnixMilli(updatedAt)

		msgs, err := s.loadMessages(db, sess.ID)
		if err != nil {
			return nil, fmt.Errorf("load messages for %s: %w", sess.ID, err)
		}
		sess.Messages = msgs
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

type messageData struct {
	Role  string `json:"role"`
	Mode  string `json:"mode"`
	Agent string `json:"agent"`
	Model struct {
		ModelID string `json:"modelID"`
	} `json:"model"`
	ModelID string `json:"modelID"`
	Time    struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

type partData struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (s *Scanner) loadMessages(db *sql.DB, sessionID string) ([]scanner.Message, error) {
	rows, err := db.Query(`
		SELECT id, data, time_created FROM message
		WHERE session_id = ?
		ORDER BY time_created ASC, id ASC
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []scanner.Message
	for rows.Next() {
		var id, dataStr string
		var createdAt int64
		if err := rows.Scan(&id, &dataStr, &createdAt); err != nil {
			return nil, err
		}

		var md messageData
		json.Unmarshal([]byte(dataStr), &md)

		model := md.Model.ModelID
		if model == "" {
			model = md.ModelID
		}

		msg := scanner.Message{
			ID:        id,
			Role:      md.Role,
			Mode:      md.Mode,
			Agent:     md.Agent,
			Model:     model,
			CreatedAt: time.UnixMilli(createdAt),
		}

		parts, err := s.loadParts(db, id)
		if err != nil {
			return nil, fmt.Errorf("load parts for %s: %w", id, err)
		}
		msg.Parts = parts
		msgs = append(msgs, msg)
	}
	return msgs, rows.Err()
}

func (s *Scanner) loadParts(db *sql.DB, messageID string) ([]scanner.Part, error) {
	rows, err := db.Query(`
		SELECT id, data FROM part
		WHERE message_id = ?
		ORDER BY time_created ASC, id ASC
	`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var parts []scanner.Part
	for rows.Next() {
		var id, dataStr string
		if err := rows.Scan(&id, &dataStr); err != nil {
			return nil, err
		}

		var pd partData
		json.Unmarshal([]byte(dataStr), &pd)

		parts = append(parts, scanner.Part{
			ID:   id,
			Type: pd.Type,
			Text: pd.Text,
			Data: dataStr,
		})
	}
	return parts, rows.Err()
}
