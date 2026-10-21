package ui

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestScrollingProgressFinishAndVerbose(t *testing.T) {
	for op, want := range map[string]string{"Installing": "installed 2 service(s)!", "Uninstalling": "uninstalled 2 service(s)!", "Checking": "completed 2 service(s)!"} {
		var out bytes.Buffer
		sp := &ScrollingProgress{out: &out}
		sp.Start(2, op)
		sp.Verbose("hidden %d", 1)
		sp.Stop()
		sp.Finish(2)
		if !strings.Contains(out.String(), want) || strings.Contains(out.String(), "hidden") {
			t.Errorf("%s: output %q, want %q and no verbose line", op, out.String(), want)
		}
	}
	var out bytes.Buffer
	sp := &ScrollingProgress{out: &out, verbose: true}
	sp.Verbose("shown %d", 7)
	if !strings.Contains(out.String(), "[VERBOSE] shown 7") {
		t.Errorf("output %q", out.String())
	}
}

func TestInteractiveProgressFinish(t *testing.T) {
	var out bytes.Buffer
	ip := &InteractiveProgress{out: &out, termWidth: 80, termHeight: 24}
	ip.Start(1, "Installing")
	ip.UpdateService(0, "api", StatusInstalling, "")
	ip.Verbose("never shown")
	ip.UpdateService(0, "api", StatusReady, "Deployed")
	ip.Finish(1)
	ip.Stop() // after Finish: no spinner left to stop
	if !strings.Contains(out.String(), "Installing 1 service(s) successfully!") || strings.Contains(out.String(), "never shown") {
		t.Errorf("output %q", out.String())
	}
}

// Resizing the terminal mid-run resizes the viewport
func TestInteractiveProgressFollowsTerminalSize(t *testing.T) {
	size := [2]int{80, 40}
	var sizeErr error
	ip := &InteractiveProgress{out: &bytes.Buffer{}, termWidth: 80, termHeight: 40,
		sizeFn: func() (int, int, error) { return size[0], size[1], sizeErr }}
	ip.Start(30, "Installing")
	if ip.viewportSize != 0 {
		t.Fatalf("viewport = %d, want 0 (everything fits) on a 40-row terminal", ip.viewportSize)
	}

	size = [2]int{100, 10}
	ip.refreshSize()
	if ip.termWidth != 100 || ip.termHeight != 10 || ip.viewportSize != 8 {
		t.Errorf("after shrinking: %dx%d, viewport %d", ip.termWidth, ip.termHeight, ip.viewportSize)
	}

	// A failed size read changes nothing
	sizeErr = errors.New("not a terminal")
	size = [2]int{1, 1}
	ip.refreshSize()
	if ip.termWidth != 100 || ip.termHeight != 10 {
		t.Errorf("a failed read changed the size to %dx%d", ip.termWidth, ip.termHeight)
	}

	// A terminal too short for any rows still shows one
	sizeErr = nil
	size = [2]int{100, 2}
	ip.refreshSize()
	if ip.viewportSize != 1 {
		t.Errorf("viewport = %d, want 1 on a 2-row terminal", ip.viewportSize)
	}

	// The viewport follows the last active service to the end
	sizeErr = nil
	size = [2]int{100, 10}
	for i := 0; i < 30; i++ {
		ip.UpdateService(i, "svc", StatusReady, "")
	}
	ip.UpdateService(29, "svc", StatusInstalling, "")
	ip.Finish(30)
	if ip.viewportStart+ip.viewportSize != 30 {
		t.Errorf("viewport [%d, %d) should end at the last service", ip.viewportStart, ip.viewportStart+ip.viewportSize)
	}
}

// The spinner redraws on its own while a service is installing
func TestInteractiveProgressSpinnerTicks(t *testing.T) {
	var out lockedBuffer
	ip := &InteractiveProgress{out: &out, termWidth: 80, termHeight: 24}
	ip.Start(1, "Installing")
	ip.UpdateService(0, "api", StatusInstalling, "")
	before := out.Len()
	time.Sleep(250 * time.Millisecond)
	ip.Finish(1)
	if out.Len() <= before {
		t.Error("the spinner should have redrawn")
	}
}

// lockedBuffer is a bytes.Buffer safe to share with the spinner goroutine
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Len()
}

func TestProgressDefaults(t *testing.T) {
	if got := getStatusIcon(ServiceStatus("bogus")); got != " " {
		t.Errorf("an unknown status's icon = %q", got)
	}
	// An unterminated escape sequence runs to the end
	if got := escapeEnd([]rune("\x1b[31"), 0); got != 4 {
		t.Errorf("escapeEnd = %d, want 4", got)
	}
	// Without a writer, output goes to stdout
	if (&InteractiveProgress{}).w() != os.Stdout || (&ScrollingProgress{}).w() != os.Stdout {
		t.Error("the default writer should be stdout")
	}
}

// Shrinking the terminal while viewing the end keeps the viewport inside the
// list, and activity near the top scrolls back to it
func TestInteractiveViewportEdges(t *testing.T) {
	size := [2]int{80, 40}
	ip := &InteractiveProgress{out: &bytes.Buffer{}, termWidth: 80, termHeight: 12,
		sizeFn: func() (int, int, error) { return size[0], size[1], nil }}
	size = [2]int{80, 12}
	ip.Start(30, "Installing")
	ip.UpdateService(29, "last", StatusInstalling, "")
	if ip.viewportStart+ip.viewportSize != 30 {
		t.Fatalf("viewport [%d, +%d) should end at the last service", ip.viewportStart, ip.viewportSize)
	}
	size = [2]int{80, 6}
	ip.refreshSize()
	if ip.viewportStart+ip.viewportSize > 30 {
		t.Errorf("viewport [%d, +%d) runs past the list", ip.viewportStart, ip.viewportSize)
	}
	ip.UpdateService(0, "first", StatusInstalling, "")
	if ip.viewportStart != 0 {
		t.Errorf("viewport start = %d, want 0", ip.viewportStart)
	}
	ip.Finish(30)
}

// Growing the terminal while viewing the end pulls the viewport back so it
// still ends at the last service
func TestInteractiveViewportGrows(t *testing.T) {
	size := [2]int{80, 6}
	ip := &InteractiveProgress{out: &bytes.Buffer{}, termWidth: 80, termHeight: 6,
		sizeFn: func() (int, int, error) { return size[0], size[1], nil }}
	ip.Start(30, "Installing")
	ip.UpdateService(29, "last", StatusInstalling, "")
	size = [2]int{80, 14}
	ip.refreshSize()
	if ip.viewportSize != 12 || ip.viewportStart != 18 {
		t.Errorf("viewport = [%d, +%d), want [18, +12)", ip.viewportStart, ip.viewportSize)
	}
	ip.Finish(30)
}
