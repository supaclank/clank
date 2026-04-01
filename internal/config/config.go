package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// Dir returns the path to the clank configuration directory (~/.clank).
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".clank"), nil
}
