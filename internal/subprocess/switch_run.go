package subprocess

// Q05 parent-side pair-switch driver: spawn N prepared workers (two
// groups of aggressor/observer pairs), bring every one through
// PREPARE + idle BEFORE the first switch, then walk the prepared
// turn sequence with the TurnLedger as the single arbiter. The
// measured transition contains no spawn and no allocation: only the
// TRIGGER commands and the acks. No-overlap is structural - the
// ledger only ever grants a turn while every other group has closed
// its barrier (VERIFIED + IDLE_ACK), so drifting timers cannot
// invent interference that was never commanded. The per-participant
// parent view rides the members' own session_burst records; the
// switch itself is the group-level session_switch record.
//
// Exit contract held like every session path: the ENTRY folds fault
// 1 > setup 2 > interrupted 130 > clean 0; the driver reports the
// facts, never the verdict. Cancellation and death release every
// process: STOP is the polite path, Reap the guarantee.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
)

// SwitchParticipant is one prepared worker inside one switch group.
// Slot order inside the group carries the role: group index x slot
// order mirrors abab (the aggressor workload is named first in the
// phase, the observer second) - attribution stays with the plan,
// this only says which logical CPU runs which session.
type SwitchParticipant struct {
	Core  int
	Group int
}

// SwitchOutcome summarizes one driven pair-switch session.
type SwitchOutcome struct {
	TurnsTried  int    // turns actually TRIGGERED
	TurnsDone   int    // turns whose idle barrier (all VERIFIED + all IDLE_ACK) closed
	Fault       string // worker ERROR seen mid-turn ("" = none)
	FaultCore   int    // the member that faulted (-1 = unknown/none)
	SetupErr    string // setup/resource failure or a bookkeeping inconsistency ("" = none)
	Interrupted bool   // ctx cancelled before the turn sequence finished
	Exit        int    // worst worker exit code from the reaps (2 while something never started)
	// Checks sums the confirmed verified checks reported on VERIFIED
	// acks. An aborted turn contributes nothing: unverified is not
	// verified.
	Checks      uint64
	ChecksKnown bool
	// ParkingPerCore carries each participant's folded parking
	// evidence (measured idle windows x OS parking samples, Q04
	// semantics applied to switch gaps): nil map entry = that
	// question was never asked for that core.
	ParkingPerCore map[int]*transition.ParkingEvidence
}

// toJournalSwitchPlan converts the prepared turn timeline into its
// journaled form (integer nanoseconds, seed only as provenance).
func toJournalSwitchPlan(s transition.SwitchSchedule) journal.SwitchPlan {
	p := journal.SwitchPlan{BaseNS: int64(s.Base), JitterNS: int64(s.Jitter), Seed: s.Seed, Order: s.Order}
	for _, t := range s.Turns {
		p.Turns = append(p.Turns, journal.SwitchPlanTurn{Group: t.Group, Round: t.Round, GapNS: int64(t.Gap), RunForNS: int64(t.RunFor)})
	}
	return p
}

// switchSpan is the full prepared timeline: the sum of every turn's
// gap plus run. Journaled at run_start so a reader knows what the
// run was supposed to last.
func switchSpan(sched transition.SwitchSchedule) time.Duration {
	var span time.Duration
	for _, t := range sched.Turns {
		span += t.Gap + t.RunFor
	}
	return span
}

