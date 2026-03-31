package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerbose(test *testing.T) {
	// Save original state
	originalVerbose := verbose
	defer func() { verbose = originalVerbose }()

	tests := []struct {
		name         string
		verboseFlag  bool
		format       string
		args         []interface{}
		expectOutput bool
	}{
		{
			name:         "verbose enabled",
			verboseFlag:  true,
			format:       "test message: %s",
			args:         []interface{}{"hello"},
			expectOutput: true,
		},
		{
			name:         "verbose disabled",
			verboseFlag:  false,
			format:       "test message: %s",
			args:         []interface{}{"hello"},
			expectOutput: false,
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			verbose = tt.verboseFlag

			// Verbose function should not panic
			// We can't easily capture stdout in unit tests without more complex setup
			// So we just verify the function doesn't panic
			Verbose(tt.format, tt.args...)
		})
	}
}

func TestGlobalFlags(test *testing.T) {
	// Test that global flags are initialized
	if rootCmd == nil {
		test.Fatal("rootCmd should not be nil")
	}

	// Verify persistent flags exist
	flags := rootCmd.PersistentFlags()

	if flags.Lookup("file") == nil {
		test.Error("--file flag should be registered")
	}

	if flags.Lookup("verbose") == nil {
		test.Error("--verbose flag should be registered")
	}

	if flags.Lookup("dry-run") == nil {
		test.Error("--dry-run flag should be registered")
	}
}

func TestCommandRegistration(test *testing.T) {
	// Verify all commands are registered
	commands := rootCmd.Commands()

	expectedCommands := []string{
		"init",
		"destroy",
		"up",
		"down",
		"status",
		"validate",
		"version",
		"load-image",
	}

	commandMap := make(map[string]bool)
	for _, cmd := range commands {
		commandMap[cmd.Name()] = true
	}

	for _, expected := range expectedCommands {
		if !commandMap[expected] {
			test.Errorf("Command %q should be registered", expected)
		}
	}
}

func TestRootCommand(test *testing.T) {
	if rootCmd == nil {
		test.Fatal("rootCmd should not be nil")
	}

	if rootCmd.Use == "" {
		test.Error("rootCmd.Use should not be empty")
	}

	if rootCmd.Short == "" {
		test.Error("rootCmd.Short should not be empty")
	}

	if rootCmd.Long == "" {
		test.Error("rootCmd.Long should not be empty")
	}
}

func TestInitCommand(test *testing.T) {
	if initCmd == nil {
		test.Fatal("initCmd should not be nil")
	}

	if initCmd.Use == "" {
		test.Error("initCmd.Use should not be empty")
	}

	if initCmd.Short == "" {
		test.Error("initCmd.Short should not be empty")
	}
}

func TestUpCommand(test *testing.T) {
	if upCmd == nil {
		test.Fatal("upCmd should not be nil")
	}

	if upCmd.Use == "" {
		test.Error("upCmd.Use should not be empty")
	}

	// Verify up command flags
	flags := upCmd.Flags()

	if flags.Lookup("wait") == nil {
		test.Error("--wait flag should be registered for up command")
	}

	if flags.Lookup("no-wait") == nil {
		test.Error("--no-wait flag should be registered for up command")
	}

	if flags.Lookup("timeout") == nil {
		test.Error("--timeout flag should be registered for up command")
	}
}

func TestDownCommand(test *testing.T) {
	if downCmd == nil {
		test.Fatal("downCmd should not be nil")
	}

	if downCmd.Use == "" {
		test.Error("downCmd.Use should not be empty")
	}

	// Verify down command flags
	flags := downCmd.Flags()

	if flags.Lookup("keep-crds") == nil {
		test.Error("--keep-crds flag should be registered for down command")
	}
}

func TestStatusCommand(test *testing.T) {
	if statusCmd == nil {
		test.Fatal("statusCmd should not be nil")
	}

	if statusCmd.Use == "" {
		test.Error("statusCmd.Use should not be empty")
	}
}

func TestValidateCommand(test *testing.T) {
	if validateCmd == nil {
		test.Fatal("validateCmd should not be nil")
	}

	if validateCmd.Use == "" {
		test.Error("validateCmd.Use should not be empty")
	}
}

func TestVersionCommand(test *testing.T) {
	if versionCmd == nil {
		test.Fatal("versionCmd should not be nil")
	}

	if versionCmd.Use == "" {
		test.Error("versionCmd.Use should not be empty")
	}
}

func TestLoadImageCommand(test *testing.T) {
	if loadImageCmd == nil {
		test.Fatal("loadImageCmd should not be nil")
	}

	if loadImageCmd.Use == "" {
		test.Error("loadImageCmd.Use should not be empty")
	}

	// Verify it requires at least one argument
	if loadImageCmd.Args == nil {
		test.Error("loadImageCmd.Args should be set to require arguments")
	}
}

func TestResolveConfigFileExplicitFlag(t *testing.T) {
	// Create a temp file to use as the config
	tmp, err := os.CreateTemp("", "kraze-*.yml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmp.Name())
	tmp.Close()

	// Save and restore the global configFile variable
	orig := configFile
	defer func() { configFile = orig }()

	configFile = tmp.Name()
	got, err := resolveConfigFile(rootCmd)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	want, _ := filepath.Abs(tmp.Name())
	if got != want {
		t.Errorf("Expected %q, got %q", want, got)
	}
}

func TestResolveConfigFileCwd(t *testing.T) {
	// Create a temp directory with a kraze.yml file
	dir, err := os.MkdirTemp("", "kraze-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	krazeYml := filepath.Join(dir, "kraze.yml")
	if err := os.WriteFile(krazeYml, []byte("cluster:\n  name: test\n"), 0644); err != nil {
		t.Fatalf("Failed to write kraze.yml: %v", err)
	}

	// Change to the temp directory
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get cwd: %v", err)
	}
	defer os.Chdir(orig) //nolint:errcheck

	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}

	// Ensure -f is not set
	origConfigFile := configFile
	defer func() { configFile = origConfigFile }()
	configFile = ""

	got, err := resolveConfigFile(rootCmd)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	want, _ := filepath.Abs("kraze.yml")
	if got != want {
		t.Errorf("Expected %q, got %q", want, got)
	}
}

func TestDestroyCommand(test *testing.T) {
	if destroyCmd == nil {
		test.Fatal("destroyCmd should not be nil")
	}

	if destroyCmd.Use == "" {
		test.Error("destroyCmd.Use should not be empty")
	}
}
