package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

// G01 (menu plan section 9): the managed event stream. stdout in
// managed mode carries exactly one JSON record per line from one
// serializer - no human banners, no ANSI, nothing that was not
// marshaled here. Human diagnostics keep flowing to stderr (the
// headless entry swaps os.Stdout for it after capturing the real
// stream for this sink).

const eventSchemaVersion = 1

// eventEnvelope is the one shape every stream record wears:
// schema_version, seq, session_id, run_id/phase_id when present,
// UTC timestamp, type, payload. A GUI must ignore event types it
// does not know (documented forward compatibility) - new types are
// additive, never a redefinition.
type eventEnvelope struct {
	SchemaVersion int    `json:"schema_version"`
	Seq           int    `json:"seq"`
	SessionID     string `json:"session_id,omitempty"` // empty only before the session identity exists
	PhaseID       string `json:"phase_id,omitempty"`   // run_id stays absent: the CLI never sees the journal's run id
	TS            string `json:"ts"`
	Type          string `json:"type"`
	Payload       any    `json:"payload,omitempty"`
}

// eventSink serializes stream records onto a bounded queue and lets
// one writer goroutine own the real stdout. A slow or dead reader
// must never block the run: droppable records (progress heartbeats)
// are dropped and counted, and once the stream is broken everything
// is dropped and counted - the durable result file and the session
// manifest are what survives a GUI that stopped reading.
type eventSink struct {
	w       chan string
	stop    chan struct{} // closed by close(): the run is over, nothing more ships
	done    chan struct{} // closed by the writer once the queue is drained
	broken  atomic.Bool
	dropped atomic.Int64

	real *os.File // the stream this sink owns (see newEventSink)

	closeOnce sync.Once // one-shot close handshake
	closed    bool      // under mu: admission is over, late emits drop

	mu      sync.Mutex
	seq     int
	session string
	phase   string

	t0 time.Time // monotonic reference for elapsed durations (wall clock never measures latency)

	// drainEnd is read and written only by the writer goroutine:
	// the absolute budget for shipping what was already enqueued.
	drainEnd time.Time
}

// The stream's I/O is bounded end to end (audit B07): one write to
// the real stdout gets a deadline where the platform supports one,
// the post-stop drain gets one absolute budget, and the close
// handshake gets a belt that cancels an un-interruptible kernel
// write by closing the handle this sink owns (probed: on Windows an
// anonymous pipe has no deadlines, and closing the write handle
// returns the pending write at once with "file already closed").
// A GUI that holds the pipe open without reading must never hang
// the CLI, its durable result, or the run lock. Tests shorten these.
var (
	eventWriteTimeout = 5 * time.Second
	eventDrainBudget  = 10 * time.Second
	eventCloseBelt    = 5 * time.Second
)

// newEventSink takes ownership of the real stdout stream. The
// caller's swap of os.Stdout to stderr must happen after this call:
// everything human then flows to stderr while the sink keeps the
// pipe. Ownership is real: a stuck write on a handle the platform
// cannot deadline (the Windows anonymous pipe - probed) is
// cancelled at teardown by closing this handle - probed too: the
// pending write returns at once with "file already closed".
func newEventSink(real *os.File) *eventSink {
	s := &eventSink{w: make(chan string, 256), stop: make(chan struct{}), done: make(chan struct{}), real: real, t0: time.Now()}
	go s.run()
	return s
}

func (s *eventSink) run() {
	defer close(s.done)
	for {
		select {
		case line := <-s.w:
			s.deliver(line, false)
		case <-s.stop:
			// The run is over: ship what was already enqueued
			// (the completed record), under one absolute budget -
			// a reader that stopped draining must not hold the
			// process open forever.
			s.drainEnd = time.Now().Add(eventDrainBudget)
			for {
				select {
				case line := <-s.w:
					s.deliver(line, true)
				default:
					return
				}
			}
		}
	}
}

