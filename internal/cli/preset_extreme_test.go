package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// R31 (owner 2026-11-12: "можем повторить этот алгоритм, хотя бы
// грубо"). ram-extreme-rough reproduces the PUBLIC TestMem5
// extreme@anta777 algorithm as disclosed: the 31-slot sequence
// VERBATIM, roles mapped to our kernels, deviations stated. These
// pins hold the reproduction honest on two axes at once: the ORDER
// must equal the published one (else "reproduction" is a fiction),
// and the TEXT must never read as a TM5 equivalence or detection
// claim (else "rough" is doing the lying).

// extreme1Sequence is the published config's Test Sequence, copied
// literally (1-based slots). If the preset's slot order ever drifts
// from it, the card's central promise breaks and this pin must fail.
var extreme1Sequence = []int{4, 6, 1, 12, 2, 4, 12, 2, 5, 7, 1, 13, 2, 5, 13, 2, 8, 9, 1, 14, 2, 8, 14, 2, 10, 11, 1, 15, 2, 10, 3}

// extreme1Kernels is the accepted mapping table written out slot by
// slot, independently of how the preset builds it: a silent kernel
// swap under the same slot names would change what each step tests.
var extreme1Kernels = []string{
	"ram-block4",  // slot 1  (test 4,  4 MB block)
	"ram-block4",  // slot 2  (test 6,  4 MB block)
	"ram-block4",  // slot 3  (test 1,  MirrorMove role: short fixed window)
	"ram-block4",  // slot 4  (test 12, 4 MB block)
	"ram-copy",    // slot 5  (test 2,  MirrorMove128 role)
	"ram-block4",  // slot 6  (test 4)
	"ram-block4",  // slot 7  (test 12)
	"ram-copy",    // slot 8  (test 2)
	"ram-address", // slot 9  (test 5,  window, pattern Mode 0)
	"ram-address", // slot 10 (test 7)
	"ram-block4",  // slot 11 (test 1,  MirrorMove role)
	"ram-address", // slot 12 (test 13)
	"ram-copy",    // slot 13 (test 2)
	"ram-address", // slot 14 (test 5)
	"ram-address", // slot 15 (test 13)
	"ram-copy",    // slot 16 (test 2)
	"ram-block4",  // slot 17 (test 8,  4 MB block)
	"ram-block4",  // slot 18 (test 9)
	"ram-block4",  // slot 19 (test 1,  MirrorMove role)
	"ram-block4",  // slot 20 (test 14)
	"ram-copy",    // slot 21 (test 2)
	"ram-block4",  // slot 22 (test 8)
	"ram-block4",  // slot 23 (test 14)
	"ram-copy",    // slot 24 (test 2)
	"ram-random",  // slot 25 (test 10, window, pattern Mode 2)
	"ram-random",  // slot 26 (test 11)
	"ram-block4",  // slot 27 (test 1,  MirrorMove role)
	"ram-random",  // slot 28 (test 15)
	"ram-copy",    // slot 29 (test 2)
	"ram-random",  // slot 30 (test 10)
	"ram-block4",  // slot 31 (test 3,  4 MB block)
}

// extreme1Fixed marks the MirrorMove-role slots (test 1): these carry
// the fixed short NOMINAL deadline; every other slot's window is the
// operator's asked duration.
var extreme1Fixed = map[int]bool{3: true, 11: true, 19: true, 27: true}

func TestRamExtremeRoughSequenceIsThePublishedOne(t *testing.T) {
	p, ok := lookupPreset("ram-extreme-rough")
	if !ok {
		t.Fatal("ram-extreme-rough preset missing")
	}
	if !p.NeedsMemory {
		t.Error("ram-extreme-rough must declare NeedsMemory (refusal before load, never dropped phases)")
	}
	if p.SuggestCycles != 3 {
		t.Errorf("SuggestCycles = %d, want 3 (the published config's Cycles, as an answer default only)", p.SuggestCycles)
	}

	cfg := config.Default()
	first := p.Phases(cfg)
	second := p.Phases(cfg)
	if len(first) != 31 || len(extreme1Kernels) != 31 || len(extreme1Sequence) != 31 {
		t.Fatalf("slot count drift: preset=%d, pins=%d/%d", len(first), len(extreme1Sequence), len(extreme1Kernels))
	}
	for i := range first {
		ph := first[i]
		wantName := fmt.Sprintf("ext-%02d-t%02d", i+1, extreme1Sequence[i])
		if ph.Name != wantName {
			t.Errorf("slot %d name = %q, want %q: the name carries the published test number", i+1, ph.Name, wantName)
		}
		if ph.A != extreme1Kernels[i] {
			t.Errorf("slot %d (test %d) kernel = %q, want %q: the accepted role mapping changed", i+1, extreme1Sequence[i], ph.A, extreme1Kernels[i])
		}
		w, wok := workload.Get(ph.A)
		if !wok || !w.RequiresMemory {
			t.Errorf("slot %d names %q: every step must be a memory-verification kernel", i+1, ph.A)
		}
		if ph.Test != "general" || !ph.PerProcess {
			t.Errorf("slot %d is not a per-process general phase: the preset must ride the ordinary pipeline", i+1)
		}
		if extreme1Fixed[i+1] {
			if ph.Duration == "" {
				t.Errorf("slot %d (MirrorMove role) lost its fixed short window: the short-time ROLE is what reproduces test 1's 20%% weight", i+1)
			}
		} else if ph.Duration != "" {
			t.Errorf("slot %d carries fixed duration %q: full-role slots run the asked duration", i+1, ph.Duration)
		}
		// Static sequence: replay and planhash depend on it.
		if second[i].Name != ph.Name || second[i].A != ph.A || second[i].Duration != ph.Duration {
			t.Errorf("slot %d differs between calls: the sequence is not static", i+1)
		}
	}
}

