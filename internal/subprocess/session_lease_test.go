package subprocess

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/memtest"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// In-process lease tests: the "child" session opens a SECOND view of
// the same section inside the test process (OpenSharedMapping on the
// parent's own handle), so writer and reader see the same physical
// bytes through different virtual addresses - the exact sharing the
// real cross-process spawn relies on, minus the process boundary
// (which P07e1 already proved separately). The full cross-process
// e2e lands with the P07e4 coordinator.

func leaseAcks(t *testing.T, buf *bytes.Buffer) []transition.Ack {
	t.Helper()
	var out []transition.Ack
	dec := json.NewDecoder(buf)
	for {
		var a transition.Ack
		if err := dec.Decode(&a); err != nil {
			break
		}
		out = append(out, a)
	}
	return out
}

func leaseFixture(t *testing.T, blockSize uint64, nBlocks int, seed uint64) (*memtest.SharedMapping, *workerSession, *bytes.Buffer) {
	t.Helper()
	size := blockSize * uint64(nBlocks)
	m, err := memtest.CreateSharedMapping(size)
	if err != nil {
		t.Fatalf("CreateSharedMapping: %v", err)
	}
	t.Cleanup(func() { m.Close() })

	grants := make([]memtest.Grant, nBlocks)
	for i := range grants {
		grants[i] = memtest.Grant{Worker: 3, Region: i, Role: memtest.RoleDest, Offset: uint64(i) * blockSize, Requested: blockSize}
	}
	jf, err := journal.OpenFile(filepath.Join(t.TempDir(), "frag.jsonl"))
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	jw := journal.NewWriter(jf, "lease-test")
	t.Cleanup(func() { jw.Close() })

	var acks bytes.Buffer
	s := &workerSession{
		p: sessionParams{
			core: 3, workloadName: "ram-handoff", beat: time.Second,
			memBudget: size, memAllowance: 0, memLock: "off", ramSeed: seed,
			grants: grants, jw: jw, cs: &coreCounter{jw: jw, core: 3},
			Handoff: true, SharedHandle: m.Handle(), SharedSize: size,
		},
		enc: json.NewEncoder(&acks),
	}
	return m, s, &acks
}

func leaseCmd(kind transition.Kind, seq, block, gen uint64) transition.Command {
	b, g := block, gen
	return transition.Command{V: transition.ProtocolVersion, Seq: seq, Kind: kind, Block: &b, Gen: &g}
}

// The clean cycle over a real shared section: PREPARE maps the
// section and opens the engine over it; LEASE_WRITE fills block 1 and
// the PARENT view (a different virtual address, same pages) sees the
// exact ram-handoff-v1 image; LEASE_READ verifies it independently and
// acks READ_OK with the real counts; STOP closes the session.
func TestSessionLeaseWriteReadClean(t *testing.T) {
	const block = 64 * 1024
	const seed = 0xABCD
	m, s, acks := leaseFixture(t, block, 2, seed)

	if code, done := s.handle(transition.Command{V: transition.ProtocolVersion, Seq: 1, Kind: transition.CmdPrepare}); code != 0 || done {
		t.Fatalf("prepare = (%d,%v), want (0,false)", code, done)
	}

	if code, done := s.handle(leaseCmd(transition.CmdLeaseWrite, 2, 1, 1)); code != 0 || done {
		t.Fatalf("lease-write = (%d,%v), want (0,false)", code, done)
	}

	// The parent-side view must hold the image the worker wrote
	// through its own view: one section, one set of physical pages.
	v, err := m.View(block, block)
	if err != nil {
		t.Fatalf("parent view: %v", err)
	}
	for i := 0; i < len(v); i += 4 {
		want := workload.HandoffWord(seed, 1, uint64(block)/4+uint64(i>>2))
		if got := binary.LittleEndian.Uint32(v[i:]); got != want {
			t.Fatalf("parent view word at %d = %08x, want %08x (views do not share bytes?)", i, got, want)
		}
	}

	if code, done := s.handle(leaseCmd(transition.CmdLeaseRead, 3, 1, 1)); code != 0 || done {
		t.Fatalf("lease-read = (%d,%v), want (0,false)", code, done)
	}
	if code, done := s.handle(transition.Command{V: transition.ProtocolVersion, Seq: 4, Kind: transition.CmdStop}); code != 0 || !done {
		t.Fatalf("stop = (%d,%v), want (0,true)", code, done)
	}

	got := leaseAcks(t, acks)
	wantKinds := []transition.Kind{transition.AckReady, transition.AckIdle, transition.AckWritten, transition.AckReadOK, transition.AckStopped}
	if len(got) != len(wantKinds) {
		t.Fatalf("acks = %d (%v), want %d", len(got), got, len(wantKinds))
	}
	for i, k := range wantKinds {
		if got[i].Kind != k {
			t.Errorf("ack %d = %s, want %s", i, got[i].Kind, k)
		}
	}
	wr := got[2]
	if wr.Seq != 2 || wr.Bytes == nil || *wr.Bytes != block {
		t.Errorf("WRITTEN ack = %+v, want seq 2 bytes %d", wr, block)
	}
	rd := got[3]
	if rd.Seq != 3 || rd.Bytes == nil || *rd.Bytes != block || rd.Checks == nil || *rd.Checks != block/4 {
		t.Errorf("READ_OK ack = %+v, want seq 3 bytes %d checks %d", rd, block, block/4)
	}
}

