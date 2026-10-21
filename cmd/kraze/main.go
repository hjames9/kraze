package main

import (
	"flag"
	"fmt"
	"os"
	"sync"

	"github.com/hjames9/kraze/internal/cli"
	"k8s.io/klog/v2"
)

var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

func main() {
	os.Exit(run())
}

// quietKlog suppresses klog output (used by Kubernetes client libraries),
// which prints "Warning: unrecognized format" and other client warnings
var quietKlog = sync.OnceFunc(func() {
	klog.InitFlags(nil)
	// These flags are registered by klog.InitFlags above, so Set cannot fail
	_ = flag.Set("logtostderr", "false")
	_ = flag.Set("alsologtostderr", "false")
	_ = flag.Set("stderrthreshold", "FATAL")
	flag.Parse()
})

// run runs kraze and returns its exit code
func run() int {
	quietKlog()
	cli.SetVersionInfo(Version, GitCommit, BuildDate)

	if err := cli.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}
