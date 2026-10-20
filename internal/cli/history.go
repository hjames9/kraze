package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/history"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
)

var (
	historyLimit       int
	historyOutput      string
	historyAllProjects bool
)

var historyCmd = &cobra.Command{
	Use:   "history [services...]",
	Short: "Show the history of changes made to the cluster",
	Long: `Display the commands that changed the cluster, newest first: who ran them,
when, and which services were installed, upgraded or removed.

History is stored in the cluster (ConfigMap kraze-history in kube-system), so it
is shared by everyone using the cluster and removed by 'kraze destroy'. It keeps
the most recent 200 commands. On a cluster shared by several projects, only
this config's project is shown unless --all-projects is given.

Examples:
  kraze history                 # Show the 20 most recent commands
  kraze history backend         # Show only commands that changed backend
  kraze history --limit 0       # Show all recorded commands
  kraze history -v              # Include per-service durations and errors
  kraze history -o json         # Machine-readable output
  kraze history --all-projects  # Include every project sharing the cluster`,
	ValidArgsFunction: getServiceNames,
	RunE:              runHistory,
}

func runHistory(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if historyOutput != "table" && historyOutput != "json" {
		return fmt.Errorf("invalid output format '%s', must be 'table' or 'json'", historyOutput)
	}
	if historyLimit < 0 {
		return fmt.Errorf("--limit must be 0 (all) or a positive number")
	}

	loaded, cleanupPack, err := loadConfig(cmd, "")
	if err != nil {
		return err
	}
	defer cleanupPack()
	cfg := loaded.cfg

	if err := requireDocker(ctx, cfg); err != nil {
		return err
	}
	conn, err := connectCluster(ctx, deps.newClusterManager(), cfg, skipMissing)
	if err != nil {
		return err
	}
	if conn == nil {
		fmt.Printf("Cluster '%s' is not running\n", cfg.Cluster.Name)
		return nil
	}

	entries, err := history.Load(ctx, conn.clientset)
	if err != nil {
		return err
	}
	entries = selectHistory(entries, args, cfg.Project, historyAllProjects, historyLimit)

	if historyOutput == "json" {
		data, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal history: %w", err)
		}
		fmt.Println(string(data))
		return nil
	}

	if len(entries) == 0 {
		fmt.Printf("No history recorded for cluster '%s'\n", cfg.Cluster.Name)
		return nil
	}
	fmt.Printf("Cluster: %s\n", cfg.Cluster.Name)
	if cfg.Project != "" && !historyAllProjects {
		fmt.Printf("Project: %s\n", cfg.Project)
	}
	fmt.Println()
	fmt.Print(formatHistory(entries, verbose, historyAllProjects))
	return nil
}

// selectHistory returns entries newest first, keeping only those run by project
// or recorded before projects existed (unless allProjects) that changed one of
// services (if any are given), up to limit entries (0 for all)
func selectHistory(entries []history.Entry, services []string, project string, allProjects bool, limit int) []history.Entry {
	selected := make([]history.Entry, 0, len(entries))
	for _, e := range slices.Backward(entries) {
		if !allProjects && e.Project != project && e.Project != "" {
			continue
		}
		if len(services) > 0 && !slices.ContainsFunc(e.Changes, func(c history.Change) bool {
			return slices.Contains(services, c.Name)
		}) {
			continue
		}
		selected = append(selected, e)
		if limit > 0 && len(selected) == limit {
			break
		}
	}
	return selected
}