// RunSwitchSession drives the whole pair-switch. argvs[i]/sessionIDs[i]
// belong to parts[i]; parks[i] (nil = no observation for that
// participant, exactly the Q04 contract) samples inside that
// participant's measured idle windows. The prepared turn timeline is
// journaled BEFORE anything spawns: even if no worker ever starts,
// the reader knows exactly what was commanded.
func RunSwitchSession(ctx context.Context, argvs [][]string, sessionIDs []string, parts []SwitchParticipant,
	sched transition.SwitchSchedule, timeouts transition.Timeouts, jw *journal.Writer, parks []*ParkingObserver) SwitchOutcome {

	out := SwitchOutcome{Exit: 2, ChecksKnown: true, FaultCore: -1}
	jw.SwitchPlan(toJournalSwitchPlan(sched))

	handles := make([]*SessionHandle, len(parts))
	reaped := make([]bool, len(handles))
	reapAll := func() {
		worst := 0
		for i, h := range handles {
			if h == nil {
				worst = 2 // something never started: setup path, not a clean reap
				continue
			}
			if reaped[i] {
				continue
			}
			reaped[i] = true
			if ex := h.Reap(15 * time.Second); ex != 0 && worst == 0 {
				worst = ex
			}
		}
		out.Exit = worst
	}
	defer reapAll()

	// Spawn and PREPARE every participant before the first switch -
	// the commands go out first, the wait comes after, so the
	// preparation overlaps and, more importantly, sits ENTIRELY
	// outside any measured turn. A participant that cannot prepare is
	// a setup error named with its core; every already-spawned
	// sibling is reaped by the deferred guarantee.
	for i := range handles {
		h, err := StartSession(argvs[i], sessionIDs[i], timeouts)
		if err != nil {
			out.SetupErr = fmt.Sprintf("switch: spawn participant %d (core %d): %v", i, parts[i].Core, err)
			return out
		}
		handles[i] = h
	}
	pcmds := make([]transition.Command, len(handles))
	for i, h := range handles {
		c, err := h.Command(transition.CmdPrepare)
		if err != nil {
			out.SetupErr = fmt.Sprintf("switch: PREPARE participant core %d: %v", parts[i].Core, err)
			return out
		}
		pcmds[i] = c
	}
	for i, h := range handles {
		if _, err := h.Await(pcmds[i].Seq, transition.AckReady, timeouts.ForKind(transition.CmdPrepare)); err != nil {
			out.SetupErr = fmt.Sprintf("switch: prepare participant core %d: %v", parts[i].Core, err) + workerStory(h)
			return out
		}
		if err := h.AwaitIdle(timeouts.ForKind(transition.CmdIdle)); err != nil {
			out.SetupErr = fmt.Sprintf("switch: participant core %d never reported idle after prepare: %v", parts[i].Core, err) + workerStory(h)
			return out
		}
	}

	// Every participant is prepared AND idle: from here the CPUs are
	// genuinely un-loaded, so this is the honest moment to start the
	// parking observers (nil = never started, the run stays exactly
	// what it was without the policy).
	for _, p := range parks {
		if p != nil {
			p.Start()
		}
	}

	membersOf := make(map[int][]int, sched.GroupCount)
	for i, p := range parts {
		if p.Group < 0 || p.Group >= sched.GroupCount {
			out.SetupErr = fmt.Sprintf("switch: participant core %d names group %d outside the schedule's %d groups", p.Core, p.Group, sched.GroupCount)
			return out
		}
		membersOf[p.Group] = append(membersOf[p.Group], i)
	}
	for g := 0; g < sched.GroupCount; g++ {
		if len(membersOf[g]) == 0 {
			out.SetupErr = fmt.Sprintf("switch: group %d has no participants: every prepared group must have a worker, or its turns would command nothing", g)
			return out
		}
	}

	ledger := transition.NewTurnLedger(sched)
	runner := transition.NewSwitchRunner(sched, time.Now(), nil)
	idleFrom := make([]time.Time, len(handles))
	for i := range idleFrom {
		idleFrom[i] = time.Now()
	}

	turnIdx := 0
	for ledger.RemainingTurns() > 0 {
		if ctx.Err() != nil {
			out.Interrupted = true
			break
		}
		if wait := time.Until(runner.Due()); wait > 0 {
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
		tt, dev := runner.Begin(time.Now())
		obs := journal.SwitchObservation{Turn: turnIdx, Group: tt.Group, Round: tt.Round, SchedDevMS: ms(dev)}
		for _, mi := range membersOf[tt.Group] {
			obs.Cores = append(obs.Cores, parts[mi].Core)
		}
		res, err := ledger.ReserveTurn()
		if err != nil {
			// Pure bookkeeping cannot refuse without a taint or going
			// past the end - both are wiring bugs. Named, journaled,
			// and the run stops honestly.
			obs.Error = err.Error()
			jw.SessionSwitch(obs)
			out.SetupErr = err.Error()
			break
		}
		_ = res // the runner and the ledger walk the same prepared sequence
		turnIdx++
		out.TurnsTried++

		members := membersOf[tt.Group]
		trigs := make([]transition.Command, len(members))
		sendFailed := ""
		for k, mi := range members {
			c, cerr := handles[mi].Command(transition.CmdTrigger)
			if cerr != nil {
				// The machine refused (state not idle): honest stop,
				// never a retry and never a "almost worked".
				ledger.CancelTurn()
				sendFailed = fmt.Sprintf("group %d member core %d refused TRIGGER: %v", tt.Group, parts[mi].Core, cerr)
				break
			}
			trigs[k] = c
			// The trigger CLOSES this member's measured idle span
			// [idleFrom, CommandedAt] - the same span IdleBeforeMS
			// below is computed from, so parking evidence and the
			// commanded story describe the same windows on the same
			// parent clock (Q04 semantics on switch gaps).
			if parks != nil && mi < len(parks) && parks[mi] != nil {
				parks[mi].Window(idleFrom[mi], c.CommandedAt)
			}
		}
		if sendFailed != "" {
			obs.Error = sendFailed
			jw.SessionSwitch(obs)
			// Members already TRIGGERED must stop at their honest
			// boundary before anything else ends: best-effort IDLE.
			idleAll(handles, members, timeouts)
			out.SetupErr = sendFailed
			break
		}

		primary := trigs[0].Seq
		obs.PrimarySeq = primary
		if err := ledger.IssueTurn(primary); err != nil {
			obs.Error = err.Error()
			jw.SessionSwitch(obs)
			idleAll(handles, members, timeouts)
			out.SetupErr = err.Error()
			break
		}
		turnStart := trigs[0].CommandedAt

		// Command the whole group's START wait before running any
		// window: no member waits on another member's ack to load,
		// and the switch latency is measured to the LAST STARTED of
		// the group (the group is only "switched in" when all of it
		// runs).
		bursts := make([]journal.BurstObservation, len(members))
		for k, mi := range members {
			bursts[k] = journal.BurstObservation{
				Burst:        trigs[k].Burst,
				Seq:          trigs[k].Seq,
				SchedDevMS:   ms(dev),
				IdleBeforeMS: ms(trigs[k].CommandedAt.Sub(idleFrom[mi])),
			}
		}
		abort := ""
		faulted := false
		faultMember := -1
		lastStarted := turnStart
		for k, mi := range members {
			_, aerr := handles[mi].Await(trigs[k].Seq, transition.AckStarted, timeouts.ForKind(transition.CmdTrigger))
			if aerr != nil {
				bursts[k].TriggerToStartMS = ms(time.Since(trigs[k].CommandedAt))
				abort = fmt.Sprintf("group %d member core %d never STARTED: %v", tt.Group, parts[mi].Core, aerr)
				faulted = errors.As(aerr, new(*transition.WorkerError))
				faultMember = mi
				break
			}
			bursts[k].TriggerToStartMS = ms(time.Since(trigs[k].CommandedAt))
			if at := time.Now(); at.After(lastStarted) {
				lastStarted = at
			}
		}
		obs.SwitchToStartMS = ms(lastStarted.Sub(turnStart))

		// Run the group's windows CONCURRENTLY: each handle is read
		// by exactly one owner at a time (the single-reader rule per
		// SessionHandle), and every result is drained before the
		// driver touches any handle again.
		if abort == "" {
			type wres struct {
				k       int
				elapsed bool
				err     error
			}
			ch := make(chan wres, len(members))
			for k, mi := range members {
				go func(k, mi int) {
					el, werr := handles[mi].WaitWindow(ctx, tt.RunFor)
					ch <- wres{k: k, elapsed: el, err: werr}
				}(k, mi)
			}
			elapsed := make([]bool, len(members))
			for n := 0; n < len(members); n++ {
				r := <-ch
				if r.err != nil && abort == "" {
					mi := members[r.k]
					abort = fmt.Sprintf("group %d member core %d faulted mid-window: %v", tt.Group, parts[mi].Core, r.err)
					faulted = errors.As(r.err, new(*transition.WorkerError))
					faultMember = mi
				}
				elapsed[r.k] = r.elapsed
			}
			if abort == "" {
				for k := range elapsed {
					if !elapsed[k] {
						// ctx cancelled mid-window: ask every member to
						// stop at its honest boundary; the IDLE answer
						// belongs to the IDLE command's own seq.
						idleAll(handles, members, timeouts)
						abort = "cancelled mid-turn"
						out.Interrupted = true
						break
					}
				}
			}
		}

		// VERIFY unless the turn already failed: a verified block
		// closed honestly is evidence; an aborted turn says so.
		if abort == "" {
			vers := make([]transition.Command, len(members))
			for k, mi := range members {
				c, cerr := handles[mi].Command(transition.CmdVerify)
				if cerr != nil {
					abort = fmt.Sprintf("group %d member core %d: VERIFY refused: %v", tt.Group, parts[mi].Core, cerr)
					faulted = errors.As(cerr, new(*transition.WorkerError))
					faultMember = mi
					break
				}
				vers[k] = c
			}
			if abort == "" {
				for k, mi := range members {
					ack, aerr := handles[mi].Await(vers[k].Seq, transition.AckVerified, timeouts.ForKind(transition.CmdVerify))
					bursts[k].TriggerToVerifiedMS = ms(time.Since(trigs[k].CommandedAt))
					if aerr != nil {
						abort = fmt.Sprintf("group %d member core %d never VERIFIED: %v", tt.Group, parts[mi].Core, aerr)
						faulted = errors.As(aerr, new(*transition.WorkerError))
						faultMember = mi
						break
					}
					bursts[k].Complete = true
					bursts[k].Checks, bursts[k].Bytes = ack.Checks, ack.Bytes
					if ack.Checks != nil {
						out.Checks += *ack.Checks
					} else {
						// A verified block closed without a reported
						// count: the total is no longer fully known -
						// never guess 0.
						out.ChecksKnown = false
					}
				}
			}
		}

		// The idle barrier: EVERY member back in blocking idle. A
		// VERIFIED alone is not the barrier (the load still has to
		// stop) and an IDLE_ACK alone is not evidence (the block may
		// never have been verified) - the ledger only closes on this
		// pair, and the next group's turn can only be granted after
		// that close.
		if abort == "" {
			for _, mi := range members {
				if err := handles[mi].AwaitIdle(timeouts.ForKind(transition.CmdIdle)); err != nil {
					abort = fmt.Sprintf("group %d member core %d never returned idle: %v", tt.Group, parts[mi].Core, err)
					faulted = errors.As(err, new(*transition.WorkerError))
					faultMember = mi
					break
				}
			}
		}

		// Member views ride their own session_burst records no matter
		// how the turn ended: the story says what never came rather
		// than staying silent about it.
		for k, mi := range members {
			jw.SessionBurst(parts[mi].Core, bursts[k])
		}
		obs.TurnCloseMS = ms(time.Since(turnStart))

		if abort != "" {
			obs.Error = abort
			if terr := ledger.TaintTurn(primary, abort); terr != nil {
				out.SetupErr = terr.Error()
			}
			// Members of the aborted turn that ARE still loaded stop
			// at their honest boundary now (best-effort: a dead member
			// simply does not answer, and Reap is the guarantee that
			// nothing survives either way). Without this a healthy
			// sibling of a faulted member would ride full-speed until
			// the reap timeout kills it.
			idleAll(handles, members, timeouts)
			jw.SessionSwitch(obs)
			if faulted {
				out.Fault = abort
				if faultMember >= 0 {
					out.FaultCore = parts[faultMember].Core
				}
			} else if !out.Interrupted {
				// No ERROR ack and nobody asked to stop, yet the
				// turn died (member vanished, pipe broke, machine
				// refused): P08's exact vocabulary - a fault is only
				// what a worker REPORTED; an unexplained integrity
				// break is a setup class, never a clean early end.
				out.SetupErr = abort
			}
			break
		}
		if err := ledger.CompleteTurn(primary); err != nil {
			// Wiring inconsistency (the turn was issued under this
			// seq and never completed elsewhere): named, never patched.
			obs.Error = err.Error()
			jw.SessionSwitch(obs)
			out.SetupErr = err.Error()
			break
		}
		out.TurnsDone++
		obs.Complete = true
		jw.SessionSwitch(obs)
		closeNow := time.Now()
		runner.End(closeNow)
		for _, mi := range members {
			idleFrom[mi] = closeNow
		}
	}

	// Q04 semantics per participant: close and fold the observation
	// BEFORE the story finishes elsewhere - whatever windows and
	// samples this core really produced (including on a cancelled or
	// faulted run) become its own parking_obs record, never merged
	// into the commanded turns or the acks. A session that never
	// started polling carries no record at all.
	if len(parks) > 0 && len(parks) <= len(parts) {
		out.ParkingPerCore = map[int]*transition.ParkingEvidence{}
		for i, p := range parks {
			if p == nil {
				continue
			}
			ev := p.Close()
			out.ParkingPerCore[parts[i].Core] = ev
			jw.ParkingObs(parts[i].Core, toJournalParking(ev))
		}
	}

	// Ask every worker to STOP (they release memory on the way out),
	// interrupted or faulted runs included: a healthy sibling of an
	// aborted turn or of a faulted member is still listening, and
	// without its polite STOP it would answer Reap's stdin close with
	// an "ended incomplete" exit 2 - collateral that masks the real
	// headline (the fault's exit 1, the cancel's honest boundary).
	// Refused STOPs are recorded as setup trouble only on an
	// otherwise-clean run; when a fault or interruption is already
	// named, Reap is the guarantee and the refusal adds no fact.
	if out.SetupErr == "" {
		for _, h := range handles {
			if err := h.Stop(timeouts.ForKind(transition.CmdStop)); err != nil {
				if out.Fault == "" && !out.Interrupted {
					out.SetupErr = fmt.Sprintf("switch: STOP: %v%s", err, workerStory(h))
					break
				}
			}
		}
	}
	reapAll()
	if out.Fault == "" && out.SetupErr == "" && !out.Interrupted && out.TurnsDone == len(sched.Turns) {
		out.Exit = 0
	}
	return out
}

// idleAll asks each listed member to stop loading at its honest
// boundary and answers the IDLE command's own seq. Best-effort by
// design: a member that cannot take the command (already dead,
// refused state) is exactly what Reap then guarantees.
func idleAll(handles []*SessionHandle, members []int, timeouts transition.Timeouts) {
	for _, mi := range members {
		ic, err := handles[mi].Command(transition.CmdIdle)
		if err != nil {
			continue
		}
		_, _ = handles[mi].Await(ic.Seq, transition.AckIdle, timeouts.ForKind(transition.CmdIdle))
	}
}
