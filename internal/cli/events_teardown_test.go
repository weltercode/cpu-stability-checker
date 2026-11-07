package cli

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// B07 (audit): the event stream's I/O must be bounded end to end.
// A GUI that holds the stdout pipe open and never reads must not
// hang the CLI, its durable result, or the run lock; concurrent
// emits must never reach the wire out of sequence order.

// shortenEventBudgets pulls the write deadline and drain budget to
// test scale and restores the production values afterwards.
func shortenEventBudgets(t *testing.T) {
	t.Helper()
	wt, db, cb := eventWriteTimeout, eventDrainBudget, eventCloseBelt
	eventWriteTimeout, eventDrainBudget, eventCloseBelt = 50*time.Millisecond, 200*time.Millisecond, 2*time.Second
	t.Cleanup(func() { eventWriteTimeout, eventDrainBudget, eventCloseBelt = wt, db, cb })
}

// The audit's probe: a 1 MiB record into a pipe nobody reads, with
// the read end left open. The old deliver blocked in WriteString
// forever and close() waited on the writer forever. Now every write
// carries a deadline, the drain carries a budget, and close returns
// bounded with the stream honestly declared broken.
func TestEventSinkUnreadPipeTeardownIsBounded(t *testing.T) {
	shortenEventBudgets(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	s := newEventSink(w)
	big := strings.Repeat("x", 1<<20)
	s.emit("test_blob", "", big, false)
	s.emit("test_blob", "", big, false)
	s.emit("beat", "", "x", true)

	done := make(chan struct{})
	go func() { s.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		r.Close()
		t.Fatal("close() hung on an unread pipe: teardown is not bounded")
	}
	if !s.broken.Load() {
		t.Error("an undeliverable stream was not declared broken")
	}
	if s.dropped.Load() == 0 {
		t.Error("dropped records were not counted")
	}
	select {
	case <-s.done:
	default:
		t.Error("close() returned while the writer goroutine was still live")
	}
	// The read end must not hang forever either: on the cancel path
	// the sink closed its own handle (EOF), on the deadline path the
	// handle stays open (read blocks) - both are honest endings, a
	// hang is not.
	rdone := make(chan struct{})
	go func() { io.Copy(io.Discard, r); close(rdone) }()
	select {
	case <-rdone:
	case <-time.After(2 * time.Second):
	}
	r.Close()
	w.Close()
}

// B07's reorder probe: seq was handed out under the mutex but the
// enqueue happened after releasing it, so concurrent emitters could
// land on the wire out of order. Now admission and seq share one
// critical section: the wire sequence is strictly increasing and
// complete.
func TestEventSinkConcurrentEmitKeepsSeqOrder(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	lines := make(chan string, 1024)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	s := newEventSink(w)
	var wg sync.WaitGroup
	const emitters, each = 8, 25
	for g := 0; g < emitters; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				s.emit("beat", "", map[string]int{"g": g}, false)
			}
		}(g)
	}
	wg.Wait()
	s.close()
	w.Close()

	prev, seen := 0, 0
	for line := range lines {
		var env eventEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("stdout line is not one JSON record: %v\n%s", err, line)
		}
		if env.Seq <= prev {
			t.Fatalf("wire sequence out of order: %d after %d", env.Seq, prev)
		}
		prev = env.Seq
		seen++
	}
	if seen != emitters*each {
		t.Errorf("records on the wire = %d, want %d", seen, emitters*each)
	}
	if prev != emitters*each {
		t.Errorf("last sequence = %d, want %d (sequence must be complete)", prev, emitters*each)
	}
}

// After close the stream is closed for admission under the same
// mutex the sequence numbers use: late emits (a stdin command
// racing the teardown) are counted, never sent, never a panic -
// and close itself is idempotent.
func TestEventSinkLateEmitAfterCloseIsCountedNotSent(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := newEventSink(w)
	s.emit("first", "", "x", false)
	s.close()
	s.close() // idempotent: no panic, no double close of stop
	s.emit("late", "", "x", false)
	s.emit("late-beat", "", "x", true)
	if s.dropped.Load() < 2 {
		t.Errorf("late emits not counted as dropped: %d", s.dropped.Load())
	}
	w.Close() // the run ended: the scanner must now see exactly one record
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
	}
	if n != 1 {
		t.Errorf("records after close reached the wire: %d (want only the pre-close one)", n)
	}
}

// Droppable heartbeats never apply backpressure: with the queue
// full and the pipe unread, they return at once and are counted.
func TestEventSinkDroppableNeverBlocksFullQueue(t *testing.T) {
	shortenEventBudgets(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := newEventSink(w)
	start := time.Now()
	for i := 0; i < 400; i++ {
		s.emit("beat", "", strings.Repeat("y", 4096), true)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("droppable emits blocked on a full queue (%v)", elapsed)
	}
	if s.dropped.Load() == 0 {
		t.Error("a full queue dropped nothing")
	}
	done := make(chan struct{})
	go func() { s.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("close hung behind a blocked writer")
	}
	w.Close()
}

// The writer goroutine must retire: after close returns, the sink
// has no live writer left (the durable result path never depends on
// a goroutine that outlives the handshake except at real process
// exit, which the belt covers honestly).
func TestEventSinkWriterRetires(t *testing.T) {
	shortenEventBudgets(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := newEventSink(w)
	s.emit("first", "", strings.Repeat("z", 1<<20), false) // will block the writer
	s.close()                                              // bounded by the budgets
	select {
	case <-s.done:
	default:
		t.Error("close() returned while the writer goroutine was still live")
	}
	w.Close()
}