// deliver writes one line. Where the platform supports a write
// deadline (Unix pipes, sockets, named pipes) every write carries
// one, and the handle's deadline is cleared again afterwards - the
// stream never leaves state behind. Where it does not (Windows
// anonymous pipe, console), the write is still safe to launch here
// because this sink owns the stream and close() owns the
// cancellation: the belt closes the handle, the pending write
// returns at once, and the writer retires with an honest broken
// note. Nothing ever waits on an un-cancelable write forever.
func (s *eventSink) deliver(line string, draining bool) {
	if s.broken.Load() {
		s.dropped.Add(1) // queued behind a dead stream: counted, never silently lost
		return           // the run must reach its durable result, not stall on a full queue
	}
	deadline := eventWriteTimeout
	if draining {
		left := time.Until(s.drainEnd)
		if left <= 0 {
			s.markBroken("event stream drain budget exhausted")
			return
		}
		if left < deadline {
			deadline = left
		}
	}
	if err := s.real.SetWriteDeadline(time.Now().Add(deadline)); err == nil {
		defer s.real.SetWriteDeadline(time.Time{})
	}
	if _, err := s.real.WriteString(line + "\n"); err != nil {
		s.markBroken(fmt.Sprintf("stdout write failed: %v", err))
	}
}

// markBroken ends the stream once, loudly, and only once: after
// this every record is dropped and counted while the run keeps
// going for its durable result.
func (s *eventSink) markBroken(why string) {
	if s.broken.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "warning: event stream broken (%s); the run continues and the durable result still lands\n", why)
	}
}

// setSession binds the session identity (the tracker mints it; the
// stream names it on every later record). Nil-safe like every other
// hook: unmanaged runs keep the pointer nil.
func (s *eventSink) setSession(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.session = id
	s.mu.Unlock()
}

func (s *eventSink) setPhase(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.phase = id
	s.mu.Unlock()
}

// emit marshals and enqueues one record. droppable marks the
// coalescable heartbeats (progress/telemetry): a full queue drops
// them with a count instead of applying backpressure to the run.
// Findings and the final result are never droppable - but even they
// only reach the stream, never a guarantee the GUI was listening:
// durability lives in the result file and the manifest.
func (s *eventSink) emit(typ, phaseID string, payload any, droppable bool) {
	if s == nil {
		return
	}
	if droppable {
		// A heartbeat never queues behind a stalled non-droppable
		// emit: if the admission lock is busy the beat is simply
		// dropped and counted.
		if !s.mu.TryLock() {
			s.dropped.Add(1)
			return
		}
		defer s.mu.Unlock()
		s.emitLocked(typ, phaseID, payload, true)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emitLocked(typ, phaseID, payload, false)
}

// emitLocked assigns the sequence number and admits the record to
// the queue under the same mutex (audit B07: seq was once handed out
// under the lock but enqueued after releasing it, so two concurrent
// emits could reach the wire out of order). The writer goroutine
// never takes this mutex, so a bounded send here cannot deadlock
// it; the admission check and the enqueue are one atomic decision.
func (s *eventSink) emitLocked(typ, phaseID string, payload any, droppable bool) {
	s.seq++
	env := eventEnvelope{
		SchemaVersion: eventSchemaVersion,
		Seq:           s.seq,
		SessionID:     s.session,
		PhaseID:       phaseID,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Type:          typ,
		Payload:       payload,
	}
	line, err := json.Marshal(env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: event %s not encoded: %v\n", typ, err)
		return
	}
	if s.closed || s.broken.Load() {
		s.dropped.Add(1)
		return
	}
	if droppable {
		select {
		case s.w <- string(line):
		default:
			s.dropped.Add(1)
		}
		return
	}
	select {
	case s.w <- string(line):
	case <-s.stop:
		// The run already ended and the writer shipped its last:
		// nothing after the final record is promised to the wire.
		s.dropped.Add(1)
	case <-time.After(10 * time.Second):
		// A reader that never drains and never closes must not hang
		// the run forever: after a bounded wait the stream is
		// declared broken and the run finishes its durable result.
		s.dropped.Add(1)
		s.markBroken("event stream stalled for 10s; declared broken, the durable result still lands")
	}
}

// close retires the sink: the writer ships everything already
// enqueued (the completed record first of all, it was emitted
// before close was called) under one absolute drain budget, and the
// handshake guarantees the stream is finished - delivered or broken -
// before the process exits. Admission is closed under the same mutex
// the sequence numbers use, so a late emit (a stdin command racing
// the teardown) is dropped and counted, never a panic and never a
// record after the final one. The queue itself is never closed.
// If the writer sits in a write the platform cannot interrupt
// (Windows anonymous pipe: no deadlines - probed), the belt closes
// this sink's own stream handle: the pending write returns at once
// (probed: "file already closed"), the writer retires with an
// honest note, and no goroutine outlives the handshake. The durable
// result landed before close was ever called.
func (s *eventSink) close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.stop)
		select {
		case <-s.done:
		case <-time.After(eventCloseBelt):
			fmt.Fprintln(os.Stderr, "warning: event stream writer stuck; cancelling its write by closing the stream handle")
			s.real.Close()
			select {
			case <-s.done:
			case <-time.After(2 * time.Second):
				fmt.Fprintln(os.Stderr, "warning: event stream writer did not retire; the durable result and journal carry the story")
			}
		}
	})
}

