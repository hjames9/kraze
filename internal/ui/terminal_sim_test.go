package ui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

// termSim is a minimal terminal emulator: a fixed grid plus the handful of
// control sequences the progress display emits. It exists because every bug
// this file has had was invisible to assertions about kraze's own bookkeeping —
// linesWritten can be perfectly self-consistent while the screen scrolls out
// from under it. Rendering the output and inspecting the resulting screen and
// scrollback is the only way to catch that.
type termSim struct {
	cols, rows int
	grid       [][]rune
	row, col   int
	scrollback []string
	wrapped    int // times a write ran past the last column
}

func newTermSim(cols, rows int) *termSim {
	ts := &termSim{cols: cols, rows: rows}
	ts.grid = make([][]rune, rows)
	for i := range ts.grid {
		ts.grid[i] = blankRow(cols)
	}
	return ts
}

func blankRow(cols int) []rune {
	r := make([]rune, cols)
	for i := range r {
		r[i] = ' '
	}
	return r
}

func (ts *termSim) Write(p []byte) (int, error) {
	runes := []rune(string(p))
	for i := 0; i < len(runes); i++ {
		switch r := runes[i]; r {
		case '\033':
			i = ts.applyEscape(runes, i)
		case '\r':
			ts.col = 0
		case '\n':
			ts.lineFeed()
		default:
			ts.put(r)
		}
	}
	return len(p), nil
}

// applyEscape handles the CSI sequences the display uses and returns the index
// of the sequence's final byte. The scanner is deliberately independent of the
// one in progress.go — a shared parser could hide a bug in either.
func (ts *termSim) applyEscape(runes []rune, start int) int {
	end := simEscapeEnd(runes, start)
	seq := string(runes[start:end])
	final := runes[end-1]

	switch final {
	case 'A': // cursor up, clamped at the top of the screen like a real terminal
		n := parseParam(seq)
		ts.row -= n
		if ts.row < 0 {
			ts.row = 0
		}
	case 'K': // erase from the cursor to the end of the row
		for c := ts.col; c < ts.cols; c++ {
			ts.grid[ts.row][c] = ' '
		}
	case 'm': // color; no effect on layout
	}
	return end - 1
}

// simEscapeEnd returns the index just past the CSI sequence starting at
// runes[start].
func simEscapeEnd(runes []rune, start int) int {
	i := start + 1
	if i < len(runes) && runes[i] == '[' {
		i++
	}
	for i < len(runes) {
		if runes[i] >= '@' && runes[i] <= '~' {
			return i + 1
		}
		i++
	}
	return len(runes)
}

func parseParam(seq string) int {
	digits := strings.TrimLeft(strings.TrimRight(seq[1:], "ABCDKm"), "[")
	n := 0
	for _, d := range digits {
		if d < '0' || d > '9' {
			return 1
		}
		n = n*10 + int(d-'0')
	}
	if n == 0 {
		return 1
	}
	return n
}

func (ts *termSim) lineFeed() {
	ts.col = 0
	ts.row++
	if ts.row >= ts.rows {
		ts.scroll()
		ts.row = ts.rows - 1
	}
}

// scroll pushes the top row into the scrollback, exactly where the user sees
// the duplicated rows when the redraw arithmetic is wrong.
func (ts *termSim) scroll() {
	ts.scrollback = append(ts.scrollback, strings.TrimRight(string(ts.grid[0]), " "))
	copy(ts.grid, ts.grid[1:])
	ts.grid[ts.rows-1] = blankRow(ts.cols)
}

func (ts *termSim) put(r rune) {
	w := runewidth.RuneWidth(r)
	if w == 0 {
		return
	}
	if ts.col+w > ts.cols {
		ts.wrapped++
		ts.lineFeed()
	}
	ts.grid[ts.row][ts.col] = r
	for i := 1; i < w; i++ {
		if ts.col+i < ts.cols {
			ts.grid[ts.row][ts.col+i] = ' '
		}
	}
	ts.col += w
}

