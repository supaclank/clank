package clankcli

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/acksell/clank/internal/launchconfig"
)

func choosePreviewSetupScope(in io.Reader, out io.Writer) (launchconfig.Scope, error) {
	fmt.Fprintln(out, "One-time setup: this project has no web preview launch configuration.")
	fmt.Fprintln(out, "No keeps the generated configuration private to this machine.")
	scanner := bufio.NewScanner(in)
	for {
		fmt.Fprintf(out, "Share generated config with the repo at %s? [y/N] ", launchconfig.ProjectRelativePath)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", fmt.Errorf("read preview setup choice: %w", err)
			}
			fmt.Fprintln(out)
			return launchconfig.ScopeHost, nil
		}
		switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
		case "", "n", "no":
			return launchconfig.ScopeHost, nil
		case "y", "yes":
			return launchconfig.ScopeProject, nil
		default:
			fmt.Fprintln(out, "Please answer y or n.")
		}
	}
}
