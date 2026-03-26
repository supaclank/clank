package main

import (
	"os"

	"github.com/acksell/clank/internal/cli/clankcli"
)

func main() {
	if err := clankcli.Command().Execute(); err != nil {
		os.Exit(1)
	}
}
