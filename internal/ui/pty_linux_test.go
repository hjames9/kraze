//go:build linux

package ui

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// openPty returns the terminal end of a new pseudo-terminal of the given size
func openPty(t *testing.T, rows, cols uint16) *os.File {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("can't unlock the pseudo-terminal: %v", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skipf("can't name the pseudo-terminal: %v", err)
	}
	tty, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR, 0)
	if err != nil {
		t.Skipf("can't open the pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { _ = tty.Close() })
	if err := unix.IoctlSetWinsize(int(tty.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols}); err != nil {
		t.Fatal(err)
	}
	return tty
}

// On a terminal, progress is drawn in place at the terminal's size; a
// terminal that reports no size gets 80x24
func TestNewProgressManagerOnTerminal(t *testing.T) {
	for name, tc := range map[string]struct {
		rows, cols    uint16
		width, height int
	}{
		"sized":   {30, 100, 100, 30},
		"no size": {0, 0, 80, 24},
	} {
		t.Run(name, func(t *testing.T) {
			tty := openPty(t, tc.rows, tc.cols)
			orig := os.Stdout
			os.Stdout = tty
			t.Cleanup(func() { os.Stdout = orig })

			ip, ok := NewProgressManager(false, false, 3).(*InteractiveProgress)
			if !ok {
				t.Fatal("a terminal should get the interactive display")
			}
			if ip.termWidth != tc.width || ip.termHeight != tc.height {
				t.Errorf("size = %dx%d, want %dx%d", ip.termWidth, ip.termHeight, tc.width, tc.height)
			}
			if w, h, err := ip.sizeFn(); err != nil || w != int(tc.cols) || h != int(tc.rows) {
				t.Errorf("sizeFn = %d, %d, %v", w, h, err)
			}
		})
	}
}
