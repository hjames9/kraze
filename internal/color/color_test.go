package color

import (
	"strings"
	"testing"
)

func TestCheckmark(test *testing.T) {
	result := Checkmark()
	if result == "" {
		test.Error("Checkmark() returned empty string")
	}
	// Check that it contains the checkmark symbol
	if !strings.Contains(result, "✓") {
		test.Error("Checkmark() does not contain checkmark symbol")
	}
}

func TestCross(test *testing.T) {
	result := Cross()
	if result == "" {
		test.Error("Cross() returned empty string")
	}
	// Check that it contains the cross symbol
	if !strings.Contains(result, "✗") {
		test.Error("Cross() does not contain cross symbol")
	}
}

func TestWarning(test *testing.T) {
	result := Warning()
	if result == "" {
		test.Error("Warning() returned empty string")
	}
	// Check that it contains the warning symbol
	if !strings.Contains(result, "⚠") {
		test.Error("Warning() does not contain warning symbol")
	}
}

func TestSuccess(test *testing.T) {
	msg := "Test message"
	result := Success(msg)
	if result == "" {
		test.Error("Success() returned empty string")
	}
	// Check that it contains both the checkmark and the message
	if !strings.Contains(result, "✓") {
		test.Error("Success() does not contain checkmark symbol")
	}
	if !strings.Contains(result, msg) {
		test.Errorf("Success() does not contain message: %s", msg)
	}
}

func TestError(test *testing.T) {
	msg := "Test error"
	result := Error(msg)
	if result == "" {
		test.Error("Error() returned empty string")
	}
	// Check that it contains both the cross and the message
	if !strings.Contains(result, "✗") {
		test.Error("Error() does not contain cross symbol")
	}
	if !strings.Contains(result, msg) {
		test.Errorf("Error() does not contain message: %s", msg)
	}
}

func TestInfo(test *testing.T) {
	msg := "Test info"
	result := Info(msg)
	if result == "" {
		test.Error("Info() returned empty string")
	}
	// Check that it contains both the info symbol and the message
	if !strings.Contains(result, "ℹ") {
		test.Error("Info() does not contain info symbol")
	}
	if !strings.Contains(result, msg) {
		test.Errorf("Info() does not contain message: %s", msg)
	}
}

func TestGreen(test *testing.T) {
	text := "green text"
	result := Green(text)
	if result == "" {
		test.Error("Green() returned empty string")
	}
	if !strings.Contains(result, text) {
		test.Errorf("Green() does not contain text: %s", text)
	}
}

func TestRed(test *testing.T) {
	text := "red text"
	result := Red(text)
	if result == "" {
		test.Error("Red() returned empty string")
	}
	if !strings.Contains(result, text) {
		test.Errorf("Red() does not contain text: %s", text)
	}
}

func TestYellow(test *testing.T) {
	text := "yellow text"
	result := Yellow(text)
	if result == "" {
		test.Error("Yellow() returned empty string")
	}
	if !strings.Contains(result, text) {
		test.Errorf("Yellow() does not contain text: %s", text)
	}
}

func TestCyan(test *testing.T) {
	text := "cyan text"
	result := Cyan(text)
	if result == "" {
		test.Error("Cyan() returned empty string")
	}
	if !strings.Contains(result, text) {
		test.Errorf("Cyan() does not contain text: %s", text)
	}
}

func TestBold(test *testing.T) {
	text := "bold text"
	result := Bold(text)
	if result == "" {
		test.Error("Bold() returned empty string")
	}
	if !strings.Contains(result, text) {
		test.Errorf("Bold() does not contain text: %s", text)
	}
}
