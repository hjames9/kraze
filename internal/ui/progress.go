package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hjames9/kraze/internal/color"
	"github.com/mattn/go-isatty"
	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

// ServiceStatus represents the current status of a service
type ServiceStatus string

const (
	StatusPending      ServiceStatus = "Pending"
	StatusWaiting      ServiceStatus = "Waiting"
	StatusInstalling   ServiceStatus = "Installing"
	StatusUninstalling ServiceStatus = "Uninstalling"
	StatusReady        ServiceStatus = "Ready"
	StatusFailed       ServiceStatus = "Failed"
	StatusSkipped      ServiceStatus = "Skipped"
)

// Status icons for progress display
const (
	IconPending = "⌚"
	IconWaiting = "⌚"
	IconReady   = "✓"
	IconFailed  = "✗"
	IconSkipped = "⊘"
)

// Spinner frames for animated "Installing" status
var spinnerFrames = []string{
	"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏",
}

// ProgressManager handles service installation/uninstallation progress display
type ProgressManager interface {
	Start(total int, operation string)
	UpdateService(index int, name string, status ServiceStatus, message string)
	Finish(successCount int)
	// Stop halts background goroutines. Idempotent.
	Stop()
	Verbose(format string, args ...interface{})
}

// NewProgressManager returns InteractiveProgress when stdout is a terminal,
// otherwise ScrollingProgress. InteractiveProgress uses a viewport so it works
// regardless of how many services there are relative to terminal height.
func NewProgressManager(verbose bool, plain bool, total int) ProgressManager {
	if plain || verbose || !isatty.IsTerminal(os.Stdout.Fd()) {
		return &ScrollingProgress{verbose: verbose, out: os.Stdout}
	}

	fd := int(os.Stdout.Fd())
	sizeFn := func() (int, int, error) { return term.GetSize(fd) }

	width, height, err := sizeFn()
	if err != nil || height <= 0 {
		height = 24
	}
	if err != nil || width <= 0 {
		width = 80
	}

	return &InteractiveProgress{out: os.Stdout, termWidth: width, termHeight: height, sizeFn: sizeFn}
}

// InteractiveProgress displays service status in-place using cursor-up
// repositioning. When the service list is taller than the terminal it uses a
// viewport: at most termHeight-2 rows of services are drawn at once, with a
// summary footer showing overall counts. The viewport auto-scrolls to keep
// newly-active services visible. linesWritten is always the number of terminal
// lines drawn in the last redraw, so the cursor-up repositioning stays exact
// regardless of viewport position.
//
// Two invariants keep the cursor-up arithmetic honest, and violating either one
// spills duplicate rows into the scrollback on every frame:
//   - Height: the drawn block plus the row the cursor rests on afterwards must
//     fit on screen (see applyViewport).
//   - Width: no drawn row may exceed the terminal width, or it wraps onto extra
//     terminal rows that linesWritten doesn't know about (see truncateToWidth).
type InteractiveProgress struct {
	out           io.Writer
	services      map[int]*serviceInfo
	operation     string
	total         int
	termWidth     int
	termHeight    int
	sizeFn        func() (width, height int, err error) // nil ⇒ fixed size (tests)
	viewportSize  int                                   // rows to show at once (≤ total)
	viewportStart int                                   // index of first visible service
	linesWritten  int
	mu            sync.Mutex
	spinnerFrame  int
	spinnerDone   chan bool
	spinnerActive bool
}

func (ip *InteractiveProgress) w() io.Writer {
	if ip.out != nil {
		return ip.out
	}
	return os.Stdout
}

// printf writes progress output. A failed terminal write has no useful recovery,
// so the error is deliberately dropped here rather than at every call site.
func (ip *InteractiveProgress) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(ip.w(), format, args...)
}

type serviceInfo struct {
	name    string
	status  ServiceStatus
	message string
}

// viewSize returns the effective viewport row count. When viewportSize is 0
// (e.g. in tests that construct InteractiveProgress directly) we fall back to
// showing the full service list, preserving the original behaviour.
func (ip *InteractiveProgress) viewSize() int {
	if ip.viewportSize <= 0 {
		return ip.total
	}
	return ip.viewportSize
}

