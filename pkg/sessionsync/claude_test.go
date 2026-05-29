package sessionsync

import (
	"context"
	"errors"
	"testing"

	"github.com/acksell/clank/internal/agent"
	claudecode "github.com/severity1/claude-agent-sdk-go"
)

func TestClaudeDiscovered_Mapping(t *testing.T) {
	t.Parallel()
	cwd := "/Users/me/repo"
	created := int64(1_700_000_000_000)
	info := claudecode.SDKSessionInfo{
		SessionID:    "sess-123",
		Summary:      "Refactor auth",
		Cwd:          &cwd,
		LastModified: 1_700_000_100_000,
		CreatedAt:    &created,
	}
	got := claudeDiscovered(info)

	if got.Backend != agent.BackendClaudeCode {
		t.Errorf("Backend = %q, want %q", got.Backend, agent.BackendClaudeCode)
	}
	if got.ExternalID != "sess-123" {
		t.Errorf("ExternalID = %q, want sess-123", got.ExternalID)
	}
	if got.Title != "Refactor auth" {
		t.Errorf("Title = %q, want 'Refactor auth'", got.Title)
	}
	if got.ProjectDir != cwd {
		t.Errorf("ProjectDir = %q, want %q", got.ProjectDir, cwd)
	}
	if got.CreatedAt.UnixMilli() != created {
		t.Errorf("CreatedAt = %d, want %d", got.CreatedAt.UnixMilli(), created)
	}
	if got.UpdatedAt.UnixMilli() != 1_700_000_100_000 {
		t.Errorf("UpdatedAt = %d, want 1700000100000", got.UpdatedAt.UnixMilli())
	}
}

func TestClaudeDiscovered_NilCwdAndCreatedAt(t *testing.T) {
	t.Parallel()
	info := claudecode.SDKSessionInfo{SessionID: "s", LastModified: 1234}
	got := claudeDiscovered(info)

	if got.ProjectDir != "" {
		t.Errorf("ProjectDir = %q, want empty when Cwd nil", got.ProjectDir)
	}
	if !got.CreatedAt.Equal(got.UpdatedAt) {
		t.Errorf("CreatedAt %v != UpdatedAt %v; want CreatedAt to fall back to UpdatedAt", got.CreatedAt, got.UpdatedAt)
	}
}

func TestClaudeBackend_ExportImportNotImplemented(t *testing.T) {
	t.Parallel()
	var be ClaudeBackend
	if err := be.ExportSession(context.Background(), "", "id", nil); !errors.Is(err, ErrExportNotImplemented) {
		t.Errorf("ExportSession err = %v, want ErrExportNotImplemented", err)
	}
	if _, err := be.ImportSession(context.Background(), "", "blob"); !errors.Is(err, ErrExportNotImplemented) {
		t.Errorf("ImportSession err = %v, want ErrExportNotImplemented", err)
	}
}
