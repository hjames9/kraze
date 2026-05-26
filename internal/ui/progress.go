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

	_, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || height <= 0 {
		height = 24
	}

	return &InteractiveProgress{out: os.Stdout, termHeight: height}
}

// InteractiveProgress displays service status in-place using cursor-up
// repositioning. When the service list is taller than the terminal it uses a
// viewport: at most termHeight-4 rows are drawn at once, with a summary footer
// showing overall counts. The viewport auto-scrolls to keep newly-active
// services visible. linesWritten is always the number of terminal lines drawn
// in the last redraw, so the cursor-up repositioning stays exact regardless of
// viewport position.
type InteractiveProgress struct {
	out           io.Writer
	services      map[int]*serviceInfo
	operation     string
	total         int
	termHeight    int
	viewportSize  int // rows to show at once (≤ total)
	viewportStart int // index of first visible service
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
	// rows + footer are redrawn, so the viewport is needed only when
	// total+1 > termHeight (service rows + footer don't fit on screen).
	if ip.termHeight > 0 && total+1 > ip.termHeight {
		maxRows := ip.termHeight - 1
		if maxRows < 1 {
			maxRows = 1
		}
		ip.viewportSize = maxRows
	}

	fmt.Fprintf(ip.w(), "\n%s %d service(s)...\n\n", operation, total)
	shown := ip.viewSize()
	for i := 0; i < shown; i++ {
		fmt.Fprintf(ip.w(), "[%d/%d]\n", i+1, total)
	}
	// Footer is always shown as a status bar.
	fmt.Fprintf(ip.w(), "  0 %s  0 done  %d pending\n", strings.ToLower(ip.operation), total)
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
	// Move back to the start of the service block. linesWritten is the exact
	// number of lines we drew last time; cursor-up by that count lands us on
	// the first service line regardless of where the screen has scrolled.
	if ip.linesWritten > 0 {
		fmt.Fprintf(ip.w(), "\033[%dA\r", ip.linesWritten)
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
			fmt.Fprintf(ip.w(), "\r\033[K[%d/%d]\n", i+1, ip.total)
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

		fmt.Fprintf(ip.w(), "\r\033[K[%d/%d] %-20s %s %s  %s\n",
			i+1,
			ip.total,
			svc.name,
			statusIcon,
			statusColor(fmt.Sprintf("%-12s", svc.status)),
			svc.message,
		)
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
	fmt.Fprintf(ip.w(), "\r\033[K  %d %s  %d done  %d pending\n",
		installing, strings.ToLower(ip.operation), done, pending)
	lines++

	extraLines := ip.linesWritten - lines
	if extraLines > 0 {
		for i := 0; i < extraLines; i++ {
			fmt.Fprint(ip.w(), "\r\033[K\n")
		}
		fmt.Fprintf(ip.w(), "\033[%dA", extraLines)
	}
	ip.linesWritten = lines
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
		fmt.Fprintln(ip.w())
	}
	fmt.Fprintf(ip.w(), "%s %s %d service(s) successfully!\n",
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

func (sp *ScrollingProgress) Start(total int, operation string) {
	sp.operation = operation
	sp.total = total
	sp.shownHeaders = make(map[int]bool)
	sp.installing = make(map[int]bool)
	fmt.Fprintf(sp.w(), "%s %d service(s)...\n", operation, total)
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
				fmt.Fprint(sp.w(), "\n")
			}
			fmt.Fprintf(sp.w(), "[%d/%d] %s '%s'...\n", index+1, sp.total, sp.operation, name)
			sp.shownHeaders[index] = true
			sp.installing[index] = true
		}
	case StatusReady:
		delete(sp.installing, index)
		fmt.Fprintf(sp.w(), "%s Service '%s' %s successfully\n",
			color.Checkmark(), name, getOperationPastTense(sp.operation))
	case StatusFailed:
		delete(sp.installing, index)
		fmt.Fprintf(sp.w(), "%s %s Service '%s' failed: %s\n",
			icon, statusColor(string(status)), name, message)
	case StatusSkipped:
		delete(sp.installing, index)
		fmt.Fprintf(sp.w(), "Service '%s' %s, skipping...\n", name, message)
	case StatusWaiting:
		// suppress
	}
}

func (sp *ScrollingProgress) Stop() {}

func (sp *ScrollingProgress) Finish(successCount int) {
	fmt.Fprintf(sp.w(), "\n%s %s %d service(s)!\n",
		color.Checkmark(),
		getOperationPastTense(sp.operation),
		successCount,
	)
}

func (sp *ScrollingProgress) Verbose(format string, args ...interface{}) {
	if sp.verbose {
		sp.mu.Lock()
		fmt.Fprintf(sp.w(), "[VERBOSE] "+format+"\n", args...)
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