// applyViewport (re)computes viewportSize from the current terminal height.
//
// A redraw writes N lines each terminated by "\n", which leaves the cursor on a
// fresh row *below* the block — so the block needs N+1 rows on screen, not N.
// If it only gets N, the trailing newline scrolls the screen, the next
// cursor-up clamps at row 1 instead of reaching the block's first row, and each
// frame pushes another copy of the top row into the scrollback. With the
// spinner redrawing 10x/second that is dozens of duplicates.
//
// N is viewportSize+1 (service rows + footer), so viewportSize ≤ termHeight-2.
func (ip *InteractiveProgress) applyViewport() {
	if ip.termHeight <= 0 || ip.total+2 <= ip.termHeight {
		ip.viewportSize = 0 // everything fits; show the full list
		ip.viewportStart = 0
		return
	}

	maxRows := ip.termHeight - 2
	if maxRows < 1 {
		maxRows = 1
	}
	ip.viewportSize = maxRows

	if ip.viewportStart+maxRows > ip.total {
		ip.viewportStart = ip.total - maxRows
	}
	if ip.viewportStart < 0 {
		ip.viewportStart = 0
	}
}

// refreshSize re-reads the terminal dimensions so that resizing the window
// mid-run doesn't leave the viewport sized for the old geometry.
func (ip *InteractiveProgress) refreshSize() {
	if ip.sizeFn == nil {
		return
	}
	width, height, err := ip.sizeFn()
	if err != nil {
		return
	}
	if width > 0 {
		ip.termWidth = width
	}
	if height > 0 && height != ip.termHeight {
		ip.termHeight = height
		ip.applyViewport()
	}
}

func (ip *InteractiveProgress) Start(total int, operation string) {
	ip.mu.Lock()
	defer ip.mu.Unlock()

	ip.operation = operation
	ip.total = total
	ip.services = make(map[int]*serviceInfo)
	ip.linesWritten = 0
	ip.spinnerFrame = 0
	ip.spinnerDone = make(chan bool)
	ip.spinnerActive = true
	ip.viewportStart = 0

	// The header ("\n Installing N...\n\n") is printed once and scrolls into
	// terminal history — it is not part of the redrawn area. Only the service
	// rows + footer are redrawn, so the viewport depends on those alone.
	ip.applyViewport()

	ip.printf("\n%s %d service(s)...\n\n", operation, total)
	shown := ip.viewSize()
	for i := 0; i < shown; i++ {
		ip.printf("[%d/%d]\n", i+1, total)
	}
	// Footer is always shown as a status bar.
	ip.printf("  0 %s  0 done  %d pending\n", strings.ToLower(ip.operation), total)
	ip.linesWritten = shown + 1

	go ip.animateSpinner()
}

func (ip *InteractiveProgress) UpdateService(index int, name string, status ServiceStatus, message string) {
	ip.mu.Lock()
	defer ip.mu.Unlock()

	if ip.services[index] == nil {
		ip.services[index] = &serviceInfo{}
	}
	ip.services[index].name = name
	ip.services[index].status = status
	ip.services[index].message = message

	// When a service starts installing outside the current viewport, scroll the
	// viewport to show it (keep it within a half-window from the top).
	vs := ip.viewSize()
	if vs < ip.total && (status == StatusInstalling || status == StatusUninstalling) {
		if index < ip.viewportStart || index >= ip.viewportStart+vs {
			newStart := index - vs/2
			if newStart < 0 {
				newStart = 0
			}
			if newStart+vs > ip.total {
				newStart = ip.total - vs
			}
			ip.viewportStart = newStart
		}
	}

	ip.redraw()
}