// currentEvents is the process-wide stream sink, set only by the
// managed entry point. One run per process is the G01 policy (the
// plan: concurrent run sessions are forbidden in v1), so a single
// pointer cannot race with another session - and every hook is
// nil-safe, so unmanaged runs pay nothing.
var currentEvents *eventSink

// --- typed payloads: the stream's vocabulary is structs, not maps,
// so the JSON fixtures (G01d) pin exactly what the GUI may rely on.
// Core keys never carry omitempty: absence would hide schema drift
// behind empty values; "" and null are the honest "unknown".

type resolvedPayload struct {
	Runs       int      `json:"runs"`
	PlanHashes []string `json:"plan_hashes"`
}

type startedPayload struct {
	Runs   int `json:"runs"`
	Cycles int `json:"cycles"` // sessions are one cycle by definition
}

type sessionPhaseStartedPayload struct {
	LogicalCPUs []int `json:"logical_cpus"`
}

type scenarioPhaseStartedPayload struct {
	Test string `json:"test"`
	A    string `json:"a"`
	B    string `json:"b"`
}

// handoffPhaseStartedPayload names the experiment the tracked handoff
// run (S6) just started: the geometry IS the criterion (no clock, no
// memory knob), so the cores, blocks, block size, rounds and seed are
// what a GUI can show while the lease steps stream in. The values are
// the manifest's, echoed honestly from what is actually running.
type handoffPhaseStartedPayload struct {
	Cores      []int  `json:"cores"`
	Blocks     int    `json:"blocks"`
	BlockBytes uint64 `json:"block_bytes"`
	Rounds     int    `json:"rounds"`
	Seed       uint64 `json:"seed"`
}

type phaseFinishedPayload struct {
	Exit       int    `json:"exit"`
	Execution  string `json:"execution"`
	Validation string `json:"validation"`
	Findings   int    `json:"findings"`
	Abort      string `json:"abort"` // "" = the run ended on its own terms
}

type progressPayload struct {
	ElapsedMs int64 `json:"elapsed_ms"` // monotonic since the stream began
}

type cancellingPayload struct {
	Reason string `json:"reason"`
}

type cancelAckPayload struct {
	CommandID         string `json:"command_id"`
	AlreadyCancelling bool   `json:"already_cancelling"`
}

type commandErrorPayload struct {
	Type      string `json:"type"` // "" = the line named no type
	CommandID string `json:"command_id"`
	Message   string `json:"message"`
}

type errorPayload struct {
	Message string `json:"message"`
}

// handoffLeasePayload is one P07e lease step on the stream (S4): the
// parent's view of the step, field-for-field the session_lease
// journal record it mirrors (same LeaseObservation value the
// coordinator journaled - the stream can never drift from the
// journal). It is DROPPABLE and that is stated honesty: the journal
// is the complete order artifact the S3 replay verifies, the stream
// is the live view - a gap is detectable in the (block, gen)
// progression and counted in dropped_events. Blocking the lease loop
// to feed a stalled GUI would make the observer part of the test,
// which is exactly what this tool refuses.
type handoffLeasePayload struct {
	Core     int     `json:"core"` // the worker that ran this lease
	Session  string  `json:"session"`
	Seq      uint64  `json:"seq"`
	Block    uint64  `json:"block"`
	Gen      uint64  `json:"gen"`
	Mode     string  `json:"mode"` // lease-write | lease-read
	PeerCore *int    `json:"peer_core"`
	ToAckMS  float64 `json:"to_ack_ms"` // commanded -> ack, upper bound incl IPC
	Checks   *uint64 `json:"checks"`    // null = the ack did not report it (unknown, never zero)
	Bytes    *uint64 `json:"bytes"`
	Acked    bool    `json:"acked"`
	Error    string  `json:"error"` // "" = the expected ack arrived
}

