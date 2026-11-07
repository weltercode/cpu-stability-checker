package subprocess

// P08 parent-side session driver: run ONE prepared worker through a
// PREPARED schedule. The timeline is journaled before the run (the
// concrete windows are the reproduction artifact), every commanded
// burst gets a parent session_burst record (when it was actually
// sent vs when the acks arrived, signed schedule deviation), and
// the worker writes its own burst_done view. The two views of the
// same burst are separate records: the plan demands the commanded
// transition and the observation stay distinguishable, and the
// parent's intervals are labeled upper bounds because they include
// the IPC round trip (no verified shared monotonic source on this
// API yet - the plan explicitly allows parent round-trip intervals
// counted separately).
//
// Waiting is timer-based: the driver sleeps until the planned
// moment, it never busy-spins, and a late wake is recorded as a
// deviation instead of being smoothed away or caught up.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
)

// SessionOutcome summarizes one driven session.
type SessionOutcome struct {
	BurstsDone    int    // bursts whose VERIFIED ack arrived
	BurstsTried   int    // windows actually triggered
	BurstsAborted int    // triggered but never verified (cancel/fault/timeout)
	Fault         string // kernel fault seen mid-session ("" = none)
	SetupErr      string // setup/resource failure ("" = none)
	Interrupted   bool   // ctx cancelled before the schedule finished
	Exit          int    // worker exit code from the reap
	// Checks sums the confirmed verified checks the worker reported on
	// VERIFIED acks. Aborted bursts contribute nothing: an unverified
	// block is not a verified check.
	Checks uint64
	// ChecksKnown is false when some verified block closed without a
	// reported count: the sum is then partial truth, not a total.
	ChecksKnown bool
	// Parking is the folded Q04 parking evidence for this slot
	// (measured idle windows x OS parking samples). nil = no
	// observation ran: the policy was off, or the session never
	// reached ready-idle - the question was never asked, never
	// "answered not parked".
	Parking *transition.ParkingEvidence
}

// toJournalSchedule converts the prepared timeline into its journaled
// form: integer nanoseconds (float ms would invent rounding drift)
// and the seed only as provenance - the windows are the artifact.
func toJournalSchedule(s transition.Schedule) journal.SchedulePlan {
	p := journal.SchedulePlan{
		BaseNS:   int64(s.Base),
		JitterNS: int64(s.Jitter),
		Seed:     s.Seed,
		Windows:  make([]journal.ScheduleWindow, 0, len(s.Windows)),
	}
	for _, w := range s.Windows {
		p.Windows = append(p.Windows, journal.ScheduleWindow{GapNS: int64(w.Gap), RunForNS: int64(w.RunFor)})
	}
	return p
}

// workerStory returns the worker's own stderr: when a session ends
// badly without an ERROR ack, the worker's last words belong in the
// parent's error, not in a lost pipe buffer. Long output is kept at
// BOTH ends: the first line usually names the failure (a flag error,
// an allocation refusal) and the tail carries the last words, so
// cutting only the tail would hide the reason.
func workerStory(h *SessionHandle) string {
	s := strings.TrimSpace(h.Stderr.String())
	const max = 400
	if len(s) > max {
		half := max / 2
		s = s[:half] + " [...snipped] " + s[len(s)-half:]
	}
	if s == "" {
		return ""
	}
	return " (worker: " + s + ")"
}

