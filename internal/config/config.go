package config

import (
	"fmt"
	"os"
	"path/filepath"

	toml "github.com/pelletier/go-toml/v2"
)

// Provider constants.
const (
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
	ProviderGemini    = "gemini"
)

type Config struct {
	LLM   LLMConfig  `toml:"llm"`
	Repos []string   `toml:"repos"`
	Scan  ScanConfig `toml:"scan"`
}

type LLMConfig struct {
	Provider string `toml:"provider"`
	APIKey   string `toml:"api_key"`
	Model    string `toml:"model"`
	BaseURL  string `toml:"base_url,omitempty"`
}

type ScanConfig struct {
	OpenCodeDB string `toml:"opencode_db"`
}

// ProviderInfo describes a supported LLM provider.
type ProviderInfo struct {
	Name         string // display name
	ID           string // config value
	EnvKey       string // env var for API key
	DefaultModel string
}

// Providers returns the list of supported providers.
func Providers() []ProviderInfo {
	return []ProviderInfo{
		{Name: "OpenAI", ID: ProviderOpenAI, EnvKey: "OPENAI_API_KEY", DefaultModel: "gpt-4o-mini"},
		{Name: "Anthropic", ID: ProviderAnthropic, EnvKey: "ANTHROPIC_API_KEY", DefaultModel: "claude-sonnet-4-20250514"},
		{Name: "Gemini", ID: ProviderGemini, EnvKey: "GOOGLE_API_KEY", DefaultModel: "gemini-2.0-flash"},
	}
}

// ProviderByID returns the ProviderInfo for a given provider ID.
func ProviderByID(id string) (ProviderInfo, bool) {
	for _, p := range Providers() {
		if p.ID == id {
			return p, true
		}
	}
	return ProviderInfo{}, false
}

// DetectEnvKeys returns a map of provider ID -> detected API key from environment.
func DetectEnvKeys() map[string]string {
	detected := make(map[string]string)
	for _, p := range Providers() {
		if v := os.Getenv(p.EnvKey); v != "" {
			detected[p.ID] = v
		}
	}
	return detected
}

func DefaultConfig() Config {
	return Config{
		LLM: LLMConfig{
			Provider: ProviderOpenAI,
			Model:    "gpt-4o-mini",
		},
		Scan: ScanConfig{
			OpenCodeDB: defaultOpenCodeDB(),
		},
	}
}

func defaultOpenCodeDB() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db")
}

func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".clank"), nil
}

func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

func Load() (Config, error) {
	cfg := DefaultConfig()
	p, err := Path()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

func Save(cfg Config) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(p, data, 0o600)
}

// ResolveAPIKey returns the API key for the configured provider,
// checking config first, then falling back to the provider's env var.
func (c LLMConfig) ResolveAPIKey() string {
	if c.APIKey != "" {
		return c.APIKey
	}
	if p, ok := ProviderByID(c.Provider); ok {
		return os.Getenv(p.EnvKey)
	}
	return ""
}
