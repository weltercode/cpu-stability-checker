package subprocess

// P08 parent-side session driver: spawn ONE prepared worker, drive it
// through many verified bursts with the transition state machine over
// JSONL pipes, and always leave no process behind (bounded STOP,
// then kill, then reap - a session that ends any other way must not
// orphan a running or frozen child).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/procjob"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
)

// SessionHandle is one live worker session. Not safe for concurrent
// use: the session loop that owns a burst schedule is the single
// driver, exactly like the rotate scheduler it parallels.
type SessionHandle struct {
	Cmd     *exec.Cmd
	Machine *transition.Machine
	Stderr  *bytes.Buffer // the worker's human messages, kept for the report

	stdin      io.WriteCloser
	stdout     io.ReadCloser
	job        *procjob.Job
	ackCh      chan transition.Ack
	done       chan error // set by reap: process wait result
	readerStop chan struct{}
	readerDone chan struct{}
	reaped     bool
	exit       int
	// killErr records a forced kill the OS REFUSED during Reap (nil
	// when the kill was accepted or the process had already left).
	// The reap promise is that a lingering worker is FORCED, never
	// waited out; without this record a refused kill and a clean
	// forced reap look identical in the report.
	killErr error
}

// StartSession spawns a --session worker. argv are the worker flags
// (core, workload, journal, run-id, grants...); the test seams
// selfExecutable/workerArgsPrefix apply exactly as in one-shot mode.
// The caller's ctx does NOT kill the child: cleanup is explicit
// (Stop/Reap) so a cancel can first ask the worker to release its
// memory - killing a worker mid-allocation without asking is exactly
// the sloppy resource handling this project refuses.
func StartSession(argv []string, sessionID string, timeouts transition.Timeouts) (*SessionHandle, error) {
	return StartSessionShared(argv, sessionID, timeouts, 0)
}

// StartSessionShared spawns a --session worker that also inherits
// the P07e shared section: sharedHandle (0 = none) is passed via
// AdditionalInheritedHandles, and the argv must carry the matching
// --handoff/--mem-shared-handle/--mem-shared-size flags (the caller
// builds them: the handle VALUE is what the child's OpenSharedMapping
// needs, and argv is how it learns it).
func StartSessionShared(argv []string, sessionID string, timeouts transition.Timeouts, sharedHandle uintptr) (*SessionHandle, error) {
	exe, err := workerExecutable()
	if err != nil {
		return nil, err
	}
	args := append(append([]string{}, workerArgsPrefix...), argv...)
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), workerChildEnv+"=1")
	if sharedHandle != 0 {
		applyInheritedHandle(cmd, sharedHandle)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("session: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("session: stdout pipe: %w", err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	job, err := startWorker(cmd)
	if err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("session: spawn worker: %w", err)
	}
	h := &SessionHandle{
		Cmd:        cmd,
		Machine:    transition.NewMachine(sessionID, nil, timeouts),
		Stderr:     stderr,
		stdin:      stdin,
		stdout:     stdout,
		job:        job,
		ackCh:      make(chan transition.Ack),
		readerStop: make(chan struct{}),
		readerDone: make(chan struct{}),
	}
	go func() {
		defer close(h.readerDone)
		defer close(h.ackCh)
		dec := json.NewDecoder(stdout)
		for {
			var a transition.Ack
			if err := dec.Decode(&a); err != nil {
				return
			}
			select {
			case h.ackCh <- a:
			case <-h.readerStop:
				return
			}
		}
	}()
	return h, nil
}

// Command stamps the command through the machine (which marks the
// trigger BEFORE returning, so the timestamp never includes the pipe
// write) and then writes it.
func (h *SessionHandle) Command(kind transition.Kind) (transition.Command, error) {
	cmd, err := h.Machine.Send(kind)
	if err != nil {
		return cmd, err
	}
	if err := json.NewEncoder(h.stdin).Encode(cmd); err != nil {
		return cmd, fmt.Errorf("session: send %s: %w", kind, err)
	}
	return cmd, nil
}

// CommandLease stamps a P07e block-ownership lease through the
// machine (which refuses unless this session opted into leases) and
// writes it. Same ordering rule as Command: the machine marks the
// command BEFORE the pipe write.
func (h *SessionHandle) CommandLease(kind transition.Kind, block, gen uint64) (transition.Command, error) {
	cmd, err := h.Machine.SendLease(kind, block, gen)
	if err != nil {
		return cmd, err
	}
	if err := json.NewEncoder(h.stdin).Encode(cmd); err != nil {
		return cmd, fmt.Errorf("session: send %s: %w", kind, err)
	}
	return cmd, nil
}

