package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// OpenAISinkPath is where clank stores OpenAI/Codex connection state —
// clank-owned for the same reasons as the Anthropic sink: codex never
// rewrites this file. The API key is consumed as env injection on the
// spawned codex-acp adapter; the ChatGPT flag records that this host
// completed the device-auth ceremony (the tokens themselves live in
// codex's own $CODEX_HOME/auth.json, which codex refreshes in place).
func (a *AuthManager) OpenAISinkPath() string {
	return filepath.Join(a.homeDir, ".local", "share", "clank", "openai.json")
}

// openaiSink is the on-disk shape at OpenAISinkPath.
type openaiSink struct {
	APIKey string `json:"api_key,omitempty"`
	// ChatGPTConnected means the codex device-auth ceremony succeeded
	// on this host. Status additionally requires codex's auth.json to
	// still exist — `codex logout` outside clank wins.
	ChatGPTConnected bool `json:"chatgpt_connected,omitempty"`
}

func (a *AuthManager) readOpenAISink() (openaiSink, error) {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	return a.readOpenAISinkLocked()
}

func (a *AuthManager) readOpenAISinkLocked() (openaiSink, error) {
	var sink openaiSink
	data, err := os.ReadFile(a.OpenAISinkPath())
	if errors.Is(err, os.ErrNotExist) || len(data) == 0 {
		return sink, nil
	}
	if err != nil {
		return sink, fmt.Errorf("read %s: %w", a.OpenAISinkPath(), err)
	}
	if err := json.Unmarshal(data, &sink); err != nil {
		return sink, fmt.Errorf("parse %s: %w", a.OpenAISinkPath(), err)
	}
	return sink, nil
}

// SetOpenAIAPIKey stores the API key injected into codex-acp spawns.
// Callers fire the OpenAI credential callback afterwards so the
// adapters restart with the new value. A ChatGPT connection is left
// intact — which credential codex then prefers is codex's resolution.
func (a *AuthManager) SetOpenAIAPIKey(key string) error {
	return a.updateOpenAISink(func(sink *openaiSink) { sink.APIKey = key })
}

// setOpenAIChatGPTConnected records a completed device-auth ceremony
// and clears any stored API key: the user explicitly switched to the
// subscription, and a lingering CODEX_API_KEY in the adapter env could
// shadow the fresh ChatGPT login.
func (a *AuthManager) setOpenAIChatGPTConnected() error {
	return a.updateOpenAISink(func(sink *openaiSink) {
		sink.ChatGPTConnected = true
		sink.APIKey = ""
	})
}

func (a *AuthManager) clearOpenAIAPIKey() error {
	return a.updateOpenAISink(func(sink *openaiSink) { sink.APIKey = "" })
}

func (a *AuthManager) clearOpenAIChatGPTConnected() error {
	return a.updateOpenAISink(func(sink *openaiSink) { sink.ChatGPTConnected = false })
}

// updateOpenAISink applies mutate to the stored sink under authMu.
// A sink with nothing left in it is deleted rather than persisted as
// an empty JSON husk.
func (a *AuthManager) updateOpenAISink(mutate func(*openaiSink)) error {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	sink, err := a.readOpenAISinkLocked()
	if err != nil {
		return err
	}
	mutate(&sink)
	path := a.OpenAISinkPath()
	if sink == (openaiSink{}) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove openai sink: %w", err)
		}
		return nil
	}
	data, err := json.MarshalIndent(sink, "", "  ")
	if err != nil {
		return fmt.Errorf("encode openai sink: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create openai sink dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write tmp openai sink: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename openai sink: %w", err)
	}
	return nil
}

// OpenAIEnv returns env vars for a spawned codex-acp adapter, or nil
// when no API key is stored — codex then uses its own login state in
// $CODEX_HOME/auth.json (the device-auth ceremony's sink, or a laptop's
// pre-existing `codex login`).
func (a *AuthManager) OpenAIEnv() map[string]string {
	sink, err := a.readOpenAISink()
	if err != nil || sink.APIKey == "" {
		return nil
	}
	return map[string]string{EnvCodexAPIKey: sink.APIKey}
}

// openAIChatGPTConnected reports whether the subscription is usable on
// this host: the ceremony completed here AND codex's auth.json still
// exists (a `codex logout` outside clank clears the login for real).
func (a *AuthManager) openAIChatGPTConnected() bool {
	sink, err := a.readOpenAISink()
	if err != nil || !sink.ChatGPTConnected {
		return false
	}
	return codexCLILoginPresent(a.codexAuthJSONPath())
}
