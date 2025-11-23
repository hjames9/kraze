package ui

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/hjames9/kraze/internal/color"
	"github.com/mattn/go-isatty"
)

// ServiceStatus represents the current status of a service
type ServiceStatus string

const (
	StatusPending    ServiceStatus = "Pending"
	StatusWaiting    ServiceStatus = "Waiting"
	StatusInstalling ServiceStatus = "Installing"
	StatusReady      ServiceStatus = "Ready"
	StatusFailed     ServiceStatus = "Failed"
	StatusSkipped    ServiceStatus = "Skipped"
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
	// Start initializes the progress display with total service count
	Start(total int, operation string)

	// UpdateService updates the status and message for a specific service
	UpdateService(index int, name string, status ServiceStatus, message string)

	// Finish completes the progress display and shows summary
	Finish(successCount int)

	// Verbose prints a verbose message (only in scrolling mode)
	Verbose(format string, args ...interface{})
}

// NewProgressManager creates the appropriate progress manager based on terminal detection
func NewProgressManager(verbose bool, plain bool) ProgressManager {
	// Use scrolling mode if:
	// - plain flag is set (user wants scrolling mode)
	// - verbose flag is set (verbose implies scrolling)
	// - stdout is not a terminal (piped/redirected)
	if plain || verbose || !isatty.IsTerminal(os.Stdout.Fd()) {
		return &ScrollingProgress{verbose: verbose}
	}

	return &InteractiveProgress{}
}

// InteractiveProgress uses ANSI escape codes for in-place terminal updates
type InteractiveProgress struct {
	services      map[int]*serviceInfo
	operation     string
	total         int
	linesWritten  int
	mu            sync.Mutex
	spinnerFrame  int
	spinnerDone   chan bool
	spinnerActive bool
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

	fmt.Printf("\n%s %d service(s)...\n\n", operation, total)

	// Start spinner animation in background
	go ip.animateSpinner()
}

func (ip *InteractiveProgress) UpdateService(index int, name string, status ServiceStatus, message string) {
	ip.mu.Lock()
	defer ip.mu.Unlock()

	// Initialize service info if needed
	if ip.services[index] == nil {
		ip.services[index] = &serviceInfo{}
	}

	ip.services[index].name = name
	ip.services[index].status = status
	ip.services[index].message = message

	// Redraw the entire status
	ip.redraw()
}

func (ip *InteractiveProgress) redraw() {
	// Move cursor up to beginning of our display area
	if ip.linesWritten > 0 {
		fmt.Printf("\033[%dA", ip.linesWritten) // Move up N lines
	}

	lines := 0

	// Show all services (including pending)
	for i := 0; i < ip.total; i++ {
		svc := ip.services[i]
		if svc == nil {
			continue
		}

		// Clear line and print service status
		var statusIcon string
		if svc.status == StatusInstalling {
			statusIcon = ip.getSpinnerIcon()
		} else {
			statusIcon = getStatusIcon(svc.status)
		}
		statusColor := getStatusColor(svc.status)

		// Format: [1/14] redis      ✓ Ready       Deployed
		// Use \r to go to start of line, then clear to end, then print
		fmt.Printf("\r\033[K[%d/%d] %-20s %s %s  %s\n",
			i+1,
			ip.total,
			svc.name,
			statusIcon,
			statusColor(fmt.Sprintf("%-12s", svc.status)),
			svc.message,
		)
		lines++
	}

	// Clear any extra lines from previous render
	extraLines := ip.linesWritten - lines
	if extraLines > 0 {
		for i := 0; i < extraLines; i++ {
			fmt.Print("\r\033[K\n") // Clear line
		}
		// Move cursor back up to the bottom of our actual display
		fmt.Printf("\033[%dA", extraLines)
	}

	ip.linesWritten = lines

	// Leave cursor at bottom of display (don't move back up)
	// This way any unexpected output will appear below our display
}

func (ip *InteractiveProgress) Finish(successCount int) {
	// Stop spinner animation first (before acquiring lock)
	if ip.spinnerActive {
		ip.spinnerDone <- true
		ip.spinnerActive = false
	}

	ip.mu.Lock()
	defer ip.mu.Unlock()

	// Just move cursor past the display area (services are already shown)
	// No need to clear anything
	if ip.linesWritten > 0 {
		// Cursor is already at the bottom, just add a blank line
		fmt.Println()
	}

	fmt.Printf("%s %s %d service(s) successfully!\n",
		color.Checkmark(),
		ip.operation,
		successCount,
	)
}

func (ip *InteractiveProgress) Verbose(format string, args ...interface{}) {
	// Interactive mode doesn't show verbose messages (they'd interfere with in-place updates)
	// User should use -v flag to get scrolling mode if they want verbose output
}

// animateSpinner runs in background and updates spinner frame every 100ms
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

// getSpinnerIcon returns the current spinner frame
func (ip *InteractiveProgress) getSpinnerIcon() string {
	return spinnerFrames[ip.spinnerFrame]
}

// ScrollingProgress uses traditional scrolling output
type ScrollingProgress struct {
	verbose      bool
	operation    string
	total        int
	shownHeaders map[int]bool // Track if we've shown the main header for each service
}

func (sp *ScrollingProgress) Start(total int, operation string) {
	sp.operation = operation
	sp.total = total
	sp.shownHeaders = make(map[int]bool)
	fmt.Printf("%s %d service(s)...\n", operation, total)
}

func (sp *ScrollingProgress) UpdateService(index int, name string, status ServiceStatus, message string) {
	icon := getStatusIcon(status)
	statusColor := getStatusColor(status)

	switch status {
	case StatusInstalling:
		// Only print the main header once (first time we transition to Installing)
		if !sp.shownHeaders[index] {
			fmt.Printf("\n[%d/%d] %s '%s'...\n", index+1, sp.total, sp.operation, name)
			sp.shownHeaders[index] = true
		}
		// Don't print sub-messages - they clutter the output
		// Verbose messages will be printed via progress.Verbose() calls
	case StatusReady:
		fmt.Printf("%s Service '%s' %s successfully\n",
			color.Checkmark(),
			name,
			getOperationPastTense(sp.operation),
		)
	case StatusFailed:
		fmt.Printf("%s %s Service '%s' failed: %s\n",
			icon,
			statusColor(string(status)),
			name,
			message,
		)
	case StatusSkipped:
		fmt.Printf("Service '%s' %s, skipping...\n", name, message)
	case StatusWaiting:
		// Don't print waiting messages - they clutter the output
		// Verbose messages will be printed via progress.Verbose() calls
	}
}

func (sp *ScrollingProgress) Finish(successCount int) {
	fmt.Printf("\n%s %s %d service(s)!\n",
		color.Checkmark(),
		getOperationPastTense(sp.operation),
		successCount,
	)
}

func (sp *ScrollingProgress) Verbose(format string, args ...interface{}) {
	if sp.verbose {
		fmt.Printf("[VERBOSE] "+format+"\n", args...)
	}
}

// Helper functions

func getStatusIcon(status ServiceStatus) string {
	switch status {
	case StatusPending:
		return IconPending
	case StatusWaiting:
		return IconWaiting
	case StatusInstalling:
		return spinnerFrames[0] // Use first frame for static display (scrolling mode)
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
	case StatusInstalling:
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
