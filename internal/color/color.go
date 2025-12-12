package color

import (
	"github.com/fatih/color"
)

// Icon constants for consistent output
const (
	IconCheckmark = "✓"
	IconCross     = "✗"
	IconWarning   = "⚠"
	IconInfo      = "ℹ"
)

var (
	// Green creates green colored text
	Green = color.New(color.FgGreen).SprintFunc()

	// Red creates red colored text
	Red = color.New(color.FgRed).SprintFunc()

	// Yellow creates yellow colored text
	Yellow = color.New(color.FgYellow).SprintFunc()

	// Cyan creates cyan colored text
	Cyan = color.New(color.FgCyan).SprintFunc()

	// Gray creates gray colored text (using bright black/dark gray)
	Gray = color.New(color.FgHiBlack).SprintFunc()

	// Bold creates bold text
	Bold = color.New(color.Bold).SprintFunc()
)

// Checkmark returns a green checkmark
func Checkmark() string {
	return Green(IconCheckmark)
}

// Cross returns a red cross
func Cross() string {
	return Red(IconCross)
}

// Warning returns a yellow warning symbol
func Warning() string {
	return Yellow(IconWarning)
}

// Success formats a success message with a green checkmark
func Success(msg string) string {
	return Checkmark() + " " + msg
}

// Error formats an error message with a red cross
func Error(msg string) string {
	return Cross() + " " + msg
}

// Info formats an info message with a cyan symbol
func Info(msg string) string {
	return Cyan(IconInfo) + " " + msg
}