func (ts *termSim) screen() []string {
	out := make([]string, ts.rows)
	for i, row := range ts.grid {
		out[i] = strings.TrimRight(string(row), " ")
	}
	return out
}

var serviceRowRe = regexp.MustCompile(`^\[\d+/\d+\]`)

// TestInteractiveProgressScrollbackStaysClean renders a full install through a
// terminal emulator and asserts the service block never scrolls off the top.
//
// This is the regression test for the reported bug: with 37 services on a
// 38-row terminal the block exactly filled the screen, so every one of the
// spinner's 10-per-second redraws pushed another copy of row 1 into the
// scrollback. It also covers the second cause — a failure message longer than
// the terminal wrapping onto uncounted rows — via the oversized ripster error.
func TestInteractiveProgressScrollbackStaysClean(t *testing.T) {
	const longErr = "failed waiting for resources: error waiting for Deployment/ripster: " +
		"Deployment has failing pod ripster-5c79577886-qc429: Container 'ripster': " +
		"CrashLoopBackOff - back-off 10s restarting failed container=ripster"

	geometries := []struct{ cols, rows int }{
		{100, 38}, // reported case: block exactly fills the screen
		{100, 60}, // list fits with room to spare — isolates the wrapping cause
		{80, 24},  // default terminal
		{60, 12},  // viewport much smaller than the list
		{200, 80}, // wide and tall
	}

	for _, g := range geometries {
		t.Run(strings.Join([]string{itoa(g.cols), itoa(g.rows)}, "x"), func(t *testing.T) {
			const total = 37
			sim := newTermSim(g.cols, g.rows)
			ip := &InteractiveProgress{out: sim, termWidth: g.cols, termHeight: g.rows}

			ip.Start(total, "Installing")
			ip.Stop() // drive frames synchronously instead of via the goroutine

			for i := 0; i < total; i++ {
				name := "service-" + itoa(i)
				ip.UpdateService(i, name, StatusInstalling, "")
				for f := 0; f < 3; f++ { // stand in for spinner animation frames
					ip.mu.Lock()
					ip.spinnerFrame = (ip.spinnerFrame + 1) % len(spinnerFrames)
					ip.redraw()
					ip.mu.Unlock()
				}
				if i == 32 {
					ip.UpdateService(i, "ripster", StatusFailed, longErr)
					continue
				}
				ip.UpdateService(i, name, StatusReady, "Deployed")
			}

			// The block is redrawn in place, so only the header may scroll away.
			// Any service row reaching the scrollback is the duplication bug.
			for _, line := range sim.scrollback {
				if serviceRowRe.MatchString(line) {
					t.Fatalf("service row scrolled into history (%d such rows): %q",
						countMatching(sim.scrollback, serviceRowRe), line)
				}
			}

			// No row may run past the last column: a wrapped row consumes a
			// terminal line that linesWritten never counted.
			if sim.wrapped > 0 {
				t.Errorf("output wrapped %d times on a %d-column terminal", sim.wrapped, g.cols)
			}

			// The visible block must be intact: one row per visible service,
			// each distinct, plus the footer.
			screen := sim.screen()
			rows := countMatching(screen, serviceRowRe)
			wantRows := ip.viewSize()
			if wantRows > total {
				wantRows = total
			}
			if rows != wantRows {
				t.Errorf("screen shows %d service rows, want %d", rows, wantRows)
			}
			seen := map[string]bool{}
			for _, line := range screen {
				if !serviceRowRe.MatchString(line) {
					continue
				}
				idx := serviceRowRe.FindString(line)
				if seen[idx] {
					t.Errorf("service row %s drawn twice on screen", idx)
				}
				seen[idx] = true
			}
			if footers := countMatching(screen, regexp.MustCompile(`\d+ done`)); footers != 1 {
				t.Errorf("found %d footer rows, want exactly 1", footers)
			}
		})
	}
}

func countMatching(lines []string, re *regexp.Regexp) int {
	n := 0
	for _, l := range lines {
		if re.MatchString(l) {
			n++
		}
	}
	return n
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
