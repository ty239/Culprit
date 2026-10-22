package main

import (
	"os"

	"github.com/culprit/culprit/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}
