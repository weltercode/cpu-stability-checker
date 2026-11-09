package cli

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

// R15 (owner queue 2026-11-09): a config.json that EXISTS but does not
// parse is this machine's deliberate plan, unread. Every command that
// starts load refuses it outright - the deleted silent Load turned the
// user's intended test into defaults with zero disclosure, and the
// journal, report and 'repeat' then described a run that never was
// the planned one. An absent config is legitimate: defaults, with the
// screen SAYING defaults are in effect.

func writeBrokenConfig(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(config.DefaultPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBrokenConfigRefusesStressEntries(t *testing.T) {
	chdirTemp(t)
	writeBrokenConfig(t)

	// Scripted run: refuse with exit 2, name the parse failure, and
	// above all start NOTHING - no log, no journal, no recipe.
	errOut := captureStderr(t, func() {
		if code := Run([]string{"run", "-test", "general", "-duration", "1s"}); code != 2 {
			t.Errorf("run(broken config) exit = %d, want 2", code)
		}
	})
	if !strings.Contains(errOut, "parse") {
		t.Errorf("refusal must name the parse failure:\n%s", errOut)
	}
	if _, err := os.Stat(journal.Path("cpu-stability.log")); !os.IsNotExist(err) {
		t.Error("a refused run left a journal behind")
	}
	if _, err := os.Stat("cpu-stability.log"); !os.IsNotExist(err) {
		t.Error("a refused run left a log file behind")
	}

	// Menu: the most common entry point refuses before any screen and
	// names the fix (fix the file, remove it, or config init -force).
	errOut = captureStderr(t, func() {
		if code := Run([]string{"menu"}); code != 2 {
			t.Errorf("menu(broken config) exit = %d, want 2", code)
		}
	})
	if !strings.Contains(errOut, "failed to parse") || !strings.Contains(errOut, "config init") {
		t.Errorf("menu refusal lost the fix guidance:\n%s", errOut)
	}

	// Evidence readers must not answer "what ran?" from defaults.
	errOut = captureStderr(t, func() {
		if code := Run([]string{"journal"}); code != 2 {
			t.Errorf("journal(broken config) exit = %d, want 2", code)
		}
	})
	if !strings.Contains(errOut, "configuration error") {
		t.Errorf("journal refusal not stated:\n%s", errOut)
	}
	errOut = captureStderr(t, func() {
		if code := Run([]string{"tools", "status"}); code != 2 {
			t.Errorf("tools status(broken config) exit = %d, want 2", code)
		}
	})
	if !strings.Contains(errOut, "configuration error") {
		t.Errorf("tools status answered from defaults despite an unreadable config:\n%s", errOut)
	}
}

// The other half of the rule: an ABSENT config is defaults, and the
// screen says so instead of leaving the user to wonder which settings
// are in effect.
func TestMissingConfigDisclosesDefaultsInMenu(t *testing.T) {
	chdirTemp(t)
	if _, err := os.Stat(config.DefaultPath); !os.IsNotExist(err) {
		t.Fatal("fixture must start without a config.json")
	}
	out := captureStdoutAndRun(t, func() {
		if code := menuLoop(bufio.NewReader(strings.NewReader("q\n")), config.Default()); code != 0 {
			t.Errorf("menuLoop(back) = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "not found") || !strings.Contains(out, "defaults are in effect") {
		t.Errorf("menu did not disclose that defaults are in effect:\n%s", out)
	}
}

// Defense in depth: even a directly-invoked worker refuses a broken
// config instead of measuring with an empty external registry.
func TestBrokenConfigRefusesMenuCategoryStart(t *testing.T) {
	chdirTemp(t)
	writeBrokenConfig(t)
	// The menu loop itself never reaches a category screen in this
	// fixture (runMenu refused first), which is the point: assert the
	// gate at the entry, not inside the dialog.
	errOut := captureStderr(t, func() {
		if code := Run([]string{"handoff", "-cores", "0,1"}); code != 2 {
			t.Errorf("handoff(broken config) exit = %d, want 2", code)
		}
	})
	if !strings.Contains(errOut, "configuration error") {
		t.Errorf("handoff refusal not stated:\n%s", errOut)
	}
}
