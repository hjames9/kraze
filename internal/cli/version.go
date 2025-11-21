package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version information",
	Long:  `Display version information for kraze including version, git commit, and build date.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Printf("kraze version: %s\n", version)
		fmt.Printf("Git commit: %s\n", gitCommit)
		fmt.Printf("Built: %s\n", buildDate)
		return nil
	},
}
