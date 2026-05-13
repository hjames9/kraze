package ui

import (
	"fmt"
	"io"
	"os"
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

// NewProgressManager returns InteractiveProgress when stdout is a terminal and
// the display fits, otherwise ScrollingProgress.
func NewProgressManager(verbose bool, plain bool, total int) ProgressManager {
	if plain || verbose || !isatty.IsTerminal(os.Stdout.Fd()) {
		return &ScrollingProgress{verbose: verbose, out: os.Stdout}
	}

	if total > 0 {
		_, height, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil || height <= 0 || total+3 >= height {
			return &ScrollingProgress{verbose: verbose, out: os.Stdout}
		}
	}

	return &InteractiveProgress{out: os.Stdout}
}

// ipServiceStartRow is the absolute row (1-indexed) of the [1/N] service line
// within the cleared screen. Layout after \033[2J\033[H:
//
//	row 1: blank          (from leading \n in header format)
//	row 2: "Installing N service(s)..."
//	row 3: blank
//	row 4: [1/N]          ← ipServiceStartRow
//	...
//	row 4+N-1: [N/N]
const ipServiceStartRow = 4

// InteractiveProgress clears the main screen and displays service status
// in-place using absolute cursor positioning. Because the screen is cleared
// before the display starts, ipServiceStartRow=4 is always exact — no scroll
// region is needed and no cursor arithmetic can drift. The main screen is used
// (not the alternate buffer), so the user can scroll up at any time to see
// prior output that was pushed into the terminal scrollback by the clear.
type InteractiveProgress struct {
	out           io.Writer
	services      map[int]*serviceInfo
	operation     string
	total         int
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

	// Clear the visible screen and move cursor to the top-left.
	// \033[2J clears only the visible screen, not the scrollback buffer, so the
	// user can still scroll up to see cluster creation output. No blank lines
	// are introduced into the output stream (unlike a newline-based pre-scroll).
	fmt.Fprint(ip.w(), "\033[2J\033[H")

	fmt.Fprintf(ip.w(), "\n%s %d service(s)...\n\n", operation, total)
	for i := 0; i < total; i++ {
		fmt.Fprintf(ip.w(), "[%d/%d]\n", i+1, total)
	}
	ip.linesWritten = total

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

	ip.redraw()
}

func (ip *InteractiveProgress) redraw() {
	// Jump directly to the first service line. Because we cleared the screen
	// before starting, this row is always correct regardless of terminal height
	// or prior output — no cursor-up arithmetic can drift.
	fmt.Fprintf(ip.w(), "\033[%d;1H", ipServiceStartRow)

	lines := 0
	for i := 0; i < ip.total; i++ {
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
