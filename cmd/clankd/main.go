package main

import (
	"os"

	"github.com/supaclank/clank/internal/cli/daemoncli"
)

func main() {
	if err := daemoncli.Command().Execute(); err != nil {
		os.Exit(1)
	}
}