func (ip *InteractiveProgress) redraw() {
	ip.refreshSize()

	// Move back to the start of the service block. linesWritten is the exact
	// number of lines we drew last time; cursor-up by that count lands us on
	// the first service line regardless of where the screen has scrolled.
	if ip.linesWritten > 0 {
		ip.printf("\033[%dA\r", ip.linesWritten)
	}

	lines := 0
	vs := ip.viewSize()
	end := ip.viewportStart + vs
	if end > ip.total {
		end = ip.total
	}
	for i := ip.viewportStart; i < end; i++ {
		svc := ip.services[i]
		if svc == nil {
			ip.drawLine(fmt.Sprintf("[%d/%d]", i+1, ip.total))
			lines++
			continue
		}

		var statusIcon string
		if svc.status == StatusInstalling || svc.status == StatusUninstalling {
			statusIcon = ip.getSpinnerIcon()
		} else {
			statusIcon = getStatusIcon(svc.status)
		}
		statusColor := getStatusColor(svc.status)

		ip.drawLine(fmt.Sprintf("[%d/%d] %-20s %s %s  %s",
			i+1,
			ip.total,
			svc.name,
			statusIcon,
			statusColor(fmt.Sprintf("%-12s", svc.status)),
			svc.message,
		))
		lines++
	}

	// Footer summary — always shown as a status bar.
	var pending, installing, done int
	for j := 0; j < ip.total; j++ {
		s := ip.services[j]
		if s == nil {
			pending++
			continue
		}
		switch s.status {
		case StatusPending, StatusWaiting:
			pending++
		case StatusInstalling, StatusUninstalling:
			installing++
		default:
			done++
		}
	}
	ip.drawLine(fmt.Sprintf("  %d %s  %d done  %d pending",
		installing, strings.ToLower(ip.operation), done, pending))
	lines++

	extraLines := ip.linesWritten - lines
	if extraLines > 0 {
		for i := 0; i < extraLines; i++ {
			ip.printf("\r\033[K\n")
		}
		ip.printf("\033[%dA", extraLines)
	}
	ip.linesWritten = lines
}

// drawLine writes one row of the redrawn block: return to column 0, clear the
// row, then emit the content truncated to a single terminal row. Truncation is
// what keeps one logical line equal to one terminal row — a message longer than
// the terminal (a multi-line CrashLoopBackOff error, say) would otherwise wrap
// onto rows that linesWritten never counted, so the next cursor-up would
// undershoot and start overwriting the middle of the block.
func (ip *InteractiveProgress) drawLine(content string) {
	// One column of slack: a row filled to the last column leaves some
	// terminals in a pending-wrap state that a following newline resolves
	// inconsistently.
	ip.printf("\r\033[K%s\n", truncateToWidth(content, ip.termWidth-1))
}

// truncateToWidth cuts s to at most width display columns, counting ANSI escape
// sequences as zero-width and preserving them intact. A width ≤ 0 means the
// terminal size is unknown, in which case s is returned unchanged.
func truncateToWidth(s string, width int) string {
	if width <= 0 || displayWidth(s) <= width {
		return s
	}

	var (
		b        strings.Builder
		cols     int
		sawEsc   bool
		runes    = []rune(s)
		maxCols  = width - 1 // room for the ellipsis
		ellipsis = "…"
	)
	if maxCols < 1 {
		maxCols, ellipsis = width, ""
	}

	for i := 0; i < len(runes); i++ {
		if runes[i] == escByte {
			sawEsc = true
			j := escapeEnd(runes, i)
			b.WriteString(string(runes[i:j]))
			i = j - 1
			continue
		}
		w := runewidth.RuneWidth(runes[i])
		if cols+w > maxCols {
			break
		}
		b.WriteRune(runes[i])
		cols += w
	}

	b.WriteString(ellipsis)
	if sawEsc {
		// The cut may have dropped the sequence that resets the color.
		b.WriteString("\033[0m")
	}
	return b.String()
}

// displayWidth reports the number of terminal columns s occupies, ignoring ANSI
// escape sequences.
func displayWidth(s string) int {
	runes := []rune(s)
	cols := 0
	for i := 0; i < len(runes); i++ {
		if runes[i] == escByte {
			i = escapeEnd(runes, i) - 1
			continue
		}
		cols += runewidth.RuneWidth(runes[i])
	}
	return cols
}

const escByte = '\033'

// escapeEnd returns the index just past the ANSI escape sequence starting at
// runes[start]. Only the CSI form kraze emits (color codes) is recognized; an
// unterminated sequence consumes the rest of the string.
func escapeEnd(runes []rune, start int) int {
	i := start + 1
	if i < len(runes) && runes[i] == '[' {
		i++
	}
	for i < len(runes) {
		// A CSI sequence ends at its first byte in the range @ to ~.
		if runes[i] >= '@' && runes[i] <= '~' {
			return i + 1
		}
		i++
	}
	return len(runes)
}

func (ip *InteractiveProgress) Stop() {
	if ip.spinnerActive {
		ip.spinnerDone <- true
		ip.spinnerActive = false
	}
}

func (ip *InteractiveProgress) Finish(successCount int) {
	ip.Stop()

	ip.mu.Lock()
	defer ip.mu.Unlock()

	if ip.linesWritten > 0 {
		ip.printf("\n")
	}
	ip.printf("%s %s %d service(s) successfully!\n",
		color.Checkmark(),
		ip.operation,
		successCount,
	)
}

