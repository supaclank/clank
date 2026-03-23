package context

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".clank", "context"), nil
}

func Init() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create context dir: %w", err)
	}

	defaults := map[string]string{
		"roadmap.md":  "# Roadmap\n\nDescribe your product roadmap here.\n",
		"strategy.md": "# Strategy\n\nDescribe your product/company strategy here.\n",
		"ideas.md":    "# Ideas\n\nCapture ideas and loose threads here.\n",
	}

	for name, content := range defaults {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", name, err)
			}
		}
	}
	return nil
}

func LoadAll() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}

	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read context dir: %w", err)
	}

	var parts []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", e.Name(), err)
		}
		parts = append(parts, fmt.Sprintf("--- %s ---\n%s", e.Name(), string(data)))
	}
	return strings.Join(parts, "\n\n"), nil
}

func List() ([]string, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	return names, nil
}
