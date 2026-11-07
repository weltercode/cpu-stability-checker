package cotx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/co"
)

// A new Engine in the same process is NOT new-process evidence: it
// shares memory, handles and timing. These tests crash a real
// SUBPROCESS mid-transaction against a file-backed fake "machine"
// (the current profile lives in a JSON file, so the crashed child
// and the recovering parent genuinely share hardware state), then
// recover from the parent - a different process, exactly the crash
// + reboot shape the recovery contract is for. No real SMU touched.

// --- file-backed fake hardware ------------------------------------

type hwState struct {
	Profile    co.Profile `json:"profile"`
	HardwareID string     `json:"hardware_id"`
}

func loadHW(t *testing.T, path string) hwState {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var hw hwState
	if err := json.Unmarshal(b, &hw); err != nil {
		t.Fatalf("fake hardware corrupted: %v", err)
	}
	return hw
}

func saveHW(t *testing.T, path string, hw hwState) {
	t.Helper()
	b, err := json.Marshal(hw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fileFake is a co.Backend whose state is a file: it survives
// process death. crashAfter names the operation after whose file
// write the process must die (os.Exit), simulating a crash with the
// hardware already changed.
type fileFake struct {
	hwPath     string
	crashAfter string // "apply" | "restore" | ""
}

func (f *fileFake) load() (hwState, error) {
	b, err := os.ReadFile(f.hwPath)
	if err != nil {
		return hwState{}, err
	}
	var hw hwState
	if err := json.Unmarshal(b, &hw); err != nil {
		return hwState{}, err
	}
	return hw, nil
}

func (f *fileFake) save(hw hwState) error {
	b, err := json.Marshal(hw)
	if err != nil {
		return err
	}
	return os.WriteFile(f.hwPath, b, 0o644)
}

func (f *fileFake) crashNow(point string) {
	if f.crashAfter == point {
		os.Exit(7) // die right after the hardware change, mid-transaction
	}
}

func (f *fileFake) Probe(ctx context.Context) (co.Capabilities, error) {
	return co.Capabilities{
		Supported: co.Yes("fake machine"), Read: co.Yes("fake machine"),
		Readback: co.Yes("fake machine"), Apply: co.Unknown("fake machine"),
		RangeKnown: true, RangeMin: -99, RangeMax: 99,
	}, nil
}

func (f *fileFake) Topology(ctx context.Context) (co.Mapping, error) {
	i0, i1 := 0, 1
	return co.Mapping{Quality: "partial", Slots: []co.CoreSlot{
		{ToolID: "core0", SMUIndex: &i0}, {ToolID: "core1", SMUIndex: &i1},
	}}, nil
}

func (f *fileFake) Read(ctx context.Context) (co.Snapshot, error) {
	hw, err := f.load()
	if err != nil {
		return co.Snapshot{}, &co.Error{Kind: co.KindMalformedOutput, Op: "read", Err: err}
	}
	return co.Snapshot{Profile: hw.Profile, Quality: "exact", HardwareID: hw.HardwareID, ReadAt: time.Now()}, nil
}

func (f *fileFake) Validate(p co.Profile) error {
	if len(p.Offsets) == 0 {
		return &co.Error{Kind: co.KindBadProfile, Op: "validate", Err: errors.New("empty profile")}
	}
	for _, o := range p.Offsets {
		if o.Core != "core0" && o.Core != "core1" {
			return &co.Error{Kind: co.KindBadProfile, Op: "validate",
				Err: fmt.Errorf("%q not addressable", o.Core)}
		}
		if o.Value < -99 || o.Value > 99 {
			return &co.Error{Kind: co.KindBadProfile, Op: "validate", Err: errors.New("out of range")}
		}
	}
	return nil
}

func (f *fileFake) Apply(ctx context.Context, p co.Profile) (co.WriteResult, error) {
	hw, err := f.load()
	if err != nil {
		return co.WriteResult{Mutated: co.MutatedUnknown}, err
	}
	hw.Profile = p
	if err := f.save(hw); err != nil {
		return co.WriteResult{Mutated: co.MutatedUnknown}, err
	}
	f.crashNow("apply")
	return co.WriteResult{Mutated: co.MutatedCompleted}, nil
}

func (f *fileFake) Verify(ctx context.Context, p co.Profile) (co.Verification, error) {
	hw, err := f.load()
	if err != nil {
		return co.Verification{}, err
	}
	if profileEqual(hw.Profile, p) {
		return co.Verification{Match: co.Yes("file machine matches")}, nil
	}
	return co.Verification{Match: co.No("file machine differs")}, nil
}

func (f *fileFake) Restore(ctx context.Context, s co.Snapshot) (co.Verification, error) {
	hw, err := f.load()
	if err != nil {
		return co.Verification{}, err
	}
	hw.Profile = s.Profile
	if err := f.save(hw); err != nil {
		return co.Verification{}, err
	}
	f.crashNow("restore")
	return co.Verification{Match: co.Yes("file machine restored")}, nil
}

// --- the crashing subprocess ---------------------------------------

// TestCrashHelperProcess is not a test: with CO_CRASH_AT set it is
// the helper "previous process". Without it, it passes through
// instantly during normal package runs.
func TestCrashHelperProcess(t *testing.T) {
	crash := os.Getenv("CO_CRASH_AT")
	if crash == "" {
		return
	}
	dir := os.Getenv("CO_TX_DIR")
	f := &fileFake{hwPath: filepath.Join(dir, "hardware.json"), crashAfter: crash}
	e := &Engine{
		Backend:   f,
		LockPath:  filepath.Join(dir, "co.lock"),
		StorePath: filepath.Join(dir, "co-tx.json"),
	}
	prof := co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -30}, {Core: "core1", Value: -40}}}
	if _, err := e.Run(context.Background(), prof, nil); err != nil {
		fmt.Fprintf(os.Stderr, "helper: Run returned %v\n", err)
		os.Exit(8)
	}
	os.Exit(0)
}

