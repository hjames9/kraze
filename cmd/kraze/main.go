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
// which prints "Warning: unrecognized format" and other client warnings.
// klog's flags go on a private FlagSet that is never parsed: on the global
// one, flag.Parse read kraze's own flags as Go flags, so `kraze -v up` failed
// (klog has its own -v) and `kraze --plain status` was an undefined flag
var quietKlog = sync.OnceFunc(func() {
	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)
	// These flags are registered by klog.InitFlags above, so Set cannot fail
	_ = fs.Set("logtostderr", "false")
	_ = fs.Set("alsologtostderr", "false")
	_ = fs.Set("stderrthreshold", "FATAL")
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
