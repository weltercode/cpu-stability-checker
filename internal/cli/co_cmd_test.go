package cli

import (
	"path/filepath"
	"testing"
)

// The write-path subcommands must validate their explicit flags and
// fail as setup (exit 2) BEFORE any hardware call: a bounded round-
// trip only happens when -core and a bounded -delta are both given and
// a tool is configured. None of these reach the SMU.
func TestCoVerifyHardwareArgValidation(t *testing.T) {
	chdirTemp(t)
	fakeTool := filepath.Join(t.TempDir(), "ryzen-smu-cli.exe") // path given so the no-tool check passes; never executed
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"missing core", []string{"co", "verify-hardware", "-tool", fakeTool}},
		{"zero delta", []string{"co", "verify-hardware", "-tool", fakeTool, "-core", "core1", "-delta", "0"}},
		{"delta too large", []string{"co", "verify-hardware", "-tool", fakeTool, "-core", "core1", "-delta", "3"}},
		{"delta too negative", []string{"co", "verify-hardware", "-tool", fakeTool, "-core", "core1", "-delta", "-3"}},
	} {
		if code := Run(tc.args); code != 2 {
			t.Errorf("%s: exit %d, want 2", tc.name, code)
		}
	}
}

// No tool configured is a setup failure (exit 2) for both write-path
// subcommands, before any engine work.
func TestCoWritePathNoToolConfigured(t *testing.T) {
	chdirTemp(t) // no config.json -> no co_tool
	if code := Run([]string{"co", "verify-hardware", "-core", "core1", "-delta", "-1"}); code != 2 {
		t.Errorf("verify-hardware with no tool: exit %d, want 2", code)
	}
	if code := Run([]string{"co", "recover"}); code != 2 {
		t.Errorf("recover with no tool: exit %d, want 2", code)
	}
	if code := Run([]string{"co", "receipt"}); code != 2 {
		t.Errorf("receipt with no tool: exit %d, want 2", code)
	}
}

// A configured-but-unusable tool path fails at probe (missing tool),
// still exit 2 and still with zero hardware change: the transaction
// never reaches an apply.
func TestCoVerifyHardwareMissingToolIsSetup(t *testing.T) {
	chdirTemp(t)
	missing := filepath.Join(t.TempDir(), "definitely-not-here.exe")
	if code := Run([]string{"co", "verify-hardware", "-tool", missing, "-core", "core1", "-delta", "-1"}); code != 2 {
		t.Errorf("verify-hardware with a missing tool: exit %d, want 2", code)
	}
}
