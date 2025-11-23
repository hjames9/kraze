package cli

import (
	"os"

	"github.com/hjames9/kraze/internal/config"
	"github.com/spf13/cobra"
)

// getServiceNames returns a list of service names from the config file for shell completion
func getServiceNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Get config file path from flag
	configFile, _ := cmd.Flags().GetString("file")
	if configFile == "" {
		configFile = "kraze.yml"
	}

	// Parse config
	cfg, err := config.Parse(configFile)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	// Extract service names
	services := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		services = append(services, name)
	}

	return services, cobra.ShellCompDirectiveNoFileComp
}

var completionCmd = &cobra.Command{
	Use:   "completion [bash|zsh|fish|powershell]",
	Short: "Generate completion script",
	Long: `Generate shell completion scripts for kraze commands.

To load completions:

Bash:
  $ source <(kraze completion bash)

  # To load completions for each session, execute once:
  # Linux:
  $ kraze completion bash > /etc/bash_completion.d/kraze
  # macOS:
  $ kraze completion bash > $(brew --prefix)/etc/bash_completion.d/kraze

Zsh:
  # If shell completion is not already enabled in your environment,
  # you will need to enable it. Add to ~/.zshrc:
  $ echo "autoload -U compinit; compinit" >> ~/.zshrc

  # To load completions for each session, execute once:
  $ kraze completion zsh > "${fpath[1]}/_kraze"

  # You may need to start a new shell for this setup to take effect.

Fish:
  $ kraze completion fish | source

  # To load completions for each session, execute once:
  $ kraze completion fish > ~/.config/fish/completions/kraze.fish

PowerShell:
  PS> kraze completion powershell | Out-String | Invoke-Expression

  # To load completions for every new session, run:
  PS> kraze completion powershell > kraze.ps1
  # and source this file from your PowerShell profile.
`,
	DisableFlagsInUseLine: true,
	ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
	Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
	Run: func(cmd *cobra.Command, args []string) {
		switch args[0] {
		case "bash":
			cmd.Root().GenBashCompletion(os.Stdout)
		case "zsh":
			cmd.Root().GenZshCompletion(os.Stdout)
		case "fish":
			cmd.Root().GenFishCompletion(os.Stdout, true)
		case "powershell":
			cmd.Root().GenPowerShellCompletionWithDesc(os.Stdout)
		}
	},
}
