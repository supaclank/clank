package scanner

import "time"

// RawSession represents a coding session extracted from a source.
type RawSession struct {
	ID        string
	Title     string
	Directory string
	Messages  []Message
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Message represents a single message in a coding session.
type Message struct {
	ID        string
	Role      string
	Parts     []Part
	Mode      string
	Agent     string
	Model     string
	CreatedAt time.Time
}

// Part represents a piece of content within a message.
type Part struct {
	ID   string
	Type string
	Text string
	Data string
}

// Scanner reads coding sessions from a source.
type Scanner interface {
	Scan(repoPath string, afterSessionID string) ([]RawSession, error)
	ScanAll(afterSessionID string) ([]RawSession, error)
	Name() string
}