// emitHandoffLease reports one lease step to the live stream (bound
// to the seam by handoffLeaseEmitter). Nil-safe like every hook:
// unmanaged runs pay nothing.
func emitHandoffLease(core int, obs journal.LeaseObservation) {
	if currentEvents == nil {
		return
	}
	currentEvents.emit("handoff_lease", "", handoffLeasePayload{
		Core:     core,
		Session:  obs.Session,
		Seq:      obs.Seq,
		Block:    obs.Block,
		Gen:      obs.Gen,
		Mode:     obs.Mode,
		PeerCore: obs.PeerCore,
		ToAckMS:  obs.ToAckMS,
		Checks:   obs.Checks,
		Bytes:    obs.Bytes,
		Acked:    obs.Acked,
		Error:    obs.Error,
	}, true)
}

// handoffLeaseEmitter binds the coordinator's lease seam to the live
// event stream (S4): with no stream there is no seam at all, and the
// coordinator's loop pays not even an indirect call.
func handoffLeaseEmitter() func(int, journal.LeaseObservation) {
	if currentEvents == nil {
		return nil
	}
	return emitHandoffLease
}

// findingPayload is the wire view of one plan.Finding: snake_case
// like every other protocol field, and the attribution travels
// intact - a finding stripped of its attribution is a clue stripped
// of its meaning.
type findingPayload struct {
	Kind        string            `json:"kind"`
	Source      string            `json:"source"`
	Message     string            `json:"message"`
	Attribution findingAttribWire `json:"attribution"`
}

type findingAttribWire struct {
	Kind       string `json:"kind"`
	Index      *int   `json:"index"`  // null = not attributed to an index
	Region     string `json:"region"` // "" = none named
	Confidence string `json:"confidence"`
	ObservedOn *int   `json:"observed_on"` // null = not recorded
}

func findingOf(f plan.Finding) findingPayload {
	return findingPayload{
		Kind:    string(f.Kind),
		Source:  f.Source,
		Message: f.Message,
		Attribution: findingAttribWire{
			Kind:       string(f.Attribution.Kind),
			Index:      f.Attribution.Index,
			Region:     f.Attribution.Region,
			Confidence: string(f.Attribution.Confidence),
			ObservedOn: f.Attribution.ObservedOn,
		},
	}
}

// emitPhaseResult reports one finished child run to the stream: the
// structured outcome as phase_finished, then one finding record per
// finding - findings are never summarized away and never droppable.
func emitPhaseResult(phaseID string, res plan.Result) {
	if currentEvents == nil {
		return
	}
	currentEvents.emit("phase_finished", phaseID, phaseFinishedPayload{
		Exit:       res.Exit,
		Execution:  string(res.Execution),
		Validation: string(res.Validation),
		Findings:   len(res.Findings),
		Abort:      res.AbortMsg,
	}, false)
	for _, f := range res.Findings {
		currentEvents.emit("finding", phaseID, findingOf(f), false)
	}
	currentEvents.setPhase("")
}

// startHeartbeat emits a droppable progress record every 5s while
// the run lives: elapsed monotonic milliseconds against the phase
// the stream last saw. It says "still running", nothing more.
func startHeartbeat() (stop func()) {
	s := currentEvents // captured: the teardown may nil the package variable while this ticker still fires
	if s == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s.mu.Lock()
				phase := s.phase
				s.mu.Unlock()
				s.emit("progress", phase, progressPayload{
					ElapsedMs: time.Since(s.t0).Milliseconds(),
				}, true)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}
