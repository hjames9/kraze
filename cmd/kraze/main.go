package main

import (
	"fmt"
	"os"

	"github.com/hjames9/kraze/internal/cli"
)

var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

func main() {
	cli.SetVersionInfo(Version, GitCommit, BuildDate)

	if err := cli.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
