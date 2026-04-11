package voice

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"
)

func TestLoggingToolExecutor_LogsArgsAndResult(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	inner := func(name string, args json.RawMessage) (string, error) {
		return "ok: done", nil
	}

	executor := loggingToolExecutor(inner, logger, false)
	result, err := executor("test_tool", json.RawMessage(`{"key":"value"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok: done" {
		t.Fatalf("expected 'ok: done', got %q", result)
	}

	output := buf.String()
	if !strings.Contains(output, `[voice-tool] test_tool args={"key":"value"}`) {
		t.Errorf("expected args log line, got:\n%s", output)
	}
	// Short result (<500 bytes) should be logged in full.
	if !strings.Contains(output, `[voice-tool] test_tool result=ok: done`) {
		t.Errorf("expected full result log line, got:\n%s", output)
	}
}

func TestLoggingToolExecutor_LogsError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	inner := func(name string, args json.RawMessage) (string, error) {
		return "", &testError{"something broke"}
	}

	executor := loggingToolExecutor(inner, logger, false)
	_, err := executor("fail_tool", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error")
	}

	output := buf.String()
	if !strings.Contains(output, "[voice-tool] fail_tool error=something broke") {
		t.Errorf("expected error log line, got:\n%s", output)
	}
}

func TestLoggingToolExecutor_SummarizesLargeResult(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	// Generate a result larger than 500 bytes.
	largeResult := strings.Repeat("- session line\n", 100)

	inner := func(name string, args json.RawMessage) (string, error) {
		return largeResult, nil
	}

	executor := loggingToolExecutor(inner, logger, false)
	result, err := executor("list_sessions", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != largeResult {
		t.Fatal("result should be passed through unchanged")
	}

	output := buf.String()
	// Should NOT contain the full result.
	if strings.Contains(output, largeResult) {
		t.Error("expected summarized output, but got full result in logs")
	}
	// Should contain byte count and preview.
	if !strings.Contains(output, "returned") {
		t.Errorf("expected 'returned' summary in log, got:\n%s", output)
	}
	if !strings.Contains(output, "bytes") {
		t.Errorf("expected 'bytes' in summary, got:\n%s", output)
	}
}

func TestLoggingToolExecutor_DebugLogsFullResult(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	largeResult := strings.Repeat("x", 1000)

	inner := func(name string, args json.RawMessage) (string, error) {
		return largeResult, nil
	}

	// debug=true should log the full result even when large.
	executor := loggingToolExecutor(inner, logger, true)
	_, err := executor("big_tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "[voice-tool] big_tool result="+largeResult) {
		t.Errorf("expected full result in debug mode, got:\n%s", output)
	}
}

// testError is a simple error type for testing.
type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