// A corrupted word (injected through the PARENT view, i.e. damage
// arriving from outside the worker) must surface as a kernel fault:
// ERROR ack, exit 1, never a retry and never a setup 2.
func TestSessionLeaseReadFaultIsKernelFault(t *testing.T) {
	m, s, acks := leaseFixture(t, 4096, 1, 7)
	if code, _ := s.handle(transition.Command{V: transition.ProtocolVersion, Seq: 1, Kind: transition.CmdPrepare}); code != 0 {
		t.Fatalf("prepare code = %d", code)
	}
	if code, _ := s.handle(leaseCmd(transition.CmdLeaseWrite, 2, 0, 1)); code != 0 {
		t.Fatalf("lease-write code = %d", code)
	}
	v, err := m.View(0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(v[100:], ^binary.LittleEndian.Uint32(v[100:]))

	code, done := s.handle(leaseCmd(transition.CmdLeaseRead, 3, 0, 1))
	if code != 1 || !done {
		t.Fatalf("corrupted lease-read = (%d,%v), want (1,true): kernel fault ends the session", code, done)
	}
	got := leaseAcks(t, acks)
	last := got[len(got)-1]
	if last.Kind != transition.AckError || !strings.Contains(last.Msg, "lease-read fault") {
		t.Errorf("final ack = %+v, want ERROR naming the lease-read fault", last)
	}
	if last.Seq != 3 {
		t.Errorf("fault ack seq = %d, want 3 (the command that was answered)", last.Seq)
	}
}

// Content identity catches stale generations: block written at gen 1
// and read under a gen 2 lease reads the older values and faults at
// the first word - no header can claim a generation the bytes do not
// carry.
func TestSessionLeaseStaleGenerationFaults(t *testing.T) {
	_, s, acks := leaseFixture(t, 4096, 1, 7)
	if code, _ := s.handle(transition.Command{V: transition.ProtocolVersion, Seq: 1, Kind: transition.CmdPrepare}); code != 0 {
		t.Fatalf("prepare code = %d", code)
	}
	if code, _ := s.handle(leaseCmd(transition.CmdLeaseWrite, 2, 0, 1)); code != 0 {
		t.Fatalf("lease-write code = %d", code)
	}
	code, done := s.handle(leaseCmd(transition.CmdLeaseRead, 3, 0, 2))
	if code != 1 || !done {
		t.Fatalf("stale-generation read = (%d,%v), want (1,true)", code, done)
	}
	got := leaseAcks(t, acks)
	if last := got[len(got)-1]; last.Kind != transition.AckError || !strings.Contains(last.Msg, "gen=2") {
		t.Errorf("final ack = %+v, want ERROR naming the requested generation", last)
	}
}

// Wiring refusals: leases on a non-handoff session, leases without
// payload, blocks outside the section, and leases before PREPARE are
// all parent-side bugs: ERROR acks, session continues, nothing
// allocated or written. (Ownership itself is the parent ARBITER's
// decision - the worker's guard is the section bound.)
func TestSessionLeaseRefusals(t *testing.T) {
	// Non-handoff session (ordinary private engine, tiny grants).
	m, s, acks := leaseFixture(t, 4096, 1, 0)
	s.p.Handoff = false // same params, but the session never opted in
	if code, _ := s.handle(transition.Command{V: transition.ProtocolVersion, Seq: 1, Kind: transition.CmdPrepare}); code != 0 {
		t.Fatalf("prepare code = %d", code)
	}
	if code, done := s.handle(leaseCmd(transition.CmdLeaseWrite, 2, 0, 1)); code != 0 || done {
		t.Fatalf("lease on non-handoff session = (%d,%v), want refusal without ending the session", code, done)
	}
	_ = m
	got := leaseAcks(t, acks)
	if last := got[len(got)-1]; last.Kind != transition.AckError || !strings.Contains(last.Msg, "not a handoff session") {
		t.Errorf("refusal ack = %+v", last)
	}

	// Handoff session: payload, ownership and ordering refusals.
	_, s2, acks2 := leaseFixture(t, 4096, 1, 0)
	if code, done := s2.handle(leaseCmd(transition.CmdLeaseRead, 1, 0, 1)); code != 0 || done {
		t.Fatalf("lease before prepare = (%d,%v), want (0,false)", code, done)
	}
	nop := transition.Command{V: transition.ProtocolVersion, Seq: 2, Kind: transition.CmdLeaseWrite}
	if code, done := s2.handle(nop); code != 0 || done {
		t.Fatalf("payload-less lease = (%d,%v), want (0,false)", code, done)
	}
	if code, _ := s2.handle(transition.Command{V: transition.ProtocolVersion, Seq: 1, Kind: transition.CmdPrepare}); code != 0 {
		t.Fatalf("prepare code = %d", code)
	}
	if code, done := s2.handle(leaseCmd(transition.CmdLeaseRead, 3, 5, 1)); code != 0 || done {
		t.Fatalf("foreign block lease = (%d,%v), want (0,false)", code, done)
	}
	got2 := leaseAcks(t, acks2)
	wantMsgs := []string{"prepared engine", "block/gen payload", "outside the shared section"}
	var errs []string
	for _, a := range got2 {
		if a.Kind == transition.AckError {
			errs = append(errs, a.Msg)
		}
	}
	if len(errs) != 3 {
		t.Fatalf("error acks = %d (%v), want 3", len(errs), errs)
	}
	for i, w := range wantMsgs {
		if !strings.Contains(errs[i], w) {
			t.Errorf("refusal %d = %q, want it to mention %q", i, errs[i], w)
		}
	}
}

// Handoff PREPARE refuses a corrupted command line (no shared size)
// and non-uniform grants (block ids are offset/size, so uniformity
// is part of the contract): setup condition, ERROR ack, exit 2.
func TestSessionHandoffPrepareRefusals(t *testing.T) {
	_, s, acks := leaseFixture(t, 4096, 1, 0)
	s.p.SharedSize = 0
	if code, _ := s.handle(transition.Command{V: transition.ProtocolVersion, Seq: 1, Kind: transition.CmdPrepare}); code != 2 {
		t.Errorf("prepare without shared size = %d, want 2", code)
	}
	got := leaseAcks(t, acks)
	if last := got[len(got)-1]; last.Kind != transition.AckError {
		t.Errorf("expected an ERROR ack for the refused prepare, got %+v", last)
	}

	_, s2, _ := leaseFixture(t, 4096, 2, 0)
	s2.p.grants[1].Requested = 8192 // non-uniform
	if code, _ := s2.handle(transition.Command{V: transition.ProtocolVersion, Seq: 1, Kind: transition.CmdPrepare}); code != 2 {
		t.Errorf("prepare with non-uniform grants = %d, want 2", code)
	}
}

// The worker command line refuses handoff flags outside their
// contract BEFORE doing any real work: -handoff without -session,
// and -handoff without the inherited section, are corrupted
// contracts, exit 2.
func TestWorkerHandoffFlagRefusals(t *testing.T) {
	if code := WorkerCmd([]string{"-handoff", "-core", "0", "-workload", "ram-handoff", "-duration", "1s"}); code != 2 {
		t.Errorf("-handoff without -session: exit %d, want 2", code)
	}
	if code := WorkerCmd([]string{"-session", "-handoff", "-core", "0", "-workload", "ram-handoff"}); code != 2 {
		t.Errorf("-handoff without shared section: exit %d, want 2", code)
	}
}