func formatHistory(entries []history.Entry, verbose, showProject bool) string {
	var b strings.Builder
	if showProject {
		fmt.Fprintf(&b, "%-19s  %-15s  %-20s  %-30s  %-11s  %s\n", "TIME", "PROJECT", "USER", "COMMAND", "RESULT", "CHANGES")
		b.WriteString(strings.Repeat("-", 117) + "\n")
	} else {
		fmt.Fprintf(&b, "%-19s  %-20s  %-30s  %-11s  %s\n", "TIME", "USER", "COMMAND", "RESULT", "CHANGES")
		b.WriteString(strings.Repeat("-", 100) + "\n")
	}

	for _, e := range entries {
		command := e.Args
		if command == "" {
			command = e.Command
		}
		// Pad before coloring so ANSI codes don't break column alignment
		result := colorResult(e.Result, fmt.Sprintf("%-11s", displayResult(e.Result)))

		fmt.Fprintf(&b, "%-19s  ", e.StartedAt.Local().Format(time.DateTime))
		if showProject {
			fmt.Fprintf(&b, "%-15s  ", truncate(projectLabel(e.Project), 15))
		}
		fmt.Fprintf(&b, "%-20s  %-30s  %s  %s\n",
			truncate(e.User, 20),
			truncate(command, 30),
			result,
			summarizeChanges(e))

		if verbose {
			for _, c := range e.Changes {
				fmt.Fprintf(&b, "    %-10s  %-30s  %-11s  %s\n",
					c.Action, truncate(c.Name, 30), displayResult(c.Result),
					(time.Duration(c.DurationMs) * time.Millisecond).Round(100*time.Millisecond))
				if c.Error != "" {
					fmt.Fprintf(&b, "      error: %s\n", c.Error)
				}
			}
			if e.Error != "" {
				fmt.Fprintf(&b, "    error: %s\n", e.Error)
			}
		}
	}

	b.WriteString("\nChanges: + install  ~ upgrade  - uninstall  ^ load image  ✗ failed  ! interrupted\n")
	return b.String()
}

// summarizeChanges renders an entry's changes compactly, e.g. "+redis ~backend ✗api"
func summarizeChanges(e history.Entry) string {
	var parts []string
	if e.ClusterCreated {
		parts = append(parts, "(cluster created)")
	}
	for _, c := range e.Changes {
		var symbol string
		switch {
		case c.Result == history.ResultFailed:
			symbol = "✗"
		case c.Result == history.ResultInterrupted:
			symbol = "!"
		case c.Action == history.ActionInstall:
			symbol = "+"
		case c.Action == history.ActionUpgrade:
			symbol = "~"
		case c.Action == history.ActionUninstall:
			symbol = "-"
		case c.Action == history.ActionLoadImage:
			symbol = "^"
		}
		parts = append(parts, symbol+c.Name)
	}
	return strings.Join(parts, " ")
}

// displayResult names a running entry "incomplete": it is either still in
// progress or was killed before it could record a result, and kraze cannot tell
// which
func displayResult(r history.Result) string {
	if r == history.ResultRunning {
		return "incomplete"
	}
	return string(r)
}

func colorResult(r history.Result, s string) string {
	switch r {
	case history.ResultSucceeded:
		return color.Green(s)
	case history.ResultFailed:
		return color.Red(s)
	default:
		return color.Yellow(s)
	}
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-3]) + "..."
}

// beginHistory starts recording a cluster-changing command. History is
// best-effort: a failed write is reported in verbose mode and never fails the command.
func beginHistory(ctx context.Context, clientset kubernetes.Interface, project, command string, clusterCreated bool) *history.Recorder {
	rec, err := history.Begin(ctx, clientset, history.Entry{
		Project:        project,
		Command:        command,
		Args:           strings.Join(os.Args[1:], " "),
		KrazeVersion:   version,
		ClusterCreated: clusterCreated,
	})
	if err != nil {
		Verbose("Warning: failed to record history: %v", err)
	}
	return rec
}

// finishHistory records the command's final result
func finishHistory(rec *history.Recorder, err error) {
	if finishErr := rec.Finish(err); finishErr != nil {
		Verbose("Warning: failed to record history: %v", finishErr)
	}
}

func init() {
	historyCmd.Flags().IntVar(&historyLimit, "limit", 20, "Maximum number of commands to show (0 for all)")
	historyCmd.Flags().BoolVar(&historyAllProjects, "all-projects", false, "Show commands run by every project sharing the cluster")
	historyCmd.Flags().StringVarP(&historyOutput, "output", "o", "table", "Output format: table or json")
}