// The card text is the contract with the operator: it may promise a
// ROUGH reproduction of the published parts, and it must never read
// as equivalence, parity or a detection claim.
func TestRamExtremeRoughCardHonesty(t *testing.T) {
	card, ok := catalog.Get("ram-extreme-rough")
	if !ok {
		t.Fatal("frozen card set lost ram-extreme-rough")
	}
	if card.Shape != catalog.ShapePreset || card.Preset != "ram-extreme-rough" ||
		card.Availability != catalog.Available || !card.NeedsMemory {
		t.Errorf("card identity broken: %+v", card)
	}
	p, _ := lookupPreset("ram-extreme-rough")
	text := strings.ToLower(card.Summary + " " + card.Details + " " + p.Description)
	for _, must := range []string{"verbatim", "internals", "not reproduced", "unmeasured", "nominal deadline"} {
		if !strings.Contains(text, must) {
			t.Errorf("card text lost %q - the rough-reproduction caveats are load-bearing:\n%s", must, text)
		}
	}
	for _, never := range []string{"as strong as", "tm5-class", "better than tm5", "catches everything", "same detection", "equivalent to tm5", "tm5 equivalent"} {
		if strings.Contains(text, never) {
			t.Errorf("card text claims %q - detection parity is an unmeasured, destructive-level question:\n%s", never, text)
		}
	}
}

// drainStdout runs fn with os.Stdout redirected, draining the pipe
// CONCURRENTLY: the 31-slot plan screen is larger than the OS pipe
// buffer, and a synchronous read-after-return (the captureStdout
// pattern) would deadlock once the plan text outgrows the buffer.
func drainStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		buf.ReadFrom(r) //nolint:err // the test verdict comes from buf contents and fn's asserts
		close(done)
	}()
	fn()
	w.Close()
	os.Stdout = old
	<-done
	return buf.String()
}

// Cycles default 3 is an ANSWER (Enter takes it, a typed number
// overrides it, the question stays).
func TestRamExtremeRoughCyclesDefaultIsAnAnswerNotARemovedQuestion(t *testing.T) {
	chdirTemp(t)
	card, ok := catalog.Get("ram-extreme-rough")
	if !ok {
		t.Fatal("frozen card set lost ram-extreme-rough")
	}
	cfg := config.Default()
	cfg.Memory = "64MiB" // never allocated: these flows cancel before load

	out := drainStdout(t, func() {
		code := startPresetProfile(bufio.NewReader(strings.NewReader("\n\nn\n")), newScreenUI(os.Stdout), cfg, card)
		if code != 0 {
			t.Fatalf("confirm-cancel exit = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "Repetitions of the whole phase list [3]") {
		t.Errorf("cycles prompt did not show the suggested default 3:\n%s", out)
	}
	if !strings.Contains(out, "x 3 cycle(s)") {
		t.Errorf("Enter did not take the suggested 3 cycles:\n%s", out)
	}
	if _, err := os.Stat(lastrunPath); !os.IsNotExist(err) {
		t.Error("a cancelled preset saved a recipe")
	}

	out = drainStdout(t, func() {
		code := startPresetProfile(bufio.NewReader(strings.NewReader("\n7\nn\n")), newScreenUI(os.Stdout), cfg, card)
		if code != 0 {
			t.Fatalf("confirm-cancel exit = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "x 7 cycle(s)") {
		t.Errorf("typed cycles did not override the suggestion:\n%s", out)
	}
}

// The dry-run proves the 31-slot preset rides the SAME
// compileScenario pipeline: an ordinary phases-list plan, first and
// last slots visible, the fixed short window shown as what it is.
func TestRamExtremeRoughDryRunRidesTheSamePipeline(t *testing.T) {
	dir := chdirTemp(t)
	body, err := json.Marshal(map[string]string{"duration": "10ms", "memory": "64MiB"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		buf.ReadFrom(r) //nolint:err // the verdict comes from buf contents below
		close(done)
	}()
	code := scenarioCmd([]string{"-preset", "ram-extreme-rough", "-dry-run"})
	w.Close()
	os.Stdout = old
	<-done
	if code != 0 {
		t.Fatalf("ram-extreme-rough dry-run exit = %d, want 0:\n%s", code, buf.String())
	}
	out := buf.String()
	for _, must := range []string{"ext-01-t04", "ext-05-t02", "ext-31-t03"} {
		if !strings.Contains(out, must) {
			t.Errorf("dry-run plan missing %q:\n%s", must, out)
		}
	}
	if !strings.Contains(out, "ext-03-t01") || !strings.Contains(out, "duration=6s") {
		t.Errorf("dry-run plan does not show the fixed short MirrorMove-role window:\n%s", out)
	}
}