// RunSessionBursts drives one worker session through sched, writing
// the journal records to jw (the parent journal). It always reaps
// the worker: no path leaves a live process behind. park (nil = no
// observation, today's run bit-for-bit) is the Q04 parking observer:
// started when the worker reports ready-idle, fed the measured idle
// windows as they close, and folded into a parking_obs record on every
// path that reached Start - an interrupted session still says what
// was observed up to the interruption.
func RunSessionBursts(ctx context.Context, argv []string, sessionID string, core int, sched transition.Schedule, timeouts transition.Timeouts, jw *journal.Writer, park *ParkingObserver) SessionOutcome {
	out := SessionOutcome{Exit: 2, ChecksKnown: true}
	// The prepared schedule is journaled BEFORE anything spawns:
	// even if the worker never starts, the reader knows exactly what
	// was commanded.
	jw.SchedulePlan(toJournalSchedule(sched))

	h, err := StartSession(argv, sessionID, timeouts)
	if err != nil {
		out.SetupErr = err.Error()
		return out
	}
	reaped := false
	reap := func() {
		if !reaped {
			reaped = true
			out.Exit = h.Reap(15 * time.Second)
		}
	}
	defer reap()

	// PREPARE: allocate + first-touch exactly once, then idle.
	cmd, err := h.Command(transition.CmdPrepare)
	if err != nil {
		out.SetupErr = err.Error()
		return out
	}
	if _, err := h.Await(cmd.Seq, transition.AckReady, timeouts.ForKind(transition.CmdPrepare)); err != nil {
		out.SetupErr = err.Error()
		reap()
		out.SetupErr += workerStory(h)
		return out
	}
	if err := h.AwaitIdle(timeouts.ForKind(transition.CmdIdle)); err != nil {
		out.SetupErr = err.Error()
		reap()
		out.SetupErr += workerStory(h)
		return out
	}

	// Q04: the worker is prepared AND reported idle - from here the CPU
	// is genuinely un-loaded, so this is the honest moment to begin
	// sampling the OS parking view (nil observer = never started, run
	// stays bit-for-bit what it was).
	if park != nil {
		park.Start()
	}

	runner := transition.NewScheduleRunner(sched, time.Now(), nil)
	idleFrom := time.Now() // when the previous burst closed (or session ready)
	for runner.Remaining() > 0 {
		if ctx.Err() != nil {
			out.Interrupted = true
			break
		}
		due := runner.Due()
		if wait := time.Until(due); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				out.Interrupted = true
				break
			}
			if out.Interrupted {
				break
			}
		}

		// The runner opens the window BEFORE the send: the schedule
		// deviation is measured against the planned moment, and the
		// window's RunFor drives the wait below.
		w, dev := runner.Begin(time.Now())
		trig, err := h.Command(transition.CmdTrigger)
		if err != nil {
			// The machine refused (state not idle): honest stop.
			out.SetupErr = err.Error()
			break
		}
		// Q04: this trigger CLOSES a measured idle span [idleFrom,
		// CommandedAt] - the very pair IdleBeforeMS below is computed
		// from, so the evidence and the commanded story describe the
		// same spans on the same parent clock. A span that ends in a
		// refused command has no real far bound and records no window:
		// half-known is not measured.
		if park != nil {
			park.Window(idleFrom, trig.CommandedAt)
		}
		out.BurstsTried++
		obs := journal.BurstObservation{
			Burst:        trig.Burst,
			Seq:          trig.Seq,
			SchedDevMS:   ms(dev), // signed: + late, - early
			IdleBeforeMS: ms(trig.CommandedAt.Sub(idleFrom)),
		}
		startedAt := trig.CommandedAt

		_, err = h.Await(trig.Seq, transition.AckStarted, timeouts.ForKind(transition.CmdTrigger))
		if err != nil {
			obs.TriggerToStartMS = ms(time.Since(startedAt))
			jw.SessionBurst(core, obs) // Complete=false: the story says what never came
			out.BurstsAborted++
			if errors.As(err, new(*transition.WorkerError)) {
				out.Fault = err.Error()
			} else {
				out.SetupErr = err.Error()
			}
			break
		}
		obs.TriggerToStartMS = ms(time.Since(startedAt))

		// Run the window while LISTENING: a fault with no VERIFY
		// pending surfaces now (ERROR ack), a user cancel aborts
		// the burst at its honest boundary via IDLE.
		elapsed, werr := h.WaitWindow(ctx, w.RunFor)
		if werr != nil {
			obs.TriggerToVerifiedMS = ms(time.Since(startedAt))
			jw.SessionBurst(core, obs)
			out.BurstsAborted++
			if errors.As(werr, new(*transition.WorkerError)) {
				out.Fault = werr.Error()
			} else {
				out.SetupErr = werr.Error()
			}
			break
		}
		if !elapsed {
			// ctx cancelled mid-burst: ask the worker to stop at
			// its honest boundary; the IDLE_ACK answers the IDLE
			// command's own seq.
			idleCmd, ierr := h.Command(transition.CmdIdle)
			if ierr == nil {
				_, _ = h.Await(idleCmd.Seq, transition.AckIdle, timeouts.ForKind(transition.CmdIdle))
			}
			obs.TriggerToVerifiedMS = ms(time.Since(startedAt))
			jw.SessionBurst(core, obs)
			out.BurstsAborted++
			out.Interrupted = true
			break
		}

		ver, err := h.Command(transition.CmdVerify)
		if err != nil {
			out.SetupErr = err.Error()
			jw.SessionBurst(core, obs)
			out.BurstsAborted++
			break
		}
		ack, err := h.Await(ver.Seq, transition.AckVerified, timeouts.ForKind(transition.CmdVerify))
		obs.TriggerToVerifiedMS = ms(time.Since(startedAt))
		if err != nil {
			jw.SessionBurst(core, obs)
			out.BurstsAborted++
			if errors.As(err, new(*transition.WorkerError)) {
				out.Fault = err.Error()
			} else {
				out.SetupErr = err.Error()
			}
			break
		}
		obs.Complete = true
		obs.Checks, obs.Bytes = ack.Checks, ack.Bytes
		if ack.Checks != nil {
			out.Checks += *ack.Checks
		} else {
			// A verified block the worker closed without reporting a
			// count: the total is no longer fully known - never guess 0.
			out.ChecksKnown = false
		}
		out.BurstsDone++
		jw.SessionBurst(core, obs)
		if err := h.AwaitIdle(timeouts.ForKind(transition.CmdIdle)); err != nil {
			out.SetupErr = err.Error()
			break
		}
		runner.End(time.Now()) // re-anchors the next window from the ACTUAL close
		idleFrom = time.Now()
	}

	// Q04: close the observation and fold it BEFORE the story is
	// finished elsewhere: whatever windows and samples this session
	// really produced - including a cancelled or faulted one - become
	// their own parking_obs record, never merged into the commanded
	// schedule or the acks. An interrupted run still tells what was
	// observed up to the interruption; a session that never started
	// polling carries no record at all (absence = the question was
	// never asked).
	if park != nil {
		ev := park.Close()
		out.Parking = ev
		jw.ParkingObs(core, toJournalParking(ev))
	}

	// Clean end: ask the worker to stop (it releases its memory on
	// the way out). Interrupted or faulted sessions still ask -
	// STOP is the polite path; Reap is the guarantee.
	if out.Fault == "" && out.SetupErr == "" {
		if err := h.Stop(timeouts.ForKind(transition.CmdStop)); err != nil {
			out.SetupErr = err.Error()
		}
	}
	reap()
	// A setup failure that the worker never explained with an ERROR
	// ack gets the worker's own last words attached - after the
	// reap, so the stderr buffer is no longer being written to.
	if out.SetupErr != "" && !strings.Contains(out.SetupErr, " (worker: ") {
		out.SetupErr += workerStory(h)
	}
	return out
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
