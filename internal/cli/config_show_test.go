package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/config"
)

// R18 (owner queue 2026-11-09): `config` and `config show` answer the
// settings question from ONE shared inspection model. The contract
// below is table-driven on purpose (the shape Sol's review named):
// every file state - absent, partial, malformed, parsed-but-invalid -
// has a defined honest answer, bare and show agree byte-for-byte, and
// unknown subcommands refuse before the configuration is even read.

func TestConfigShowInspectionContract(t *testing.T) {
	const partial = `{"duration":"1m30s","cores":[0,1],"memory":"banana","execution":"nope",` +
		`"external":{"ghost":{"command":"no-such-dir/gone.exe"},"real":{"command":"realtool.exe"}},` +
		`"phases":[{"name":"warm","test":"general","duration":"10ms"},{"test":"general","duration":"10ms"}]}`

	t.Run("absent: defaults said out loud, never a defaults-printout-as-plan", func(t *testing.T) {
		chdirTemp(t)
		code, out := captureAnswer(t, func() int { return configShow() })
		if code != 0 {
			t.Fatalf("configShow(no file) exit = %d, want 0 (absence is legitimate)", code)
		}
		for _, want := range []string{
			"no file there",
			"built-in defaults in effect",
			"config init",       // first-run guidance
			"first run:",        // the guidance is labelled, not whispered
			"values in effect:", // effective values arrive even without a file
			`duration:`, `30s`,
			`memory engine`, `off: no "memory" budget`, // engine off is loud, not silent
			"none registered",
			"none configured", // phases
		} {
			if !strings.Contains(out, want) {
				t.Errorf("absent-state view missing %q:\n%s", want, out)
			}
		}
		// The real path is absolute: the temp dir this ran in must
		// appear, so "config.json" can never mean "some other dir's
		// config" again.
		if !strings.Contains(out, filepathAbsWant(t)) {
			t.Errorf("view must name the real absolute path of the file:\n%s", out)
		}
	})

	t.Run("partial: every value with its meaning, defects named not repaired", func(t *testing.T) {
		chdirTemp(t)
		if err := os.WriteFile("realtool.exe", []byte("not really an exe, just an existing file"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config.DefaultPath, []byte(partial), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out := captureAnswer(t, func() int { return configShow() })
		if code != 0 {
			t.Fatalf("configShow(partial) exit = %d, want 0 (the view answers; the runner is what refuses): %s", code, out)
		}
		for _, want := range []string{
			"present and parsed",
			`1m30s`,                          // the configured duration with its meaning
			"logical CPUs 0,1 as configured", // selector echoed, resolution deferred honestly
			"FILE MISSING", "ghost",          // the missing external tool is named, not dropped
			"[file present]", "real", // the existing one too (never launched)
			"banana", "NOT a valid budget", // semantic defect stated as written
			"nope", "NOT in the vocabulary", // invalid execution word, refused-later named
			"warm", "phase 2 (unnamed)", // phases listed, missing names stated
			"scenario -dry-run", // where compiled runs live (this view does not fork them)
		} {
			if !strings.Contains(out, want) {
				t.Errorf("partial-state view missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("malformed: refusal with reason, no effective values printed", func(t *testing.T) {
		chdirTemp(t)
		if err := os.WriteFile(config.DefaultPath, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		var code int
		errOut := captureStderr(t, func() {
			code = configCmd(nil)
		})
		if code != 2 {
			t.Fatalf("bare config(broken) exit = %d, want 2", code)
		}
		for _, want := range []string{"configuration error", "parse", "present but unreadable"} {
			if !strings.Contains(strings.ToLower(errOut), strings.ToLower(want)) {
				t.Errorf("broken-config refusal missing %q:\n%s", want, errOut)
			}
		}
		// No answer dressed as values: a broken file has none.
		code2, out := captureAnswer(t, func() int { return configCmd([]string{"show"}) })
		if code2 != 2 {
			t.Fatalf("config show(broken) exit = %d, want 2", code2)
		}
		if strings.Contains(out, "values in effect") {
			t.Errorf("show printed effective values over an unreadable config:\n%s", out)
		}
	})

	t.Run("invalid duration: the fallback is stated as a defect, not shown as intent", func(t *testing.T) {
		chdirTemp(t)
		if err := os.WriteFile(config.DefaultPath, []byte(`{"duration":"30x"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out := captureAnswer(t, func() int { return configShow() })
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (state + named defect): %s", code, out)
		}
		if !strings.Contains(out, `"30x" as written is NOT parseable`) || !strings.Contains(out, "falls back to 30s") {
			t.Errorf("the duration defect must name both the written value and the documented fallback:\n%s", out)
		}
	})
}

// The single-model promise: bare and show are not two renderers that
// could drift - they answer identically, in every file state.
func TestConfigBareAndShowAreTheSameView(t *testing.T) {
	states := []struct {
		name string
		body string // "" leaves no file at all
	}{
		{"absent", ""},
		{"present", `{"duration":"45s","memory":"2GiB","memory_lock":"try"}`},
		{"broken", "{not json"},
	}
	for _, st := range states {
		t.Run(st.name, func(t *testing.T) {
			chdirTemp(t)
			if st.body != "" {
				if err := os.WriteFile(config.DefaultPath, []byte(st.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			bareCode, bareOut := captureAnswer(t, func() int { return configCmd(nil) })
			showCode, showOut := captureAnswer(t, func() int { return configCmd([]string{"show"}) })
			if bareCode != showCode {
				t.Errorf("bare exit %d != show exit %d (state %s)", bareCode, showCode, st.name)
			}
			if bareOut != showOut {
				t.Errorf("bare and show drifted apart (state %s):\nbare:\n%s\nshow:\n%s", st.name, bareOut, showOut)
			}
		})
	}
}

// Unknown subcommands refuse BEFORE the file is read, whatever its
// state: the answer must not leak from a command nobody typed, and a
// broken file must not turn a typo into a JSON dump (the pre-R18 bug).
func TestConfigUnknownSubcommandsRefuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		args []string
	}{
		{"typo, no file", "", []string{"frobnicate"}},
		{"typo over a good file", `{"duration":"5s"}`, []string{"frobnicate"}},
		{"typo over a broken file", "{not json", []string{"frobnicate"}},
		{"show with junk tail", "{not json", []string{"show", "--please"}},
		{"bare with junk tail", `{"duration":"5s"}`, []string{"--dump"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chdirTemp(t)
			if tc.body != "" {
				if err := os.WriteFile(config.DefaultPath, []byte(tc.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var code int
			errOut := captureStderr(t, func() { code = configCmd(tc.args) })
			if code != 2 {
				t.Fatalf("config %v exit = %d, want 2", tc.args, code)
			}
			if !strings.Contains(errOut, "unknown argument") {
				t.Errorf("refusal must name the unknown argument:\n%s", errOut)
			}
			// Nothing about the file's contents leaked before the
			// refusal: the only honest answer to an unknown command
			// is the refusal itself.
			if strings.Contains(errOut, "values in effect") || strings.Contains(errOut, "built-in defaults in effect") {
				t.Errorf("an unknown subcommand received the settings answer:\n%s", errOut)
			}
		})
	}
}

// filepathAbsWant resolves the temp dir we are sitting in, so the
// assertion "the view printed an absolute path" is checked against the
// REAL directory, not a guessed one.
func filepathAbsWant(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}