// Await reads acks until the machine has applied (seq, kind).
// Unsolicited IDLE_ACKs (seq 0) feed Machine.IdleAck and keep the
// read going; ERROR acks return an error carrying the worker's own
// reason; every classification (late/duplicate/stale/unexpected) is
// recorded by the machine on the way. On timeout the machine's Tick
// records the missed deadline once and the error says so - the
// machine never retried, and neither do we here. The matched ack is
// returned so callers can read its payload (VERIFIED counts).
func (h *SessionHandle) Await(seq uint64, kind transition.Kind, timeout time.Duration) (transition.Ack, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case a, ok := <-h.ackCh:
			if !ok {
				return a, errors.New("session: worker channel closed before " + string(kind) + " (seq " + fmt.Sprint(seq) + ")")
			}
			if a.Seq == 0 && a.Kind == transition.AckIdle {
				h.Machine.IdleAck()
				continue
			}
			if err := h.Machine.Ack(a); err != nil {
				return a, err
			}
			if a.Kind == transition.AckError {
				return a, &transition.WorkerError{Msg: a.Msg}
			}
			if a.Seq == seq && a.Kind == kind {
				return a, nil
			}
		case <-timer.C:
			h.Machine.Tick()
			return transition.Ack{}, fmt.Errorf("session: no %s ack for seq %d within %s (missed deadline recorded)", kind, seq, timeout)
		}
	}
}

// AwaitIdle waits for the machine to observe IDLE_ACK (the worker is
// blocked on its command channel). Used after READY and after
// VERIFIED, where the idle ack is unsolicited state, not a command
// answer.
func (h *SessionHandle) AwaitIdle(timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for h.Machine.State() != transition.Idle {
		select {
		case a, ok := <-h.ackCh:
			if !ok {
				return errors.New("session: worker channel closed before idle")
			}
			if a.Seq == 0 && a.Kind == transition.AckIdle {
				h.Machine.IdleAck()
			} else if err := h.Machine.Ack(a); err != nil {
				return err
			} else if a.Kind == transition.AckError {
				return &transition.WorkerError{Msg: a.Msg}
			}
			// Any ack can arrive while waiting for idle (a late
			// answer to an earlier command, say): the machine
			// classifies it; we keep waiting for the state.
		case <-timer.C:
			h.Machine.Tick()
			return fmt.Errorf("session: worker not idle within %s (state %s)", timeout, h.Machine.State())
		}
	}
	return nil
}

// WaitWindow waits out a burst window while LISTENING: if the
// worker sends an ERROR ack (a fault with no VERIFY pending) or the
// channel closes mid-window, the driver learns it NOW, not after a
// doomed command hits a broken pipe. Returns (windowElapsed, err):
// err is non-nil when the worker errored or vanished; windowElapsed
// is false when ctx was cancelled first (the caller then aborts the
// burst honestly via IDLE). Unsolicited IDLE_ACKs keep the machine
// honest on the way. Single reader: the caller must not race another
// Await against the same window.
func (h *SessionHandle) WaitWindow(ctx context.Context, d time.Duration) (bool, error) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			return true, nil
		case <-ctx.Done():
			return false, nil
		case a, ok := <-h.ackCh:
			if !ok {
				return true, errors.New("session: worker channel closed mid-burst")
			}
			if a.Seq == 0 && a.Kind == transition.AckIdle {
				h.Machine.IdleAck()
				continue
			}
			if err := h.Machine.Ack(a); err != nil {
				return true, err
			}
			if a.Kind == transition.AckError {
				return true, &transition.WorkerError{Msg: a.Msg}
			}
			// Any other ack while no command is pending: the
			// machine classified it (stale/duplicate/...); we keep
			// waiting out the window.
		}
	}
}

// Stop asks the worker to end the session cleanly and waits for the
// STOPPED ack. It returns the Await error if the ack never came -
// Reap afterwards still guarantees no process survives.
func (h *SessionHandle) Stop(timeout time.Duration) error {
	cmd, err := h.Command(transition.CmdStop)
	if err != nil {
		return err
	}
	_, err = h.Await(cmd.Seq, transition.AckStopped, timeout)
	return err
}

// Reap closes the command channel (a worker that loses its parent
// ends the session as incomplete, never as "ok") and waits bounded
// for the process; if it lingers, it is killed and reaped. Either
// way this returns the worker's exit code and no process is left
// running. Close-safe: calling it twice returns the same code.
func (h *SessionHandle) Reap(bounded time.Duration) int {
	if h.reaped {
		return h.exit
	}
	// Release the decoder even if it is delivering an unsolicited IDLE_ACK
	// that the owner will never read after a timeout or cancellation.
	close(h.readerStop)
	_ = h.stdout.Close()
	defer func() {
		_ = h.job.Close()
		<-h.readerDone
		h.reaped = true
	}()
	_ = h.stdin.Close()
	if h.done == nil {
		h.done = make(chan error, 1)
		go func() { h.done <- h.Cmd.Wait() }()
	}
	select {
	case err := <-h.done:
		h.exit = sessionExit(err)
	case <-time.After(bounded):
		if err := h.Cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			// The OS refused the forced kill. The wait below may now
			// run until the worker's natural end, and that outcome
			// must not masquerade as a forced one (audit R11): keep
			// the error visible to callers and to the tests.
			h.killErr = err
		}
		_ = h.job.Close()
		h.exit = sessionExit(<-h.done)
	}
	return h.exit
}

// KillFailure reports a forced kill that Reap had to attempt but the
// OS refused (nil when the kill was accepted, already done, or never
// needed). It exists so a caller can tell a slow OS apart from a
// broken promise: a lingering worker that ends at its natural exit
// after a refused kill is an honest failure, never a success.
func (h *SessionHandle) KillFailure() error { return h.killErr }

func sessionExit(err error) int {
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		return ee.ExitCode()
	case err != nil:
		return 2
	}
	return 0
}
