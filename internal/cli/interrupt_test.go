//go:build unix

package cli

import (
	"syscall"
	"testing"
	"time"
)

// The first Ctrl-C cancels the command's context, so it can stop cleanly and
// record the run. (A second one exits the process, so it isn't sent here.)
func TestInterruptContextCancelsOnFirstSignal(t *testing.T) {
	// Started inside the capture, so the handler's goroutine sees the
	// redirected stderr (it prints before cancelling, so the restore after
	// ctx.Done is ordered after the print)
	var err error
	out := captureStderr(t, func() {
		ctx, stop := interruptContext()
		defer stop()
		if err = syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Error("the context should be cancelled by SIGINT")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Interrupted, stopping (press Ctrl-C again to force quit)")
}

// Stopping without a signal cancels the context and ends the watcher
func TestInterruptContextStop(t *testing.T) {
	ctx, stop := interruptContext()
	stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Error("stop should cancel the context")
	}
}
