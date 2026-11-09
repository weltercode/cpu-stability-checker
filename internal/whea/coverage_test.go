package whea

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// P13 coverage proof: Proven - the right to claim "no WHEA records in
// this window" - attaches only after a FULL query cycle (newest
// probe + oldest probe + cursor scan) has succeeded. The start-of-
// window baseline probe proves the channel answers, nothing more.
//
// The oldest() probe is gated so "cycle started, cycle not done" is
// a guaranteed observation point: on the first Actions runner this
// test went red NOT because the monitor proved coverage too early
// (the contract held: Proven arrived only after a completed cycle)
// but because starved runner goroutine scheduling let the whole cycle
// finish before the test's first state read. Asserting an
// intermediate state that may never be observable is timing luck; a
// gated fake makes the ordering the thing under test.
func TestCoverageProvenOnlyAfterFullCycle(t *testing.T) {
	fq := &fakeQ{newestID: 100, oldestID: 1}
	gate := make(chan struct{})
	fq.gate = gate
	m := &Monitor{ch: make(chan Event, 16), stop: make(chan struct{})}
	go func() {
		for range m.ch {
		}
	}()
	go m.poll(fq, 10*time.Millisecond, -1)

	wait := func(cond func(State) bool, why string) State {
		deadline := time.Now().Add(2 * time.Second)
		for {
			st := m.State()
			if cond(st) {
				return st
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s (state %+v)", why, st)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	st := wait(func(s State) bool { return s.Available }, "baseline probe never landed")
	if st.Proven {
		t.Fatalf("the baseline probe alone must not prove coverage: %+v", st)
	}
	close(gate) // only now may the full cycle pass its oldest probe
	wait(func(s State) bool { return s.Proven }, "a completed full cycle must prove coverage")
	m.Close()
}

// A window on the owner-machine pattern (every cursor query fails,
// e.g. wevtutil exit 15001) must never claim Proven: its final state
// is a failed-coverage state carrying the tool's own reason, never a
// clean "no records".
func TestCoverageNeverProvenWhenQueriesAlwaysFail(t *testing.T) {
	fq := &fakeQ{newestID: 100, oldestID: 1, failQ: errors.New("exit status 15001: wevtutil: The specified query is invalid.")}
	m := &Monitor{ch: make(chan Event, 16), stop: make(chan struct{})}
	go func() {
		for range m.ch {
		}
	}()
	go m.poll(fq, 10*time.Millisecond, -1)
	time.Sleep(150 * time.Millisecond)
	m.Close()
	st := m.State()
	if st.Proven {
		t.Errorf("a window whose cursor query never succeeded must never be Proven: %+v", st)
	}
	if !st.WindowDegraded {
		t.Errorf("failed queries must mark the window degraded: %+v", st)
	}
	if !strings.Contains(st.WindowReason, "15001") {
		t.Errorf("window reason must carry the tool's exit: %q", st.WindowReason)
	}
	if st.Failures == 0 {
		t.Errorf("failed queries must be counted, not swallowed: %+v", st)
	}
}
