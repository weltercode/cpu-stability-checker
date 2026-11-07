package ryzensmucli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/co"
)

// fakeRunner replays scripted (stdout, exit, err) triples in call
// order and records every invocation, so tests pin BOTH the parsing
// of pinned output shapes and the exact args the adapter passes.
type fakeRunner struct {
	calls []([]string)
	outs  []string
	codes []int
	errs  []error
	i     int
}

func (r *fakeRunner) Run(ctx context.Context, exe string, args ...string) (string, int, error) {
	call := append([]string{exe}, args...)
	r.calls = append(r.calls, call)
	i := r.i
	if i >= len(r.outs) {
		i = len(r.outs) - 1 // the last scripted answer repeats
	} else {
		r.i++
	}
	if i < 0 {
		return "", 0, nil
	}
	var err error
	if i < len(r.errs) {
		err = r.errs[i]
	}
	code := 0
	if i < len(r.codes) {
		code = r.codes[i]
	}
	return r.outs[i], code, err
}

func tempExe(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "ryzen-smu-cli.exe")
	if err := os.WriteFile(p, []byte("MZ-fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The apphost convention pairs a managed payload beside the
	// launcher (audit C02). The fixture writes one too, so adapters
	// that digest real files measure a payload that exists - it is
	// still NOT the pinned payload, keeping such fixtures UNVALIDATED
	// while the fingerprint stays complete.
	if err := os.WriteFile(strings.TrimSuffix(p, ".exe")+".dll", []byte("MZ-fixture-payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const phys16 = `Fused status of physical cores:
Core 0: Enabled
Core 1: Enabled
Core 2: Enabled
`

const phys2 = `Fused status of physical cores:
Core 0: Enabled
Core 1: Enabled
`

// verOK is the MEASURED --version output of the pinned 0.1.3 build
// (read-only smoke on real hardware, 2026-10-18): the bare .NET
// informational version with its embedded SourceLink commit, which
// equals the pinned commit. The old fixture shape "ryzen-smu-cli
// 0.1.3" was inferred and never matched the real build.
const verOK = "0.1.3+39413e27658223b27470196ed0f2b35c1d966899\n"

func adapter(t *testing.T, r *fakeRunner) *Adapter {
	t.Helper()
	return New(tempExe(t), r)
}

// pinBuild presents the pinned launcher AND payload digests through
// the digest seams: the fixture then stands for the validated 0.1.3
// install without shipping the real files. Both axes must be pinned
// for anything to write at all (audit C02): installCurrent re-checks
// launcher and payload against the manifest immediately before the
// only mutating tool call.
func pinBuild(a *Adapter) {
	a.exeSHA = func(string) (string, error) { return pinnedProvenance.ExeSHA256, nil }
	a.dllSHA = func(string) (string, error) { return pinnedProvenance.DllSHA256, nil }
}

// Probe happy path (B12 rewritten - the old version pinned inflated
// claims): identity parsed, support known, the READ PATH proven by
// the terse read itself answering AND parsing, apply honestly
// UNKNOWN, the hardware-validated range honestly UNKNOWN, the tool
// syntax bound kept as a separate fact, and the pinned-provenance
// note stating the installed exe is UNVALIDATED.
func TestProbeHappy(t *testing.T) {
	r := &fakeRunner{
		outs:  []string{verOK, phys16, "Current PBO offsets:\n-10,0,-5\n"},
		codes: []int{0, 0, 0},
	}
	a := adapter(t, r)
	caps, err := a.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Version != "0.1.3" || caps.SHA256 == "" {
		t.Fatalf("identity not pinned: %+v", caps)
	}
	if !caps.Supported.Known || !caps.Supported.Yes {
		t.Fatalf("supported = %+v", caps.Supported)
	}
	if !caps.Read.Known || !caps.Read.Yes {
		t.Fatalf("read must be proven by the terse read itself: %+v", caps.Read)
	}
	if !caps.Readback.Known || !caps.Readback.Yes {
		t.Fatalf("readback = %+v", caps.Readback)
	}
	if caps.Apply.Known {
		t.Fatalf("apply must stay unknown before a real write: %+v", caps.Apply)
	}
	if caps.RangeKnown {
		t.Fatalf("hardware-validated range must stay unknown after a probe: %+v", caps)
	}
	if !caps.SyntaxKnown || caps.SyntaxMin != -99 || caps.SyntaxMax != 99 {
		t.Fatalf("syntax bound = %v [%d,%d], want known [-99,99]", caps.SyntaxKnown, caps.SyntaxMin, caps.SyntaxMax)
	}
	if !strings.Contains(strings.Join(caps.Notes, "\n"), "UNVALIDATED") {
		t.Errorf("the installed exe must be stated UNVALIDATED against pinned build digests, notes: %v", caps.Notes)
	}
	if len(r.calls) != 3 || r.calls[0][1] != "--version" ||
		r.calls[1][1] != "--get-physical-cores" || r.calls[2][1] != "--get-offsets-terse" {
		t.Fatalf("calls = %v, want version + physical-cores + terse read", r.calls)
	}
}

// Probe failure branches: missing exe, elevation, ZenStates init
// failure (unsupported CPU or driver - the tool cannot tell), and
// unparseable support output.
func TestProbeBranches(t *testing.T) {
	mk := func(outs []string, codes []int, errs []error) *Adapter {
		return New(tempExe(t), &fakeRunner{outs: outs, codes: codes, errs: errs})
	}
	a := New("Z:\\nope\\ryzen-smu-cli.exe", &fakeRunner{})
	if _, err := a.Probe(context.Background()); !co.IsKind(err, co.KindMissingTool) {
		t.Errorf("missing exe: %v", err)
	}
	a = mk([]string{"", ""}, []int{0, 1}, nil)
	if _, err := a.Probe(context.Background()); !co.IsKind(err, co.KindNoPrivilege) {
		t.Errorf("exit 1: %v", err)
	}
	a = mk([]string{"", "Error: no CPU\n"}, []int{0, 2}, nil)
	if _, err := a.Probe(context.Background()); !co.IsKind(err, co.KindUnsupportedCPU) {
		t.Errorf("exit 2: %v", err)
	}
	a = mk([]string{"", "totally different tool output\n"}, []int{0, 0}, nil)
	if _, err := a.Probe(context.Background()); !co.IsKind(err, co.KindMalformedOutput) {
		t.Errorf("garbage support output: %v", err)
	}
	// Unparseable --version answer: version stays unknown ("") but
	// the probe itself must not fabricate one.
	a = mk([]string{"something else\n", phys16}, []int{0, 0}, nil)
	caps, err := a.Probe(context.Background())
	if err != nil || caps.Version != "" {
		t.Fatalf("version honesty broken: %+v err=%v", caps, err)
	}
}

// Topology: disabled physical cores are skipped with an honest note
// (their offsets are not addressable), slots keep SMU physical
// indices separate from tool ids, and OS logical ids stay nil.
func TestTopologySkipsDisabled(t *testing.T) {
	fused := "Fused status of physical cores:\nCore 0: Enabled\nCore 1: Disabled\nCore 2: Enabled\n"
	enabled := "Core 0: Enabled\nCore 1: Disabled\nCore 2: Enabled\n"
	r := &fakeRunner{outs: []string{fused, enabled}, codes: []int{0, 0}}
	a := adapter(t, r)
	m, err := a.Topology(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Slots) != 2 {
		t.Fatalf("slots = %d, want 2 (disabled core skipped)", len(m.Slots))
	}
	if m.Slots[1].ToolID != "core1" || *m.Slots[1].SMUIndex != 2 {
		t.Fatalf("slot numbering wrong: %+v", m.Slots[1])
	}
	if m.Slots[0].OSLogical != nil {
		t.Fatalf("OS logical claimed without evidence: %+v", m.Slots[0])
	}
	if m.Quality != "partial" {
		t.Fatalf("quality = %q, want partial", m.Quality)
	}
}

// Topology count disagreement between the two lists is offset_count.
func TestTopologyCountMismatch(t *testing.T) {
	r := &fakeRunner{outs: []string{phys16, "Core 0: Enabled\n"}, codes: []int{0, 0}}
	a := adapter(t, r)
	if _, err := a.Topology(context.Background()); !co.IsKind(err, co.KindOffsetCount) {
		t.Fatalf("count mismatch: %v", err)
	}
}

// --- Read ladder (audit B03) -------------------------------------
//
// The old TestReadBranches pinned the overreach this fixes: "exact"
// for any parsed CSV with positionally-assigned cores, no topology
// cross-check, no identity. The ladder now: attribution-unknown
// answers (short CSV, disabled-cores note, count mismatch) =>
// quality unknown with an EMPTY profile and the raw values kept as
// evidence only; a full CSV without a complete fingerprint =>
// partial naming the missing components; exact only when the count
// matches the established slots AND the fingerprint is complete.

const enabled3 = "Core 0: Enabled\nCore 1: Enabled\nCore 2: Enabled\n"
const enabled2 = "Core 0: Enabled\nCore 1: Enabled\n"
const terse3 = "Current PBO offsets:\n-15,0,2\n"

// readScript scripts one Read from a bare adapter: Topology's two
// lists (3 addressable slots) then the terse answer under test.
func readScript(terse string) *fakeRunner {
	return &fakeRunner{outs: []string{phys16, enabled3, terse}, codes: []int{0, 0, 0}}
}

func mustRead(t *testing.T, r *fakeRunner) co.Snapshot {
	t.Helper()
	s, err := adapter(t, r).Read(context.Background())
	if err != nil {
		t.Fatalf("read errored: %v", err)
	}
	return s
}

// mustReadSame reads again from an established adapter (mapping
// cached: the fakeRunner's repeated last answer is the terse one).
func mustReadSame(t *testing.T, a *Adapter) co.Snapshot {
	t.Helper()
	s, err := a.Read(context.Background())
	if err != nil {
		t.Fatalf("read errored: %v", err)
	}
	return s
}

func TestReadLadder(t *testing.T) {
	// exact: probe establishes identity, CSV matches the slots.
	r := &fakeRunner{
		outs:  []string{verOK, phys16, terse3, phys16, enabled3, terse3},
		codes: []int{0, 0, 0, 0, 0, 0},
	}
	a := adapter(t, r)
	if _, err := a.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := a.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Quality != "exact" || s.HardwareID == "" || len(s.Profile.Offsets) != 3 {
		t.Fatalf("exact read broken: %+v", s)
	}
	if s.Profile.Offsets[0] != (co.Offset{Core: "core0", Value: -15}) {
		t.Errorf("slot ids must come from the mapping, not positions: %+v", s.Profile.Offsets)
	}

	// Short CSV (upstream skipped a failed core read): attribution
	// is unknown, so NOTHING is addressable; raw values stay notes.
	s = mustRead(t, readScript("Current PBO offsets:\n-15,0\n"))
	if s.Quality != "unknown" || len(s.Profile.Offsets) != 0 {
		t.Fatalf("short CSV gained an attributed profile: %+v", s)
	}
	if !strings.Contains(strings.Join(s.Notes, " "), "attribution unknown") ||
		!strings.Contains(strings.Join(s.Notes, " "), "raw values") {
		t.Errorf("raw evidence not kept in notes: %v", s.Notes)
	}

	// Extra value: the count mismatch cuts both ways.
	s = mustRead(t, readScript("Current PBO offsets:\n-15,0,2,7\n"))
	if s.Quality != "unknown" || len(s.Profile.Offsets) != 0 {
		t.Fatalf("extra value gained an attributed profile: %+v", s)
	}

	// Disabled-cores note: values may have been skipped silently -
	// unknown, never a partial attribution.
	s = mustRead(t, readScript("Current PBO offsets:\n-15,0,2\nYou have manually disabled cores. It is not possible to retrieve offset values while they are disabled.\n"))
	if s.Quality != "unknown" || len(s.Profile.Offsets) != 0 {
		t.Fatalf("disabled-note read gained an attributed profile: %+v", s)
	}

	// Topology changed under the read: 3 values for 2 slots.
	r = &fakeRunner{outs: []string{phys2, enabled2, terse3}, codes: []int{0, 0, 0}}
	s = mustRead(t, r)
	if s.Quality != "unknown" || !strings.Contains(strings.Join(s.Notes, " "), "3 values for 2 addressable slots") {
		t.Fatalf("slot/value mismatch not caught: %+v", s)
	}

	// Full CSV but identity incomplete (no probe: version, exe
	// digest and tool name unknown): partial, missing parts named.
	s = mustRead(t, readScript(terse3))
	if s.Quality != "partial" || s.HardwareID != "" {
		t.Fatalf("incomplete fingerprint claimed exact: %+v", s)
	}
	if !strings.Contains(strings.Join(s.Notes, " "), "tool version") {
		t.Errorf("missing fingerprint components not named: %v", s.Notes)
	}

	// Malformed shapes stay malformed_output.
	for _, bad := range []string{"Current PBO offsets:\n-15,oops\n", "wrong header\n-15\n", "Current PBO offsets:\n"} {
		if _, err := adapter(t, readScript(bad)).Read(context.Background()); !co.IsKind(err, co.KindMalformedOutput) {
			t.Errorf("malformed %q: %v", bad, err)
		}
	}
}

// The fingerprint binds machine+tool+topology: the same state reads
// back the same id, and any moved component moves it - a later
// restore compare can then detect the machine or tool changed.
func TestFingerprintIdentity(t *testing.T) {
	probeRead := []string{verOK, phys16, terse3, phys16, enabled3, terse3}
	r := &fakeRunner{outs: probeRead, codes: []int{0, 0, 0, 0, 0, 0}}
	a := adapter(t, r)
	if _, err := a.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	s1, err := a.Read(context.Background())
	if err != nil || s1.Quality != "exact" {
		t.Fatalf("first read: %+v err=%v", s1, err)
	}
	if s2 := mustReadSame(t, a); s2.HardwareID != s1.HardwareID {
		t.Errorf("stable machine+tool+topology changed fingerprint: %q vs %q", s1.HardwareID, s2.HardwareID)
	}
	// Different topology (one core disabled) => different id.
	r = &fakeRunner{outs: []string{verOK, phys16, terse3, phys2, enabled2, "Current PBO offsets:\n-15,0\n"}, codes: []int{0, 0, 0, 0, 0, 0}}
	a = adapter(t, r)
	if _, err := a.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	s3, err := a.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s3.HardwareID == "" || s3.HardwareID == s1.HardwareID {
		t.Errorf("topology change not reflected in fingerprint: %q vs %q", s3.HardwareID, s1.HardwareID)
	}
}

// --- Apply: call log, not mutation proof (audit B05) -------------
//
// The old TestApplyOutcomes pinned the overreach this fixes:
// matching line COUNTS became "completed", and log lines before an
// exit 6 became a "confirmed" partial Applied list. Program.cs
// prints "Set logical core ..." after calling SetPsmMarginSingleCore
// WITHOUT checking its result, so for this backend the write outcome
// is unknown whatever the log says; the log is only cross-read for
// disagreements, and Verify's readback is the sole state witness.

// validatedAdapter simulates the state only a P10d-verified backend
// earns: probe-known support AND a hardware-validated range AND a
// live mapping. Tests may fabricate it; the real Probe never sets
// RangeKnown without a verified smoke.
func validatedAdapter(t *testing.T, r *fakeRunner) *Adapter {
	t.Helper()
	a := adapter(t, r)
	pinBuild(a) // C02: the write path re-checks the CURRENT install against the pinned manifest
	a.caps = co.Capabilities{
		Tool: "ryzen-smu-cli", Version: "0.1.3", SHA256: "testdigest",
		PayloadSHA256: "testpayload",
		Supported:     co.Yes("verified in test"),
		RangeKnown:    true, RangeMin: -99, RangeMax: 99,
		SyntaxKnown: true, SyntaxMin: -99, SyntaxMax: 99,
	}
	m := co.Mapping{Quality: "partial", Slots: []co.CoreSlot{
		{ToolID: "core0", SMUIndex: intp(0)},
		{ToolID: "core1", SMUIndex: intp(1)},
	}}
	a.mapping = &m
	a.topoID = "f:11;e:11;k:2"
	return a
}

func TestApplyCallLogNeverSelfConfirms(t *testing.T) {
	want := co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -10}, {Core: "core1", Value: -20}}}

	// exit 0 with a perfectly matching log: still unknown, Applied
	// empty, the call-log semantics stated.
	a := validatedAdapter(t, &fakeRunner{outs: []string{
		"Set logical core 0, physical core 0 offset to -10!\nSet logical core 1, physical core 1 offset to -20!\n"}, codes: []int{0}})
	res, err := a.Apply(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mutated != co.MutatedUnknown || len(res.Applied) != 0 {
		t.Fatalf("backend self-confirmed a mutation: %+v", res)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "call log, not hardware proof") {
		t.Errorf("call-log semantics not stated: %v", res.Notes)
	}

	// The B05 reproduction: two lines for core0 against a
	// {core0, core1} request used to yield completed/Applied=[core0,
	// core0]. Now: unknown + duplicate + missing-core1 named.
	a = validatedAdapter(t, &fakeRunner{outs: []string{
		"Set logical core 0, physical core 0 offset to -10!\nSet logical core 0, physical core 0 offset to -10!\n"}, codes: []int{0}})
	res, _ = a.Apply(context.Background(), want)
	notes := strings.Join(res.Notes, " | ")
	if res.Mutated != co.MutatedUnknown || !strings.Contains(notes, "duplicate") ||
		!strings.Contains(notes, "no Set call logged for logical core 1") {
		t.Fatalf("duplicate/missing log lines not surfaced: %+v", res)
	}

	// exit 0 without any line: unknown, every core reported missing.
	a = validatedAdapter(t, &fakeRunner{outs: []string{"ok\n"}, codes: []int{0}})
	res, _ = a.Apply(context.Background(), want)
	notes = strings.Join(res.Notes, " | ")
	if res.Mutated != co.MutatedUnknown || !strings.Contains(notes, "no Set call logged for logical core 0") ||
		!strings.Contains(notes, "no Set call logged for logical core 1") {
		t.Fatalf("silent exit-0 apply not reported honestly: %+v", res)
	}

	// Abort after one call (exit 6): unknown outcome, the logged
	// call is a call log, the error keeps its exit-code kind.
	a = validatedAdapter(t, &fakeRunner{outs: []string{
		"Set logical core 0, physical core 0 offset to -10!\nTried to set an offset on logical core 9, but there are only 2 (zero-indexed, as a reminder) logical cores active in the system.\n"}, codes: []int{6}})
	res, err = a.Apply(context.Background(), want)
	if !co.IsKind(err, co.KindBadProfile) || res.Mutated != co.MutatedUnknown ||
		!strings.Contains(strings.Join(res.Notes, " "), "no Set call logged for logical core 1") {
		t.Fatalf("mid-loop abort lost the honest story: %+v err=%v", res, err)
	}

	// Disagreements in the log are named: foreign core, wrong value,
	// wrong physical index.
	a = validatedAdapter(t, &fakeRunner{outs: []string{
		"Set logical core 9, physical core 9 offset to -5!\n" +
			"Set logical core 0, physical core 0 offset to -11!\n" +
			"Set logical core 1, physical core 7 offset to -20!\n"}, codes: []int{0}})
	res, _ = a.Apply(context.Background(), want)
	notes = strings.Join(res.Notes, " | ")
	for _, wantNote := range []string{"not requested", "differs from the requested", "SMU index"} {
		if !strings.Contains(notes, wantNote) {
			t.Errorf("log disagreement %q not surfaced: %v", wantNote, res.Notes)
		}
	}
}

// Apply timeout/cancel: mutation outcome UNKNOWN, never none, and
// no call-log claims read out of an answer we never saw.
func TestApplyTimeoutUnknown(t *testing.T) {
	tErr := &co.Error{Kind: co.KindTimeout, Op: "exec"}
	a := validatedAdapter(t, &fakeRunner{outs: []string{""}, codes: []int{-1}, errs: []error{tErr}})
	res, err := a.Apply(context.Background(), co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -5}}})
	if !co.IsKind(err, co.KindTimeout) || res.Mutated != co.MutatedUnknown {
		t.Fatalf("timeout outcome: %+v err=%v", res, err)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "may or may not have reached") {
		t.Errorf("unobserved-answer semantics not stated: %v", res.Notes)
	}
}

// Verify: exact match yes, mismatch no with diff, an
// attribution-unknown read caps the verdict to unknown. Reads ride
// the established mapping (scripted topology first).
func TestVerifyBranches(t *testing.T) {
	a := validatedAdapter(t, &fakeRunner{outs: []string{"Current PBO offsets:\n-10,-20\n"}, codes: []int{0}})
	v, err := a.Verify(context.Background(), co.Profile{Offsets: []co.Offset{
		{Core: "core0", Value: -10}, {Core: "core1", Value: -20}}})
	if err != nil || !v.Match.Known || !v.Match.Yes {
		t.Fatalf("match yes broken: %+v err=%v", v, err)
	}
	v, _ = a.Verify(context.Background(), co.Profile{Offsets: []co.Offset{{Core: "core1", Value: -21}}})
	if v.Match.Known && v.Match.Yes {
		t.Fatalf("mismatch claimed match: %+v", v)
	}
	if len(v.Diff) != 1 || v.Diff[0].Core != "core1" {
		t.Fatalf("diff lost: %+v", v)
	}
	a = validatedAdapter(t, &fakeRunner{outs: []string{"Current PBO offsets:\n-10,-20\nYou have manually disabled cores. It is not possible to retrieve offset values while they are disabled.\n"}, codes: []int{0}})
	v, err = a.Verify(context.Background(), co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -10}}})
	if err != nil || v.Match.Known {
		t.Fatalf("attribution-unknown read must cap the verdict at unknown: %+v err=%v", v, err)
	}
}

// The exit-code table from Program.cs, pinned end to end.
func TestExitCodeMapping(t *testing.T) {
	for _, tc := range []struct {
		code int
		kind string
	}{
		{1, co.KindNoPrivilege}, {2, co.KindUnsupportedCPU}, {3, co.KindUnsupportedOp},
		{4, co.KindBadProfile}, {5, co.KindOffsetCount}, {6, co.KindBadProfile},
		{8, co.KindOffsetCount}, {99, co.KindMalformedOutput},
	} {
		err := exitError("apply", tc.code, "")
		if !co.IsKind(err, tc.kind) {
			t.Errorf("exit %d -> %q, want %q", tc.code, co.KindOf(err), tc.kind)
		}
	}
}

// Restore (B03/B05): an incomplete, fingerprint-less or empty
// snapshot never reaches the runner; a restorable one re-applies and
// lets the readback answer - the unknown write outcome never gates
// it, and a validation refusal stops the restore before any write.
func TestRestorePaths(t *testing.T) {
	good := co.Snapshot{
		Profile:    co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -7}}},
		Quality:    "exact",
		HardwareID: "fp-test",
	}
	// Gate table: every unusable snapshot is refused with zero
	// runner calls.
	for _, tc := range []struct {
		name string
		snap co.Snapshot
	}{
		{"partial", co.Snapshot{Profile: good.Profile, Quality: "partial", HardwareID: "fp"}},
		{"unknown", co.Snapshot{Profile: good.Profile, Quality: "unknown", HardwareID: "fp"}},
		{"no fingerprint", co.Snapshot{Profile: good.Profile, Quality: "exact"}},
		{"empty profile", co.Snapshot{Quality: "exact", HardwareID: "fp"}},
	} {
		r := &fakeRunner{}
		a := validatedAdapter(t, r)
		if _, err := a.Restore(context.Background(), tc.snap); !co.IsKind(err, co.KindRestoreRefused) {
			t.Errorf("%s: restore not refused: %v", tc.name, err)
		}
		if len(r.calls) != 0 {
			t.Errorf("%s: restore reached the runner (%v)", tc.name, r.calls)
		}
	}
	// Happy path: the restore re-reads to find the diff, writes only
	// the differing core, and the readback confirms.
	a := validatedAdapter(t, &fakeRunner{outs: []string{
		"Current PBO offsets:\n-9,-20\n",
		"Set logical core 0, physical core 0 offset to -7!\n",
		"Current PBO offsets:\n-7,-20\n"}, codes: []int{0, 0, 0}})
	v, err := a.Restore(context.Background(), good)
	if err != nil || !v.Match.Known || !v.Match.Yes {
		t.Fatalf("restore happy path: %+v err=%v", v, err)
	}
	if !strings.Contains(strings.Join(v.Notes, " "), "write-call outcome "+co.MutatedUnknown) {
		t.Errorf("restore hid the unknown write outcome: %v", v.Notes)
	}
	// Readback disagrees: the answer is no, whatever the log said.
	a = validatedAdapter(t, &fakeRunner{outs: []string{
		"Current PBO offsets:\n-9,-20\n",
		"Set logical core 0, physical core 0 offset to -7!\n",
		"Current PBO offsets:\n-9,-20\n"}, codes: []int{0, 0, 0}})
	v, _ = a.Restore(context.Background(), good)
	if v.Match.Known && v.Match.Yes {
		t.Fatalf("unchanged/contradicted readback confirmed restore: %+v", v)
	}
	// The write ran but died (exit 2): the readback still answers,
	// and the error keeps its kind.
	a = validatedAdapter(t, &fakeRunner{outs: []string{
		"Current PBO offsets:\n-9,-20\n", "Error: no CPU\n", "Current PBO offsets:\n-7,-20\n"}, codes: []int{0, 2, 0}})
	v, err = a.Restore(context.Background(), good)
	if !co.IsKind(err, co.KindUnsupportedCPU) {
		t.Fatalf("restore write kind: %v", err)
	}
	if !v.Match.Known || !v.Match.Yes {
		t.Fatalf("readback after failed write must still answer: %+v", v)
	}
	// A validation refusal stops everything before any write.
	a = adapter(t, &fakeRunner{}) // nothing established
	v, err = a.Restore(context.Background(), good)
	if err == nil || len(a.Run.(*fakeRunner).calls) != 0 {
		t.Fatalf("restore passed an unvalidated adapter to the runner: %v", err)
	}
	if !v.Match.Known {
		// unknown match is right; just ensure it is not a silent yes
		_ = v
	}
}

// The adapter must pass EXACTLY the pinned args - direct exec, no
// shell strings. The tool numbers its cores numerically ("0:-10"),
// so our "coreN" ids map to the bare index. The adapter must be in
// a validated state to get that far at all (B04).
func TestArgsPinned(t *testing.T) {
	r := &fakeRunner{outs: []string{"Set logical core 0, physical core 0 offset to -5!\nSet logical core 1, physical core 1 offset to 7!\n"}, codes: []int{0}}
	a := validatedAdapter(t, r)
	if _, err := a.Apply(context.Background(), co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -5}, {Core: "core1", Value: 7}}}); err != nil {
		t.Fatal(err)
	}
	got := r.calls[0][1:]
	if len(got) != 2 || got[0] != "--offset" || got[1] != "0:-5,1:7" {
		t.Fatalf("args = %v, want [--offset 0:-5,1:7]", got)
	}
}

// --- Validate gates every write path (audit B04) -----------------
//
// The old Validate checked only value bounds against a range the
// probe had fabricated from the tool's syntax; unknown cores,
// duplicates and unestablished prerequisites sailed through, and
// Apply never called Validate at all. Now every refusal happens
// before the runner: the call counter stays zero.
func TestValidateGatesBeforeRunner(t *testing.T) {
	prof := co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -10}}}

	// (a) Fresh adapter: nothing established - neither Validate nor
	// Apply may reach the runner.
	r := &fakeRunner{}
	a := adapter(t, r)
	if err := a.Validate(prof); err == nil {
		t.Error("unprobed adapter validated a write")
	}
	if _, err := a.Apply(context.Background(), prof); err == nil || len(r.calls) != 0 {
		t.Errorf("Apply on an unprobed adapter: err=%v calls=%v", err, r.calls)
	}

	// (b) Probed but hardware range unvalidated (today's honest
	// state): the syntax bound does not authorize writes.
	r = &fakeRunner{outs: []string{verOK, phys16, terse3}, codes: []int{0, 0, 0}}
	a = adapter(t, r)
	if _, err := a.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Validate(prof); !co.IsKind(err, co.KindNotValidated) {
		t.Errorf("unvalidated range authorized a write: %v", err)
	}
	if _, err := a.Apply(context.Background(), prof); !co.IsKind(err, co.KindNotValidated) || len(r.calls) != 3 {
		t.Errorf("Apply wrote despite unvalidated range (calls=%v): %v", r.calls[3:], err)
	}

	// (c) Validated caps but no mapping.
	a = validatedAdapter(t, &fakeRunner{})
	a.mapping = nil
	if err := a.Validate(prof); !co.IsKind(err, co.KindNotValidated) {
		t.Errorf("missing mapping authorized a write: %v", err)
	}

	// (d) Profile shape on a fully validated adapter: bad ids,
	// duplicates and out-of-range values are refused; the exact
	// syntax bounds pass.
	for _, tc := range []struct {
		name  string
		prof  co.Profile
		kind  string
		match string
	}{
		{"garbage id", co.Profile{Offsets: []co.Offset{{Core: "garbage", Value: -10}}}, co.KindBadProfile, "not an addressable"},
		{"core999", co.Profile{Offsets: []co.Offset{{Core: "core999", Value: -10}}}, co.KindBadProfile, "not an addressable"},
		{"negative index", co.Profile{Offsets: []co.Offset{{Core: "core-1", Value: -10}}}, co.KindBadProfile, "not an addressable"},
		{"duplicate core", co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -10}, {Core: "core0", Value: -20}}}, co.KindBadProfile, "appears twice"},
		{"out of syntax bound", co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -100}}}, co.KindBadProfile, "outside the tool syntax bound"},
	} {
		a := validatedAdapter(t, &fakeRunner{})
		err := a.Validate(tc.prof)
		if !co.IsKind(err, tc.kind) || !strings.Contains(err.Error(), tc.match) {
			t.Errorf("%s: %v", tc.name, err)
		}
		if _, err := a.Apply(context.Background(), tc.prof); !co.IsKind(err, tc.kind) {
			t.Errorf("%s: Apply bypassed Validate: %v", tc.name, err)
		}
	}
	// A hardware-validated range NARROWER than the syntax bound must
	// gate values inside the syntax bound but outside the validated
	// span (this is the B04/B12 distinction: parse limit != accepted
	// span). core0=-50 parses (-99..99) but is outside [-20,0].
	a = validatedAdapter(t, &fakeRunner{})
	a.caps.RangeMin, a.caps.RangeMax = -20, 0
	if err := a.Validate(co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -50}}}); !co.IsKind(err, co.KindBadProfile) ||
		!strings.Contains(err.Error(), "outside validated range") {
		t.Errorf("narrow validated range not enforced: %v", err)
	}
	// Empty profile: Validate refuses (nothing to write), while
	// Apply short-circuits honestly to none with zero runner calls -
	// an empty request is not a mutation and must not reach the
	// tool, but it is also not an error worth retrying.
	a = validatedAdapter(t, &fakeRunner{})
	if err := a.Validate(co.Profile{}); !co.IsKind(err, co.KindBadProfile) {
		t.Errorf("empty profile Validate: %v", err)
	}
	res, err := a.Apply(context.Background(), co.Profile{})
	if err != nil || res.Mutated != co.MutatedNone || len(a.Run.(*fakeRunner).calls) != 0 {
		t.Errorf("empty profile Apply: %+v err=%v", res, err)
	}
	// Bounds of the validated range pass.
	a = validatedAdapter(t, &fakeRunner{outs: []string{"ok\n"}, codes: []int{0}})
	if err := a.Validate(co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -99}, {Core: "core1", Value: 99}}}); err != nil {
		t.Errorf("validated bounds refused: %v", err)
	}
}

// --- Probe honesty about provenance and the read path (B12) ------
func TestProbeReadProvenance(t *testing.T) {
	// The terse answer must PARSE for Read to be yes; garbage caps
	// read/readback at unknown with the reason, and the probe itself
	// stays usable.
	r := &fakeRunner{outs: []string{verOK, phys16, "totally not offsets\n"}, codes: []int{0, 0, 0}}
	a := adapter(t, r)
	caps, err := a.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Read.Known {
		t.Errorf("unparsed terse answer claimed Read: %+v", caps.Read)
	}
	if caps.Readback.Known {
		t.Errorf("readback claimed while read unproven: %+v", caps.Readback)
	}
	// A version that does not match the pinned release is stated as
	// an UNVALIDATED build.
	r = &fakeRunner{outs: []string{"ryzen-smu-cli 9.9.9\n", phys16, terse3}, codes: []int{0, 0, 0}}
	caps, _ = adapter(t, r).Probe(context.Background())
	if !strings.Contains(strings.Join(caps.Notes, "\n"), "does not match pinned") {
		t.Errorf("mismatched version not flagged: %v", caps.Notes)
	}
	// The pinned version matches, but the installed exe digest is
	// matched against nothing: still UNVALIDATED.
	r = &fakeRunner{outs: []string{verOK, phys16, terse3}, codes: []int{0, 0, 0}}
	caps, _ = adapter(t, r).Probe(context.Background())
	notes := strings.Join(caps.Notes, "\n")
	if !strings.Contains(notes, "matches pinned release") || !strings.Contains(notes, "UNVALIDATED") {
		t.Errorf("provenance note wrong: %v", caps.Notes)
	}
	// A probe re-establishes everything: a second probe with a
	// failing support call must not keep the first probe's support.
	r = &fakeRunner{outs: []string{verOK, phys16, terse3, verOK, "Error: no CPU\n"}, codes: []int{0, 0, 0, 0, 2}}
	a = adapter(t, r)
	if _, err := a.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Probe(context.Background()); !co.IsKind(err, co.KindUnsupportedCPU) {
		t.Fatalf("second probe did not re-derive support: %v", err)
	}
	if a.caps.Supported.Known {
		t.Errorf("stale support survived a failed probe: %+v", a.caps)
	}
}

// The VALIDATED branch needs all three axes to match the manifest:
// the reported version, the SourceLink commit the build embeds in it,
// and the measured exe digest. The exeSHA seam supplies the pinned
// digest so the branch is testable without shipping the real apphost.
// Even when validated, the hardware-validated RANGE stays unknown: a
// confirmed build identity never authorizes a write.
func TestProbeValidatedBuild(t *testing.T) {
	r := &fakeRunner{outs: []string{verOK, phys16, terse3}, codes: []int{0, 0, 0}}
	a := New(tempExe(t), r)
	pinBuild(a)
	caps, err := a.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(caps.Notes, "\n")
	if !strings.Contains(notes, "build VALIDATED") {
		t.Errorf("validated build not stated: %v", caps.Notes)
	}
	if !strings.Contains(notes, pinnedProvenance.Commit[:12]) {
		t.Errorf("embedded SourceLink commit not surfaced: %v", caps.Notes)
	}
	if caps.RangeKnown {
		t.Errorf("a validated build wrongly implied a hardware-validated range")
	}
}

// A build reporting the pinned version but whose exe digest differs is
// a different or tampered binary: UNVALIDATED. Version alone never
// inherits trust.
func TestProbeVersionMatchIdentityMismatch(t *testing.T) {
	r := &fakeRunner{outs: []string{verOK, phys16, terse3}, codes: []int{0, 0, 0}}
	a := New(tempExe(t), r)
	a.exeSHA = func(string) (string, error) {
		return "0000000000000000000000000000000000000000000000000000000000000000", nil
	}
	caps, _ := a.Probe(context.Background())
	notes := strings.Join(caps.Notes, "\n")
	if !strings.Contains(notes, "identity does NOT") || !strings.Contains(notes, "UNVALIDATED") {
		t.Errorf("digest mismatch not flagged as a different/tampered build: %v", caps.Notes)
	}
}

// While no hardware-validated range exists, only the minimal single-
// core bootstrap may write, and the write must touch ONLY the changed
// core (differential), never the whole profile.
func TestBootstrapAuthorityAndDiffWrite(t *testing.T) {
	r := &fakeRunner{outs: []string{
		"Current PBO offsets:\n-12,-20\n",                      // Read -> lastExact
		"Set logical core 0, physical core 0 offset to -13!\n", // Apply --offset 0:-13
	}}
	a := adapter(t, r)
	pinBuild(a)
	a.caps = co.Capabilities{Tool: "ryzen-smu-cli", Version: "0.1.3", SHA256: "d", PayloadSHA256: "p",
		Supported: co.Yes("ok"), SyntaxKnown: true, SyntaxMin: -99, SyntaxMax: 99,
		BuildValidated: true} // RangeKnown false, but the build IS validated
	a.mapping = &co.Mapping{Quality: "partial", Slots: []co.CoreSlot{
		{ToolID: "core0", SMUIndex: intp(0)}, {ToolID: "core1", SMUIndex: intp(1)}}}
	a.topoID = "f:11;e:11;k:2"
	if _, err := a.Read(context.Background()); err != nil {
		t.Fatal(err) // sets lastExact = {-12,-20}
	}
	desired := co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -13}, {Core: "core1", Value: -20}}}
	if err := a.Validate(desired); err != nil {
		t.Fatalf("bootstrap delta 1 refused: %v", err)
	}
	if _, err := a.Apply(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	// Only core0 is written; core1 already holds -20 and is untouched.
	if len(r.calls) != 2 || r.calls[1][1] != "--offset" || r.calls[1][2] != "0:-13" {
		t.Fatalf("apply wrote %v, want --offset 0:-13 only", r.calls)
	}
	// delta 3 from current: refused.
	if err := a.Validate(co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -15}, {Core: "core1", Value: -20}}}); !co.IsKind(err, co.KindNotValidated) {
		t.Errorf("delta 3 not refused: %v", err)
	}
	// two cores changed: refused.
	if err := a.Validate(co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -13}, {Core: "core1", Value: -21}}}); !co.IsKind(err, co.KindNotValidated) {
		t.Errorf("two-core change not refused: %v", err)
	}
	// no current read to bound against: refused.
	a.lastExact = nil
	if err := a.Validate(desired); !co.IsKind(err, co.KindNotValidated) {
		t.Errorf("bootstrap without a current read not refused: %v", err)
	}
	// A tampered/unvalidated build is refused even for a minimal
	// delta: the delta rule bounds the VALUE, not the binary.
	a.lastExact = &co.Snapshot{Profile: co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -12}, {Core: "core1", Value: -20}}}, Quality: "exact", HardwareID: "x"}
	a.caps.BuildValidated = false
	if err := a.Validate(desired); !co.IsKind(err, co.KindNotValidated) {
		t.Errorf("bootstrap on an unvalidated build not refused: %v", err)
	}
}

// A restore re-applies values the machine already held; it is
// authorized by the transaction that captured them, NOT by a
// hardware-validated writable range. So an original like -48 (outside
// any minimal bootstrap span) restores fine even while a FRESH write
// of -48 is refused.
func TestRestoreNotGatedByHardwareRange(t *testing.T) {
	r := &fakeRunner{outs: []string{
		"Current PBO offsets:\n-13,-20\n",                      // current before restore
		"Set logical core 0, physical core 0 offset to -48!\n", // restore --offset 0:-48
		"Current PBO offsets:\n-48,-20\n",                      // verify
	}}
	a := adapter(t, r)
	pinBuild(a)
	a.caps = co.Capabilities{Tool: "ryzen-smu-cli", Version: "0.1.3", SHA256: "d", PayloadSHA256: "p",
		Supported: co.Yes("ok"), SyntaxKnown: true, SyntaxMin: -99, SyntaxMax: 99,
		BuildValidated: true} // RangeKnown FALSE, build validated
	a.mapping = &co.Mapping{Quality: "partial", Slots: []co.CoreSlot{
		{ToolID: "core0", SMUIndex: intp(0)}, {ToolID: "core1", SMUIndex: intp(1)}}}
	a.topoID = "f:11;e:11;k:2"
	// A fresh write of -48 (delta 35 from current) is refused while
	// unvalidated...
	if err := a.Validate(co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -48}, {Core: "core1", Value: -20}}}); !co.IsKind(err, co.KindNotValidated) {
		t.Errorf("a fresh -48 write should be refused while unvalidated: %v", err)
	}
	// ...but restoring the saved exact snapshot proceeds and confirms.
	saved := co.Snapshot{Profile: co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -48}, {Core: "core1", Value: -20}}}, Quality: "exact", HardwareID: "fp"}
	v, err := a.Restore(context.Background(), saved)
	if err != nil {
		t.Fatalf("restore of an out-of-span original refused: %v", err)
	}
	if !v.Match.Known || !v.Match.Yes {
		t.Errorf("restore not confirmed: %+v", v)
	}
	// Only the differing core (core0) was rewritten, not core1.
	if len(r.calls) != 3 || r.calls[1][1] != "--offset" || r.calls[1][2] != "0:-48" {
		t.Fatalf("restore wrote %v, want --offset 0:-48 only", r.calls)
	}
}