// --- parent-side scenarios ------------------------------------------

func spawnCrashedHelper(t *testing.T, crashAt string) string {
	t.Helper()
	dir := t.TempDir()
	saveHW(t, filepath.Join(dir, "hardware.json"), hwState{Profile: profA, HardwareID: "fp-machine"})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestCrashHelperProcess$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), "CO_CRASH_AT="+crashAt, "CO_TX_DIR="+dir)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("helper %q: err=%v out=%s", crashAt, err, out)
	}
	return dir
}

func engineOver(dir string) *Engine {
	return &Engine{
		Backend:   &fileFake{hwPath: filepath.Join(dir, "hardware.json")},
		LockPath:  filepath.Join(dir, "co.lock"),
		StorePath: filepath.Join(dir, "co-tx.json"),
	}
}

func TestCrashRecoveryInNewProcess(t *testing.T) {
	t.Run("crashed after apply landed", func(t *testing.T) {
		dir := spawnCrashedHelper(t, "apply")
		hwPath := filepath.Join(dir, "hardware.json")
		if hw := loadHW(t, hwPath); !profileEqual(hw.Profile, profB) {
			t.Fatal("helper did not reach the hardware before dying")
		}
		doc, err := readDoc(filepath.Join(dir, "co-tx.json"))
		if err != nil || doc.Status != StatusApplyPending {
			t.Fatalf("durable intent missing after crash: %+v err=%v", doc, err)
		}
		// This parent IS a different process from the crashed one.
		e := engineOver(dir)
		res, err := e.Recover(context.Background())
		if err != nil {
			t.Fatalf("recovery: %v", err)
		}
		if res.Status != StatusRestored {
			t.Fatalf("status = %s, want restored", res.Status)
		}
		if hw := loadHW(t, hwPath); !profileEqual(hw.Profile, profA) {
			t.Errorf("hardware not restored: %+v", hw.Profile)
		}
		// Retry: terminal, never re-executed, hardware untouched.
		res2, err := e.Recover(context.Background())
		if err != nil || !strings.Contains(strings.Join(res2.Notes, " "), "never re-executed") {
			t.Fatalf("terminal retry: %+v err=%v", res2, err)
		}
		if hw := loadHW(t, hwPath); !profileEqual(hw.Profile, profA) {
			t.Errorf("retry changed the hardware again")
		}
	})

	t.Run("crashed after restore landed", func(t *testing.T) {
		dir := spawnCrashedHelper(t, "restore")
		hwPath := filepath.Join(dir, "hardware.json")
		if hw := loadHW(t, hwPath); !profileEqual(hw.Profile, profA) {
			t.Fatal("helper restore did not land")
		}
		doc, err := readDoc(filepath.Join(dir, "co-tx.json"))
		if err != nil || doc.Status != StatusRestorePending {
			t.Fatalf("durable restore intent missing: %+v err=%v", doc, err)
		}
		e := engineOver(dir)
		res, err := e.Recover(context.Background())
		if err != nil {
			t.Fatalf("recovery: %v", err)
		}
		if res.Status != StatusRestored {
			t.Fatalf("status = %s, want restored", res.Status)
		}
		if !strings.Contains(strings.Join(res.Notes, " "), "restored by observation") {
			t.Errorf("honest observation note missing: %v", res.Notes)
		}
		if hw := loadHW(t, hwPath); !profileEqual(hw.Profile, profA) {
			t.Errorf("hardware drifted during recovery: %+v", hw.Profile)
		}
	})

	t.Run("external profile after the crash", func(t *testing.T) {
		dir := spawnCrashedHelper(t, "apply")
		hwPath := filepath.Join(dir, "hardware.json")
		// A foreign tool moved the profile after the crash:
		// recovery must NOT silently overwrite it.
		saveHW(t, hwPath, hwState{Profile: profX, HardwareID: "fp-machine"})
		e := engineOver(dir)
		_, err := e.Recover(context.Background())
		if !co.IsKind(err, co.KindExternalChange) {
			t.Fatalf("recovery overwrote or accepted an external profile: %v", err)
		}
		if hw := loadHW(t, hwPath); !profileEqual(hw.Profile, profX) {
			t.Errorf("recovery overwrote the external profile: %+v", hw.Profile)
		}
		doc, derr := readDoc(filepath.Join(dir, "co-tx.json"))
		if derr != nil || doc.Status != StatusApplyPending {
			t.Errorf("recovery damaged or rewrote the pending document: %+v err=%v", doc, derr)
		}
	})
}
