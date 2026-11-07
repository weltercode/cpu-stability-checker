package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/lockfile"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/runctl"
)

// G01c (menu plan section 9): the managed control channel. stdin
// carries one JSON command per line; cancel carries the session id
// and a command id, is idempotent, and confirms exactly once per
// command id. EOF on the control pipe in explicitly managed mode
// cancels the run - a GUI that vanished is indistinguishable from a
// GUI that closed the pipe - while a plain CLI run without stdin
// never starts this reader and is never considered managed.

// streamCommand is one control record on stdin.
type streamCommand struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	CommandID string `json:"command_id,omitempty"`
}

// startCommandReader watches stdin for control commands until the
// run ends (stop) or the pipe breaks. The sink is captured locally:
// the teardown may nil the package variable while this reader is
// still answering a command.
func startCommandReader() (stop func()) {
	s := currentEvents
	if s == nil {
		return func() {}
	}
	in := bufio.NewReader(os.Stdin)
	var runDone atomic.Bool
	var cancelRequested atomic.Bool
	var cancellingOnce sync.Once
	// stopMu makes the run-done decision atomic end to end: while stop()
	// closes the session, no in-flight cancel (a GUI command's or a pipe
	// EOF's) can slip between "not done yet" and RequestCancel and arm
	// the process-wide pending slot after the session is already dead.
	// This one goroutine serializes its own sections; stop() excludes
	// them from the outside.
	var stopMu sync.Mutex
	acked := map[string]bool{} // touched only by this one goroutine
	go func() {
		for {
			line, err := in.ReadString('\n')
			stopMu.Lock()
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				handleCommand(s, &runDone, &cancelRequested, &cancellingOnce, acked, trimmed)
			}
			if err != nil {
				// EOF (or a broken pipe) on the control channel in
				// managed mode is a cancel: the GUI is gone.
				if !runDone.Load() {
					cancellingOnce.Do(func() {
						s.emit("cancelling", "", cancellingPayload{Reason: "control pipe closed"}, false)
					})
					runctl.RequestCancel(plan.ExecCancelled, "gui control pipe closed (EOF)")
				}
				stopMu.Unlock()
				return
			}
			stopMu.Unlock()
		}
	}()
	return func() {
		stopMu.Lock()
		defer stopMu.Unlock()
		runDone.Store(true)
		// The session ends here, and so does everything its own control
		// channel meant: a cancel that raced this very stop (the run's
		// Guard had finished, runDone had not flipped yet) may have
		// armed the pending slot. Left armed, it kills the NEXT run in
		// this long-lived process - a menu host, a test binary (audit
		// A01's ghost cancel) - with a reason that GUI never sent.
		runctl.ClearPendingCancel()
	}
}

// handleCommand answers one control record. Every refusal is a
// structured command_error - never a silent ignore, never a guess.
func handleCommand(s *eventSink, runDone *atomic.Bool, cancelRequested *atomic.Bool, cancellingOnce *sync.Once, acked map[string]bool, line string) {
	var cmd streamCommand
	if err := json.Unmarshal([]byte(line), &cmd); err != nil {
		s.emit("command_error", "", commandErrorPayload{
			Message: "control line is not one JSON command: " + err.Error(),
		}, false)
		return
	}
	if cmd.Type == "" {
		s.emit("command_error", "", commandErrorPayload{CommandID: cmd.CommandID, Message: "control command has no type"}, false)
		return
	}
	if runDone.Load() {
		s.emit("command_error", "", commandErrorPayload{
			Type:      cmd.Type,
			CommandID: cmd.CommandID,
			Message:   "the run already finished: no command applies anymore",
		}, false)
		return
	}
	switch cmd.Type {
	case "cancel":
		s.mu.Lock()
		session := s.session
		s.mu.Unlock()
		if session != "" && cmd.SessionID != "" && cmd.SessionID != session {
			s.emit("command_error", "", commandErrorPayload{
				Type:      cmd.Type,
				CommandID: cmd.CommandID,
				Message:   "session_id does not match this run",
			}, false)
			return
		}
		if cmd.CommandID != "" {
			if acked[cmd.CommandID] {
				// The same command id was already confirmed: a
				// resend is ignored, one ack per command.
				return
			}
			acked[cmd.CommandID] = true
		}
		already := cancelRequested.Swap(true)
		if !already {
			cancellingOnce.Do(func() {
				s.emit("cancelling", "", cancellingPayload{Reason: "gui cancel command"}, false)
			})
			runctl.RequestCancel(plan.ExecCancelled, "gui cancel command")
		}
		s.emit("cancel_ack", "", cancelAckPayload{
			CommandID:         cmd.CommandID,
			AlreadyCancelling: already,
		}, false)
	default:
		s.emit("command_error", "", commandErrorPayload{
			Type:      cmd.Type,
			CommandID: cmd.CommandID,
			Message:   "unknown command type: this build answers 'cancel' only",
		}, false)
	}
}

