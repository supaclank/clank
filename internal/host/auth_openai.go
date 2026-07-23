package host

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// OpenAISinkPath is where clank stores OpenAI/Codex credentials —
// clank-owned for the same reasons as the Anthropic sink: the consumer
// is env injection on the spawned codex-acp adapter, never a file codex
// itself rewrites.
func (a *AuthManager) OpenAISinkPath() string {
	return filepath.Join(a.homeDir, ".local", "share", "clank", "openai.json")
}

// openaiSink is the on-disk shape at OpenAISinkPath.
type openaiSink struct {
	APIKey string `json:"api_key,omitempty"`
}

func (a *AuthManager) readOpenAISink() (openaiSink, error) {
	var sink openaiSink
	b, err := os.ReadFile(a.OpenAISinkPath())
	if err != nil {
		return sink, err
	}
	if err := json.Unmarshal(b, &sink); err != nil {
		return sink, fmt.Errorf("parse %s: %w", a.OpenAISinkPath(), err)
	}
	return sink, nil
}

// SetOpenAIAPIKey stores the API key injected into codex-acp spawns.
// Callers nudge the ACP supervisor afterwards so the env-fingerprint
// restart picks it up.
func (a *AuthManager) SetOpenAIAPIKey(key string) error {
	sink := openaiSink{APIKey: key}
	b, err := json.MarshalIndent(sink, "", "  ")
	if err != nil {
		return err
	}
	path := a.OpenAISinkPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// OpenAIEnv returns env vars for a spawned codex-acp adapter, or nil
// when no OpenAI provider is connected — codex then falls back to its
// own ChatGPT login in ~/.codex (mirroring the claude keychain
// fallback philosophy).
func (a *AuthManager) OpenAIEnv() map[string]string {
	sink, err := a.readOpenAISink()
	if err != nil || sink.APIKey == "" {
		return nil
	}
	return map[string]string{EnvCodexAPIKey: sink.APIKey}
}
