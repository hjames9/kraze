package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/hjames9/kraze/internal/cli"
	"k8s.io/klog/v2"
)

var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

func main() {
	// Suppress klog output (used by Kubernetes client libraries)
	// This prevents "Warning: unrecognized format" and other k8s client warnings
	klog.InitFlags(nil)
	// These flags are registered by klog.InitFlags above, so Set cannot fail
	_ = flag.Set("logtostderr", "false")
	_ = flag.Set("alsologtostderr", "false")
	_ = flag.Set("stderrthreshold", "FATAL")
	flag.Parse()

	cli.SetVersionInfo(Version, GitCommit, BuildDate)

	if err := cli.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