// runLockPath marks the one live run session of this user's machine
// area (section 9: concurrent run sessions are forbidden in v1). It
// lives in the per-user temp area, NOT next to the artifacts: a lock
// in the working directory never guards a second run started from
// another directory (audit B08). The file is persistent and the OS
// lock inside it IS the ownership: a crashed holder's lock vanishes
// with its process, a live holder is never evicted, and the pid text
// inside is a diagnostic, never the basis of a decision. Tests
// relocate it.
var runLockPath = filepath.Join(os.TempDir(), "cpu-stability-run.lock")

// acquireRunLock takes the OS lock or refuses, naming the live
// holder's metadata for humans. The refusal arrives before any load:
// two GUIs cannot stack two full loads on one machine by accident,
// from any directory.
func acquireRunLock() (release func(), err error) {
	l, err := lockfile.Acquire(runLockPath, fmt.Sprintf("pid=%d\nstarted=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339)))
	if err != nil {
		var held *lockfile.HeldError
		if errors.As(err, &held) {
			return nil, fmt.Errorf("another run session is active (%v): this build forbids concurrent run sessions", held)
		}
		return nil, fmt.Errorf("cannot take the run lock: %w", err)
	}
	rel := func() {
		if rerr := l.Release(); rerr != nil {
			fmt.Fprintf(os.Stderr, "warning: run lock release failed (%v); the OS releases it with the process anyway\n", rerr)
		}
	}
	return rel, nil
}

// The run lock is operation-scoped and re-entrant within one
// process: the managed entry takes it early (before anything loads)
// and the load choke point (executeOptWithPanel) takes it for every
// other entry point. Nesting must not self-refuse, and the OS lock
// is released when the OUTERMOST holder is done - not when an inner
// one is. The refcount below is the operation scope: it is only ever
// nonzero for the duration of a run operation, never a global
// bypass.
var (
	runLockMu  sync.Mutex
	runLockRef int
	runLockRel func()
)

// acquireRunLockShared is the re-entrant form: the first caller in
// the process takes the OS lock, nested callers join the same
// operation for free. A refusal is real contention with another
// process, never with itself.
func acquireRunLockShared() (func(), error) {
	runLockMu.Lock()
	defer runLockMu.Unlock()
	if runLockRef > 0 {
		runLockRef++
		return releaseRunLockShared, nil
	}
	rel, err := acquireRunLock()
	if err != nil {
		return nil, err
	}
	runLockRef, runLockRel = 1, rel
	return releaseRunLockShared, nil
}

func releaseRunLockShared() {
	runLockMu.Lock()
	defer runLockMu.Unlock()
	if runLockRef <= 0 {
		return // double release: the OS lock is already gone; say nothing twice
	}
	runLockRef--
	if runLockRef == 0 {
		rel := runLockRel
		runLockRel = nil
		if rel != nil {
			rel()
		}
	}
}