func (ip *InteractiveProgress) Verbose(format string, args ...interface{}) {
	// Suppress in interactive mode; use -v for verbose scrolling output.
}

func (ip *InteractiveProgress) animateSpinner() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ip.spinnerDone:
			return
		case <-ticker.C:
			ip.mu.Lock()
			ip.spinnerFrame = (ip.spinnerFrame + 1) % len(spinnerFrames)
			ip.redraw()
			ip.mu.Unlock()
		}
	}
}

func (ip *InteractiveProgress) getSpinnerIcon() string {
	return spinnerFrames[ip.spinnerFrame]
}

// ScrollingProgress uses traditional scrolling output
type ScrollingProgress struct {
	out          io.Writer
	mu           sync.Mutex
	verbose      bool
	operation    string
	total        int
	shownHeaders map[int]bool
	installing   map[int]bool
}

func (sp *ScrollingProgress) w() io.Writer {
	if sp.out != nil {
		return sp.out
	}
	return os.Stdout
}

// printf writes progress output; see InteractiveProgress.printf
func (sp *ScrollingProgress) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(sp.w(), format, args...)
}

func (sp *ScrollingProgress) Start(total int, operation string) {
	sp.operation = operation
	sp.total = total
	sp.shownHeaders = make(map[int]bool)
	sp.installing = make(map[int]bool)
	sp.printf("%s %d service(s)...\n", operation, total)
}

func (sp *ScrollingProgress) UpdateService(index int, name string, status ServiceStatus, message string) {
	icon := getStatusIcon(status)
	statusColor := getStatusColor(status)

	sp.mu.Lock()
	defer sp.mu.Unlock()

	if sp.installing == nil {
		sp.installing = make(map[int]bool)
	}

	switch status {
	case StatusInstalling, StatusUninstalling:
		if !sp.shownHeaders[index] {
			if len(sp.installing) == 0 {
				sp.printf("\n")
			}
			sp.printf("[%d/%d] %s '%s'...\n", index+1, sp.total, sp.operation, name)
			sp.shownHeaders[index] = true
			sp.installing[index] = true
		}
	case StatusReady:
		delete(sp.installing, index)
		sp.printf("%s Service '%s' %s successfully\n",
			color.Checkmark(), name, getOperationPastTense(sp.operation))
	case StatusFailed:
		delete(sp.installing, index)
		sp.printf("%s %s Service '%s' failed: %s\n",
			icon, statusColor(string(status)), name, message)
	case StatusSkipped:
		delete(sp.installing, index)
		sp.printf("Service '%s' %s, skipping...\n", name, message)
	case StatusWaiting:
		// suppress
	}
}

func (sp *ScrollingProgress) Stop() {}

func (sp *ScrollingProgress) Finish(successCount int) {
	sp.printf("\n%s %s %d service(s)!\n",
		color.Checkmark(),
		getOperationPastTense(sp.operation),
		successCount,
	)
}

func (sp *ScrollingProgress) Verbose(format string, args ...interface{}) {
	if sp.verbose {
		sp.mu.Lock()
		sp.printf("[VERBOSE] "+format+"\n", args...)
		sp.mu.Unlock()
	}
}

func getStatusIcon(status ServiceStatus) string {
	switch status {
	case StatusPending:
		return IconPending
	case StatusWaiting:
		return IconWaiting
	case StatusInstalling, StatusUninstalling:
		return spinnerFrames[0]
	case StatusReady:
		return IconReady
	case StatusFailed:
		return IconFailed
	case StatusSkipped:
		return IconSkipped
	default:
		return " "
	}
}

func getStatusColor(status ServiceStatus) func(string) string {
	switch status {
	case StatusReady:
		return func(s string) string { return color.Green(s) }
	case StatusFailed:
		return func(s string) string { return color.Red(s) }
	case StatusWaiting:
		return func(s string) string { return color.Yellow(s) }
	case StatusInstalling, StatusUninstalling:
		return func(s string) string { return color.Cyan(s) }
	default:
		return func(s string) string { return s }
	}
}

func getOperationPastTense(operation string) string {
	switch operation {
	case "Installing":
		return "installed"
	case "Uninstalling":
		return "uninstalled"
	default:
		return "completed"
	}
}
