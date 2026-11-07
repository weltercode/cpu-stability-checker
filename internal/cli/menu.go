package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/tui"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// runMenu is the default human-facing entry point. Configuration remains the
// source of defaults, while the interactive session can override the settings
// for one run without rewriting config.json.
func runMenu() int {
	cfg := config.Load(config.DefaultPath)
	// External workloads (Prime95, y-cruncher, ...) live in config.json;
	// register them up front so the menu's workload picker and 'list'
	// show them just like the scripted commands do.
	workload.RegisterExternal(cfg.External)
	return menuLoop(bufio.NewReader(os.Stdin), cfg)
}

// menuLoop is the task menu of menu plan section 1: repeat first, the
// three goal categories, results, settings, exit. The crash-loop
// workflow is unchanged: a stopped or faulted run (exit 1) returns
// toward "Repeat last test" one keystroke away, setup errors (2) and
// force quits (130) end the session, and the worst run code seen
// stays the menu's own exit code. On a VT terminal every step is a
// full screen that replaces the last one; on any other output the
// menu appends exactly as before.
func menuLoop(in *bufio.Reader, cfg config.Config) int {
	ui := newScreenUI(os.Stdout)
	defer ui.finish()
	note := ""
	worst := 0
	for {
		lines := menuLines(ui)
		if note != "" {
			lines = append(lines, "", note)
			note = ""
		}
		ui.screenPrompt(lines, "Action [1]: ")

		line, readErr := in.ReadString('\n')
		choice := strings.ToLower(strings.TrimSpace(line))
		if readErr != nil && choice == "" {
			// stdin is exhausted (piped input ended, Ctrl+Z, closed
			// terminal): the default action would restart forever on an
			// empty line, so an exhausted input means quit.
			fmt.Println()
			return worst
		}
		if choice == "" {
			// Enter is the shortcut for item 1 only when a recipe
			// exists (menu plan section 1); without one it starts
			// nothing - the screen already says "No previous test
			// yet".
			if !hasSavedRecipe() {
				continue
			}
			choice = "1"
		}

		switch choice {
		case "1":
			before := lastAttemptID()
			code := repeatLastRunWithUI(ui)
			if stop, exit := menuRunOutcome(in, ui, &worst, code, before); stop {
				return menuStopExit(in, ui, exit, before)
			}
			note = ui.paint(tui.FGBrightRed, runStopNote(code))
			ui.waitForEnter(in)
		case "2", "3", "4":
			topology, err := sysinfo.DiscoverTopology()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Cannot discover CPU topology: %v\n", err)
				return 2
			}
			cat, title := catalog.CPU, "CPU check"
			if choice == "3" {
				cat, title = catalog.RAM, "RAM check"
			}
			if choice == "4" {
				cat, title = catalog.MIX, "Mixed CPU + RAM tests"
			}
			if code := categoryMenu(in, ui, cfg, topology, cat, title, &worst); code != 0 {
				return code
			}
		case "5":
			// U02: real session history - the last sessions read from
			// their manifests, each reportable and replayable on its
			// own. The old honest limitation note ("history is not
			// kept yet") is gone because history is kept now.
			if code := historyMenu(in, ui, &worst); code != 0 {
				return code
			}
		case "6":
			if code := settingsMenu(in, ui, cfg, &worst); code != 0 {
				return code
			}
		case "0", "q", "quit":
			return worst
		default:
			bad := "Enter a number from 1 to 6, or 0 to quit."
			if ui.hasScreen() {
				note = ui.paint(tui.FGBrightRed, bad)
			} else {
				ui.note(ui.paint(tui.FGBrightRed, bad))
			}
		}
	}
}

// menuAfterRun applies the menu's run-result policy: a clean run (0)
// and a stopped or faulted run (1) keep the session alive - the
// crash-loop workflow is one keystroke from "Repeat last run" - while
// setup errors (2) and force quits (130) end it with that code. A
// stopped run is remembered in *worst so the menu's own exit code
// still honors the exit-code contract.
//
// beforeAttempt is the receipt attempt id captured before the action
// ran. A nonzero code whose receipt is PROVABLY still the pre-run one
// (readable, non-empty, unchanged) means the action never armed an
// attempt: it was refused or cancelled before any load. That is a
// dialog event, not a run outcome - the exit-code contract belongs to
// runs, and closing the program after a refusal that says "pick
// another profile" (or a cancelled form) contradicts the refusal
// itself. Such codes return stop=false with the code in exitCode for
// menuRunOutcome to state and stay; codes from real runs keep ending
// the session unchanged.
func menuAfterRun(worst *int, code int, beforeAttempt string) (stop bool, exitCode int) {
	switch code {
	case 0:
		return false, 0
	case 1:
		*worst = 1
		return false, 0
	default:
		if r, err := loadLastResult(); err == nil && r.AttemptID != "" && r.AttemptID == beforeAttempt {
			return false, code
		}
		return true, code
	}
}

// menuRunOutcome is the call-site wrapper: it applies menuAfterRun
// and, for a refusal that never armed an attempt, states the stay
// note in plain words, holds the screen for reading (same courtesy
// the stop exit established - closing without a pause read as "the
// program fell over") and returns control to the SAME menu the user
// was in, so "pick another profile" is actually possible.
func menuRunOutcome(in *bufio.Reader, ui *screenUI, worst *int, code int, beforeAttempt string) (stop bool, exitCode int) {
	stop, exit := menuAfterRun(worst, code, beforeAttempt)
	if stop {
		return true, exit
	}
	if exit != 0 {
		ui.stopMonitor() // nil-safe: release the screen from the live panel
		fmt.Fprintf(os.Stderr, "Not started (exit %d): the refusal line above is the reason - no attempt was armed and nothing was loaded. Back to the menu.\n", exit)
		waitForEnter(in)
	}
	if code == 0 {
		// A clean ending gets the same courtesy a stop and a refusal
		// already demand: the next menu frame buries the panel's
		// RESULT lines instantly, and a finished run that returns
		// without a closing word reads exactly like the crash it was
		// not (the owner saw a preset jump straight back to the menu
		// and asked whether it had failed). The note states only what
		// the attempt receipt actually recorded; a code 0 from a
		// cancelled dialog armed no attempt and stays silent.
		if n, color := runFinishNote(beforeAttempt); n != "" {
			ui.stopMonitor() // nil-safe
			fmt.Println(ui.paint(color, n))
			waitForEnter(in)
		}
	}
	return false, 0
}

// runStopNote is the menu note after a stopped run; empty for clean
// runs. It states what the attempt receipt actually recorded: a
// cancellation is named a cancellation with its reason, a setup
// failure a setup failure with its finding count, a fault a fault.
// The old "fault or user stop" coin flip forced the user to guess
// whether they stopped the tool or the machine reported something -
// the ending is an observation, never a guess.
func runStopNote(code int) string {
	if code == 0 {
		return ""
	}
	r, err := loadLastResult()
	if err != nil {
		return fmt.Sprintf("Run stopped (exit %d): INCOMPLETE - the attempt receipt is unreadable; the report and journal carry the story.", code)
	}
	if r.State == receiptPending {
		return fmt.Sprintf("Run stopped (exit %d): INCOMPLETE - the attempt never reported an ending (crash mid-run?); the report and journal carry the story.", code)
	}
	v := verdictOfReceipt(r)
	detail := v.Reason
	if r.AbortMsg != "" && !strings.Contains(detail, r.AbortMsg) {
		if detail == "" {
			detail = r.AbortMsg
		} else {
			detail += fmt.Sprintf(" (%s)", r.AbortMsg)
		}
	}
	fnd := ""
	if r.Findings > 0 {
		fnd = fmt.Sprintf(", %d finding(s) recorded", r.Findings)
	}
	switch v.Kind {
	case plan.VerdictFail:
		return fmt.Sprintf("Run stopped: FAIL - %s%s - a fault was reported; see the findings in the report and journal.", detail, fnd)
	case plan.VerdictUnverified:
		return fmt.Sprintf("Run stopped: UNVERIFIED - %s%s - no fault was confirmed, but the run did not confirm its checks either.", detail, fnd)
	case plan.VerdictPass:
		// The recorded runs all passed, yet the attempt as a whole
		// stopped non-zero: the stop itself is the fact, not a pass.
		return fmt.Sprintf("Run stopped (exit %d): INCOMPLETE - the runs that finished all passed, but the attempt did not finish cleanly.", code)
	default:
		return fmt.Sprintf("Run stopped: INCOMPLETE - %s%s - the load did not finish as planned; report and journal are ready.", detail, fnd)
	}
}

// runFinishNote states what an exit-0 ending actually achieved, read
// from the attempt receipt's own recorded verdict - never upgraded,
// never guessed from the exit code alone. It returns ("", "") when the
// code 0 armed no attempt (a cancelled dialog: the receipt on disk is
// provably still the pre-run one) or when the receipt is unreadable:
// the run's own lines above carry its story, and inventing an ending
// from an unreadable file would be a guess.
func runFinishNote(beforeAttempt string) (note, colorCode string) {
	r, err := loadLastResult()
	if err != nil || r.AttemptID == "" || r.AttemptID == beforeAttempt {
		return "", ""
	}
	if r.State == receiptPending {
		return fmt.Sprintf("Run ended (exit %d) but the attempt never reported an ending (crash mid-run?); the report and journal carry the story.", 0), tui.FGYellow
	}
	v := verdictOfReceipt(r)
	cnt := fmt.Sprintf("%d/%d run(s) finished", r.RunsFinished, r.RunsPlanned)
	if r.RunsPlanned == 0 {
		cnt = fmt.Sprintf("%d run(s) finished", r.RunsFinished)
	}
	fnd := ""
	if r.Findings > 0 {
		fnd = fmt.Sprintf(", %d finding(s) recorded", r.Findings)
	}
	switch v.Kind {
	case plan.VerdictPass:
		detail := v.Reason
		if detail == "" {
			detail = "every finished run confirmed its own verification"
		}
		return fmt.Sprintf("Run finished: RESULT: PASS - %s (%s%s). The summary above is the short story; 'report' the long one.", detail, cnt, fnd), ""
	case plan.VerdictFail:
		// The exit says clean, the receipt says a fault was recorded:
		// the receipt wins, this must not read as a success.
		return fmt.Sprintf("Run finished (exit 0) but the receipt records RESULT: FAIL - %s%s. Treat the cores/regions named by the findings as suspect.", v.Reason, fnd), tui.FGBrightRed
	case plan.VerdictUnverified:
		detail := v.Reason
		if detail == "" {
			detail = "the load completed but its correctness contract was not confirmed"
		}
		return fmt.Sprintf("Run finished: RESULT: UNVERIFIED - %s (%s%s). No fault was confirmed, but the checks did not confirm themselves either.", detail, cnt, fnd), tui.FGYellow
	default:
		return fmt.Sprintf("Run finished (exit 0): RESULT: %s - %s (%s%s); the report and journal carry the story.", v.Kind, v.Reason, cnt, fnd), tui.FGYellow
	}
}

// lastAttemptID reads the current receipt's attempt id ("" when no
// readable receipt). The menu captures it before a run so a stop
// exit can tell this run's ending from an older run's: a refusal
// that never armed an attempt must not borrow the previous run's
// story.
func lastAttemptID() string {
	r, err := loadLastResult()
	if err != nil {
		return ""
	}
	return r.AttemptID
}

// menuStopExit ends the menu session after a run whose code says the
// session must end (setup error, force quit). The honest stop reason
// is stated and the screen is held until Enter: previously the exit
// closed the terminal tab with the report still unread, which looked
// exactly like "the program fell over" - the exit code was right,
// the story was lost.
func menuStopExit(in *bufio.Reader, ui *screenUI, code int, beforeAttempt string) int {
	ui.stopMonitor() // nil-safe: releases the screen from the live panel
	n := runStopNote(code)
	if r, err := loadLastResult(); err == nil && r.AttemptID != "" && r.AttemptID == beforeAttempt {
		// The receipt on disk is the one that was already there when
		// this run was chosen: this run never armed an attempt (it
		// was refused before load), so its ending must not reuse the
		// previous run's story - the refusal line printed above is
		// the real reason.
		n = fmt.Sprintf("Run refused (exit %d) before an attempt was armed - see the refusal line above; nothing was loaded.", code)
	}
	if n != "" {
		fmt.Fprintln(os.Stderr, n)
	}
	waitForEnter(in)
	return code
}

func menuLines(ui *screenUI) []string {
	lines := []string{"=== CPU Stability Checker ==="}
	if lr, err := loadLastRun(); err == nil {
		// Menu plan section 1: the saved test's short parameters and
		// its last outcome are visible on the screen right away - the
		// outcome line comes from the attempt receipt (C3) and says
		// "unknown" honestly when there is none. Over a real recipe
		// Enter is the item-1 shortcut, so the selected-default marker
		// (a text glyph, NO_COLOR-safe) sits on item 1; without one
		// Enter starts nothing and no item is marked.
		text, code := outcomeSummary(lr)
		lines = append(lines,
			"› 1) Repeat the last test",
			"     "+recipeSummaryLine(lr),
			"     "+ui.paint(code, text))
	} else {
		lines = append(lines,
			"  1) Repeat the last test",
			"     No previous test yet")
	}
	return append(lines,
		"",
		"  2) CPU check",
		"  3) RAM check",
		"  4) Mixed CPU + RAM tests",
		"",
		"  5) Session history and reports",
		"  6) Settings",
		"  0) Quit",
	)
}

// hasSavedRecipe gates the Enter shortcut: Enter repeats only an
// actually saved request, never a guess.
func hasSavedRecipe() bool {
	_, err := loadLastRun()
	return err == nil
}

// recipeSummaryLine is the compact "what will replay" line. It states
// what the recipe holds; when the recipe knows its catalog profile
// the card's own title names it, otherwise the raw workload/phase
// shape does - never an invented profile.
func recipeSummaryLine(lr *lastRun) string {
	if lr.Kind == lastRunSession && lr.Session != nil {
		s := lr.Session
		mode := "all cores"
		if s.Mode == sessionPerCore {
			mode = "per core"
		}
		what := s.Workload
		if p, ok := catalog.Get(lr.ProfileID); ok && lr.ProfileID != "" {
			what = p.Title
		}
		return fmt.Sprintf("%s · %s · %d cores · %s per pass · %d pass(es)",
			what, mode, len(s.PhysicalCores), human.Dur(s.Duration), s.Passes)
	}
	if lr.Kind == lastRunHandoff && lr.Handoff != nil {
		h := lr.Handoff
		return fmt.Sprintf("Handoff · %d cores · %d blocks × %s · %d round(s). Repeat arms a new attempt of the same geometry and seed (P07e S6).",
			len(h.Cores), h.Blocks, human.Bytes(h.BlockBytes), h.Rounds)
	}
	return fmt.Sprintf("Scenario · %d phase(s) · %d cycle(s).", len(lr.Phases), max(lr.Cycles, 1))
}

// outcomeSummary maps the bound receipt onto one honest verdict line
// plus its semantic color code ("" = neutral). The mapping never
// upgrades: an attempt that faulted is not "stopped", an unverified
// completion is not "no errors", and a missing or foreign receipt
// says unknown. The color only decorates: faults red, unknown and
// partial states yellow, always over the words that already say it
// (menu plan section 7).
func outcomeSummary(lr *lastRun) (text, colorCode string) {
	r, err := loadLastResult()
	if err != nil || lr.RecipeID == "" || r.RecipeID != lr.RecipeID {
		// UNKNOWN exactly per P13: the record itself is missing,
		// corrupt or belongs to a different saved request.
		return "Result: UNKNOWN - no readable receipt for this saved recipe", tui.FGYellow
	}
	if r.State == receiptPending {
		return "Result: INCOMPLETE - attempt started and not finished (power loss or still running?)", tui.FGYellow
	}
	v := verdictOfReceipt(r)
	switch v.Kind {
	case plan.VerdictPass:
		return "Result: " + v.String(), ""
	case plan.VerdictFail:
		return "Result: " + v.String(), tui.FGBrightRed
	default:
		return "Result: " + v.String(), tui.FGYellow
	}
}

// settingsMenu holds the lower-traffic scenario and config-plan
// commands plus the diagnostics submenu. An empty answer goes back
// rather than starting a stress run by accident.
func settingsMenu(in *bufio.Reader, ui *screenUI, cfg config.Config, worst *int) int {
	note := ""
	for {
		lines := []string{
			"--- Settings ---",
			"  1) Scenario from config.json — run",
			"  2) Scenario from config.json — view",
			"  3) Scenario builder (burst/pairs/curve)",
			"  4) Plan from config.json — run",
			"  5) Plan from config.json — view",
			"  6) Diagnostics",
			"  q) Back to the main menu",
		}
		if note != "" {
			lines = append(lines, "", note)
			note = ""
		}
		ui.screenPrompt(lines, "Action [q]: ")

		line, readErr := in.ReadString('\n')
		choice := strings.ToLower(strings.TrimSpace(line))
		if readErr != nil && choice == "" {
			fmt.Println()
			return 0
		}
		if choice == "" || choice == "q" || choice == "0" {
			return 0
		}
		switch choice {
		case "1":
			before := lastAttemptID()
			code := scenarioCmdWithUI(nil, ui)
			if stop, exit := menuRunOutcome(in, ui, worst, code, before); stop {
				return menuStopExit(in, ui, exit, before)
			}
			note = ui.paint(tui.FGBrightRed, runStopNote(code))
		case "2":
			code := scenarioCmd([]string{"-dry-run"})
			if code != 0 {
				return code
			}
		case "3":
			before := lastAttemptID()
			code := scenarioSession(in, ui, cfg)
			if stop, exit := menuRunOutcome(in, ui, worst, code, before); stop {
				return menuStopExit(in, ui, exit, before)
			}
			note = ui.paint(tui.FGBrightRed, runStopNote(code))
		case "4":
			before := lastAttemptID()
			code := runConfiguredIOWithUI(true, in, os.Stdout, ui)
			if stop, exit := menuRunOutcome(in, ui, worst, code, before); stop {
				return menuStopExit(in, ui, exit, before)
			}
			note = ui.paint(tui.FGBrightRed, runStopNote(code))
		case "5":
			code := runConfiguredIO(false, in, os.Stdout)
			if code != 0 {
				return code
			}
		case "6":
			if code := diagnosticsMenu(in, ui, cfg, worst); code != 0 {
				return code
			}
		default:
			bad := "Enter a number from 1 to 6, or q to go back."
			if ui.hasScreen() {
				note = ui.paint(tui.FGBrightRed, bad)
			} else {
				ui.note(ui.paint(tui.FGBrightRed, bad))
			}
			continue
		}
		ui.waitForEnter(in)
	}
}

// diagnosticsMenu keeps the platform inspection and developer tools
// out of the task menu but reachable (menu plan section 3: fault,
// soak, raw journal and friends stay explicit diagnostics, not user
// profiles).
func diagnosticsMenu(in *bufio.Reader, ui *screenUI, cfg config.Config, worst *int) int {
	note := ""
	for {
		lines := []string{
			"--- Diagnostics ---",
			"  1) CPU topology (cores, SMT, NUMA, CCX)",
			"  2) Algorithm list",
			"  3) Journal: recover run results",
			"  4) WHEA errors",
			"  5) Leak soak (100 cycles: all modes + fault)",
			"  6) Core-parking observation (10 s) - what the OS reports, not a claim",
			"  7) Telemetry capabilities",
			"  q) Back",
		}
		if note != "" {
			lines = append(lines, "", note)
			note = ""
		}
		ui.screenPrompt(lines, "Action [q]: ")

		line, readErr := in.ReadString('\n')
		choice := strings.ToLower(strings.TrimSpace(line))
		if readErr != nil && choice == "" {
			fmt.Println()
			return 0
		}
		if choice == "" || choice == "q" || choice == "0" {
			return 0
		}
		switch choice {
		case "1":
			topology, err := sysinfo.DiscoverTopology()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Cannot discover CPU topology: %v\n", err)
				return 2
			}
			fmt.Println()
			sysinfo.WriteTopology(os.Stdout, topology)
		case "2":
			fmt.Println()
			list()
		case "3":
			fmt.Println()
			journalCmd(nil)
		case "4":
			fmt.Println()
			wheaCmd(nil)
		case "5":
			before := lastAttemptID()
			code := soakCmdWithUI(nil, ui)
			if stop, exit := menuRunOutcome(in, ui, worst, code, before); stop {
				return menuStopExit(in, ui, exit, before)
			}
			note = ui.paint(tui.FGBrightRed, runStopNote(code))
		case "6":
			fmt.Println()
			parkwatchCmd(nil)
		case "7":
			fmt.Println()
			capsCmd(nil)
		default:
			bad := "Enter a number from 1 to 7, or q to go back."
			if ui.hasScreen() {
				note = ui.paint(tui.FGBrightRed, bad)
			} else {
				ui.note(ui.paint(tui.FGBrightRed, bad))
			}
			continue
		}
		ui.waitForEnter(in)
	}
}

// categoryCards returns the category's cards at one visibility level,
// in the catalog's display order.
func categoryCards(cat catalog.Category, visibility catalog.Visibility) []catalog.Profile {
	var out []catalog.Profile
	for _, p := range catalog.All() {
		if p.Category == cat && p.Visibility == visibility {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// categoryAccent is the menu plan's section 7 color coding: CPU blue,
// RAM light blue, MIX purple. It decorates the header text that
// already names the category, and NO_COLOR or plain output drop it
// whole - the words stay the authority.
func categoryAccent(cat catalog.Category) string {
	switch cat {
	case catalog.CPU:
		return tui.FGBlue
	case catalog.RAM:
		return tui.FGBrightBlue
	case catalog.MIX:
		return tui.FGPurple
	}
	return ""
}

// cardLine lays out one profile row: number, title, and the summary
// clipped to the visible width (menu plan section 7: one short
// description line in the list; the unclipped summaries and the long
// Details sit behind the "Detailed description" choice). The
// unavailable mark is text, never color alone.
func cardLine(n int, title, summary string, unavailable bool, cols int) string {
	prefix := fmt.Sprintf("  %d) ", n)
	mark := ""
	if unavailable {
		mark = " (unavailable)"
	}
	head := prefix + title + mark
	avail := cols - tui.DisplayWidth(head+" — ")
	if avail < 8 {
		return head // no room for a summary: the title alone still names the goal
	}
	return head + " — " + tui.Truncate(summary, avail)
}

// detailsSubmenu reads the full description of a category's cards. Its
// rows carry MORE than the parent menu's clipped one-liners: number,
// title, the text unavailable mark, and the FULL summary wrapped over
// continuation lines. A pick adds the long Details text and, for an
// unavailable goal, its recorded Reason. Detail grows in exactly one
// direction - clipped showcase → full summaries → full contract -
// never the old backwards step where the "detailed" screen showed
// bare titles while the main screen already carried summaries (the
// owner: "на первом экране я вижу более подробное описание, чем в
// детальном"). A card without a Details text never renders empty:
// its recorded reason is the content. (menu plan section 7.)
func detailsSubmenu(in *bufio.Reader, ui *screenUI, cards []catalog.Profile) {
	for {
		cols := ui.width()
		lines := []string{"--- Detailed description ---"}
		for i, c := range cards {
			mark := ""
			if c.Availability != catalog.Available {
				mark = " (unavailable)" // text, never color alone
			}
			lines = append(lines, fmt.Sprintf("  %d) %s%s", i+1, c.Title, mark))
			lines = append(lines, wrapLines(c.Summary, cols-5)...)
		}
		lines = append(lines, "  q) Back")
		ui.screenPrompt(lines, "Profile [q]: ")

		line, readErr := in.ReadString('\n')
		choice := strings.ToLower(strings.TrimSpace(line))
		if readErr != nil && choice == "" {
			fmt.Println()
			return
		}
		if choice == "" || choice == "q" || choice == "0" {
			return
		}
		idx, err := strconv.Atoi(choice)
		if err != nil || idx < 1 || idx > len(cards) {
			continue
		}
		c := cards[idx-1]
		fmt.Printf("\n%s\n%s\n", c.Title, c.Summary)
		if c.Details != "" {
			fmt.Printf("\n%s\n", c.Details)
		}
		if c.Availability != catalog.Available {
			// The recorded reason belongs to the description: an
			// unavailable goal says why, right here, in its own
			// words - the same words the parent menu states on a
			// direct pick.
			fmt.Println("\n" + ui.paint(tui.FGBrightRed, "Not available: "+c.Reason))
		}
		ui.waitForEnter(in)
	}
}

// wrapLines word-wraps s into continuation lines of at most width
// display cells (width < 10 is clamped: a line that narrow would
// split single words). No ANSI is interpreted here: catalog text is
// plain data, and menu coloring is only ever added around whole
// lines, never inside these.
func wrapLines(s string, width int) []string {
	const indent = "     "
	if width < 10 {
		width = 10
	}
	width -= tui.DisplayWidth(indent) // wrapped lines carry the indent
	if width < 5 {
		width = 5
	}
	var out []string
	cur := ""
	curW := 0
	for _, w := range strings.Fields(s) {
		ww := tui.DisplayWidth(w)
		if cur == "" {
			cur, curW = w, ww
			continue
		}
		if curW+1+ww > width {
			out = append(out, indent+cur)
			cur, curW = w, ww
			continue
		}
		cur += " " + w
		curW += 1 + ww
	}
	if cur != "" {
		out = append(out, indent+cur)
	}
	return out
}

// categoryMenu lists the category's Normal cards in catalog order -
// unavailable ones visibly marked, never hidden (menu plan section 2:
// an unavailable goal is shown with its short reason, and selecting
// it states the full Reason and stays in the submenu: the refusal
// happens before any collection, save or load).
func categoryMenu(in *bufio.Reader, ui *screenUI, cfg config.Config, topology sysinfo.Topology, cat catalog.Category, title string, worst *int) int {
	cards := categoryCards(cat, catalog.Normal)
	extended := categoryCards(cat, catalog.Extended)
	accent := categoryAccent(cat)
	note := ""
	for {
		lines := []string{ui.paint(accent, "--- "+title+" ---")}
		cols := ui.width()
		for i, c := range cards {
			lines = append(lines, cardLine(i+1, c.Title, c.Summary, c.Availability != catalog.Available, cols))
		}
		next := len(cards)
		if len(extended) > 0 || cat == catalog.CPU {
			next++
			lines = append(lines, fmt.Sprintf("  %d) Extended profiles", next))
		}
		detailsIdx := next + 1
		lines = append(lines, fmt.Sprintf("  %d) Detailed description", detailsIdx), "  q) Back")
		if note != "" {
			lines = append(lines, "", note)
			note = ""
		}
		ui.screenPrompt(lines, "Profile [q]: ")

		line, readErr := in.ReadString('\n')
		choice := strings.ToLower(strings.TrimSpace(line))
		if readErr != nil && choice == "" {
			fmt.Println()
			return 0 // back out; the outer menu turns exhausted input into quit
		}
		if choice == "" || choice == "q" || choice == "0" {
			return 0
		}
		idx, err := strconv.Atoi(choice)
		if err != nil || idx < 1 || idx > detailsIdx {
			bad := "Enter a number from 1 to " + strconv.Itoa(detailsIdx) + " or q to go back."
			if ui.hasScreen() {
				note = ui.paint(tui.FGBrightRed, bad)
			} else {
				ui.note(ui.paint(tui.FGBrightRed, bad))
			}
			continue
		}
		if idx == detailsIdx {
			detailsSubmenu(in, ui, cards)
			continue
		}
		if idx == next && (len(extended) > 0 || cat == catalog.CPU) {
			if code := extendedMenu(in, ui, cfg, topology, cat, extended, worst); code != 0 {
				return code
			}
			continue
		}
		card := cards[idx-1]
		if card.Availability != catalog.Available {
			// The refusal is the card's own recorded Reason, stated in
			// red plus text before anything is collected or saved.
			fmt.Println()
			fmt.Println(ui.paint(tui.FGBrightRed, card.Title+" is not available: "+card.Reason))
			ui.waitForEnter(in)
			continue
		}
		before := lastAttemptID()
		code := startProfile(in, ui, cfg, topology, card)
		if stop, exit := menuRunOutcome(in, ui, worst, code, before); stop {
			return menuStopExit(in, ui, exit, before)
		}
		note = ui.paint(tui.FGBrightRed, runStopNote(code))
		if note != "" {
			ui.waitForEnter(in)
		}
	}
}

// extendedMenu offers the category's Extended-visibility cards plus,
// for CPU, the manual algorithm/external-engine choice (menu plan
// section 2 item 4). The manual wizard is honestly labeled as manual:
// it asks mode and workload from the full registry, and its choices
// stay unknown provenance when no card matches them.
func extendedMenu(in *bufio.Reader, ui *screenUI, cfg config.Config, topology sysinfo.Topology, cat catalog.Category, extended []catalog.Profile, worst *int) int {
	note := ""
	for {
		lines := []string{"--- Extended profiles ---"}
		cols := ui.width()
		for i, c := range extended {
			lines = append(lines, cardLine(i+1, c.Title, c.Summary, c.Availability != catalog.Available, cols))
		}
		next := len(extended)
		manual := cat == catalog.CPU
		if manual {
			next++
			lines = append(lines, fmt.Sprintf("  %d) Manually pick an algorithm or an external engine", next))
		}
		detailsIdx := next + 1
		lines = append(lines, fmt.Sprintf("  %d) Detailed description", detailsIdx), "  q) Back")
		if note != "" {
			lines = append(lines, "", note)
			note = ""
		}
		ui.screenPrompt(lines, "Profile [q]: ")

		line, readErr := in.ReadString('\n')
		choice := strings.ToLower(strings.TrimSpace(line))
		if readErr != nil && choice == "" {
			fmt.Println()
			return 0
		}
		if choice == "" || choice == "q" || choice == "0" {
			return 0
		}
		idx, err := strconv.Atoi(choice)
		if err != nil || idx < 1 || idx > detailsIdx {
			bad := "Enter a number from 1 to " + strconv.Itoa(detailsIdx) + " or q to go back."
			if ui.hasScreen() {
				note = ui.paint(tui.FGBrightRed, bad)
			} else {
				ui.note(ui.paint(tui.FGBrightRed, bad))
			}
			continue
		}
		if idx == detailsIdx {
			detailsSubmenu(in, ui, extended)
			continue
		}
		if manual && idx == next {
			before := lastAttemptID()
			code := interactiveSession(in, ui, cfg, topology)
			if stop, exit := menuRunOutcome(in, ui, worst, code, before); stop {
				return menuStopExit(in, ui, exit, before)
			}
			note = ui.paint(tui.FGBrightRed, runStopNote(code))
			if note != "" {
				ui.waitForEnter(in)
			}
			continue
		}
		card := extended[idx-1]
		if card.Availability != catalog.Available {
			fmt.Println()
			fmt.Println(ui.paint(tui.FGBrightRed, card.Title+" is not available: "+card.Reason))
			ui.waitForEnter(in)
			continue
		}
		before := lastAttemptID()
		code := startProfile(in, ui, cfg, topology, card)
		if stop, exit := menuRunOutcome(in, ui, worst, code, before); stop {
			return menuStopExit(in, ui, exit, before)
		}
		note = ui.paint(tui.FGBrightRed, runStopNote(code))
		if note != "" {
			ui.waitForEnter(in)
		}
	}
}

// startProfile routes one catalog card to the pipeline its shape
// compiles into: session shapes through the shared collector (C4),
// preset shapes through the single scenario pipeline. A card never
// grows a second executor.
func startProfile(in *bufio.Reader, ui *screenUI, cfg config.Config, topology sysinfo.Topology, card catalog.Profile) int {
	switch card.Shape {
	case catalog.ShapeSessionAll, catalog.ShapeSessionPerCore:
		return startSessionProfile(in, ui, cfg, topology, card)
	case catalog.ShapePreset:
		return startPresetProfile(in, ui, cfg, card)
	default:
		fmt.Fprintf(os.Stderr, "profile %q has no runnable shape in this build\n", card.ID)
		return 2
	}
}

// startSessionProfile launches one session-shape catalog card: the
// card fixes the goal-level choices (shape -> mode, workload), the
// shared collector gathers the rest, and the confirm/save/run tail is
// the wizard's own - one pipeline, one save-before-load promise. The
// card provenance is stamped as fact: the menu picked this card, no
// reverse inference needed.
func startSessionProfile(in *bufio.Reader, ui *screenUI, cfg config.Config, topology sysinfo.Topology, card catalog.Profile) int {
	if card.Availability != catalog.Available {
		fmt.Fprintf(os.Stderr, "profile %q (%s) is not available: %s\n", card.ID, card.Title, card.Reason)
		return 2
	}
	mode := sessionAllCore
	if card.Shape == catalog.ShapeSessionPerCore {
		mode = sessionPerCore
	}
	w := newWizard(in, ui, card.Title)
	fmt.Printf("%s — %s\n", card.Title, card.Summary)
	session, ok := collectSessionParams(w, cfg, topology, mode, card.Workload)
	if !ok {
		return 0
	}
	if !confirmSession(w, ui, topology, session) {
		return 0
	}
	lr := sessionRecipeForProfile(cfg, session, card)
	if err := saveLastRun(lr); err != nil {
		noteSaveFailure(err)
	}
	return runSessionWithPanel(session, topology, ui.monitor(), cfg.External, lr.RecipeID)
}

// confirmSession shows the concrete plan and asks once; cancelling
// starts no load and saves nothing.
func confirmSession(w *wizard, ui *screenUI, topology sysinfo.Topology, session SessionOptions) bool {
	var buf bytes.Buffer
	writeSessionPlan(&buf, session, topology)
	if !ui.hasScreen() {
		fmt.Println()
	}
	w.showPlan(strings.Split(strings.TrimRight(buf.String(), "\n"), "\n"))
	if !w.confirm() {
		fmt.Println("Run cancelled. No load was started.")
		return false
	}
	return true
}

// interactiveSession is the menu's "configure and start" flow: the
// wizard collects a complete session, the user confirms it, and the
// confirmed session becomes the repeat recipe BEFORE the first load
// (a crash mid-run must still leave an honest replay).
func interactiveSession(in *bufio.Reader, ui *screenUI, cfg config.Config, topology sysinfo.Topology) int {
	session, ok := configureSession(in, ui, cfg, topology)
	if !ok {
		return 0
	}
	lr := sessionRecipe(cfg, session)
	if err := saveLastRun(lr); err != nil {
		noteSaveFailure(err)
	}
	return runSessionWithPanel(session, topology, ui.monitor(), cfg.External, lr.RecipeID)
}

// startPresetProfile runs a catalog preset profile (menu plan section
// 2: transitions, comprehensive MIX) through the scenario pipeline -
// the single one: a preset only produces ordinary config.Phase values,
// so dry-run, journal, blacklist and repeat behave exactly as for a
// hand-written scenario (presets.go contract). C4 provides the entry;
// C5's menu wires it to the category items. Confirmation is required
// (this is a new test, not a repeat) and cancelling saves nothing;
// the gate and refusal wording mirror the scenario command's A05
// promises, in the profile's own words - a preset that verifies
// memory regions asks for the budget in the dialog when config has
// none (same knob the wizard offers), never quietly shrunk into a
// CPU-only run and no longer closed out with a config.json homework
// assignment.
func startPresetProfile(in *bufio.Reader, ui *screenUI, cfg config.Config, profile catalog.Profile) int {
	if profile.Availability != catalog.Available {
		fmt.Fprintf(os.Stderr, "profile %q (%s) is not available: %s\n",
			profile.ID, profile.Title, profile.Reason)
		return 2
	}
	if profile.Shape != catalog.ShapePreset {
		fmt.Fprintf(os.Stderr, "profile %q is not a preset profile (shape %q)\n", profile.ID, profile.Shape)
		return 2
	}
	p, ok := lookupPreset(profile.Preset)
	if !ok {
		fmt.Fprintf(os.Stderr, "profile %q names preset %q, which this build does not have\n",
			profile.ID, profile.Preset)
		return 2
	}
	if p.NeedsMemory && strings.TrimSpace(cfg.Memory) == "" {
		// configureSession's rule, now applied to menu-chosen presets
		// too: a fixable setup knob is asked in the dialog - it does
		// not die as a preflight refusal. The old wording said "pick
		// another profile" while offering no way to supply the budget
		// right there, even though the wizard asks this exact question
		// for ram sessions ("почему в других ситуациях [auto]
		// работает, а тут нет такого?"). The answer applies to THIS
		// run only; config.json is never rewritten from here.
		fmt.Printf("profile %q verifies memory regions and needs a memory budget.\n", p.Name)
		fmt.Println("It applies to this run only - to keep it, set \"memory\" in config.json.")
		for {
			value := ask(in, "Memory budget (\"auto\" or an exact size like 2GiB)", "auto")
			if strings.EqualFold(value, "q") {
				fmt.Println("Run cancelled. No load was started.")
				return 0
			}
			if err := plan.ValidateMemorySettings(value, cfg.MemoryLock); err != nil {
				fmt.Printf("Invalid memory budget: %v\n", err)
				continue
			}
			cfg.Memory = value
			break
		}
	}
	cfg.Phases = p.Phases(cfg)
	fmt.Printf("preset %q: %s\n", p.Name, p.Description)
	opts, notices, err := compileScenario(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "preset %q: %v\n", p.Name, err)
		return 2
	}
	for _, n := range notices {
		fmt.Println(n)
	}
	// Refuse before the confirm could ask for a run the machine
	// cannot honor, and before any save (audit A04).
	if errs := scenarioMemoryRefusals(cfg.External, opts); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "preset %q: %s\n", p.Name, e)
		}
		return 2
	}
	w := newWizard(in, ui, "Preset: "+profile.Title)
	var buf bytes.Buffer
	printScenarioPlan(&buf, cfg, opts, 1)
	w.showPlan(strings.Split(strings.TrimRight(buf.String(), "\n"), "\n"))
	if !w.confirm() {
		fmt.Println("Run cancelled. No load was started.")
		return 0
	}
	lr := scenarioRecipe(cfg, 1, nil, false, p.Name)
	return runScenarioRequest(lr, opts, 1, -1, false, ui, true)
}

// configureSession drives the wizard up to and including the confirm
// step and returns the session the user described; ok=false means the
// user cancelled or went back, so nothing is saved and no load starts.
// RAM-family workloads (audit A03) get their engine requirements asked
// HERE - the per-process mode they need and the budget they verify -
// so a fixable setup problem returns to the form instead of dying in
// the executor's preflight refusal, which the menu would treat as a
// fatal setup error and close the whole menu on top.
func configureSession(in *bufio.Reader, ui *screenUI, cfg config.Config, topology sysinfo.Topology) (SessionOptions, bool) {
	w := newWizard(in, ui, "Configure and start a test")

	modeIndex := w.choose("Test mode", "Choose a mode", []string{
		"All-core — load all selected physical cores at the same time",
		"Per-core — test the selected physical cores one after another",
	}, 1)
	if modeIndex < 0 {
		return SessionOptions{}, false
	}
	mode := sessionAllCore
	if modeIndex == 1 {
		mode = sessionPerCore
	}

	load, ok := w.chooseWorkload(cfg.WorkloadA)
	if !ok {
		return SessionOptions{}, false
	}
	// An external workload whose tool is not on disk is a fixable
	// problem: state the reason, return to the workload list - never
	// let the run die in the child's setup after the user confirmed
	// (audit A04's "no hidden substitution" applied to fresh runs).
	for {
		msg := externalToolMissing(cfg.External, load)
		if msg == "" {
			break
		}
		if ui.hasScreen() {
			w.note = msg
		} else {
			fmt.Println(msg)
		}
		load, ok = w.chooseWorkload(cfg.WorkloadA)
		if !ok {
			return SessionOptions{}, false
		}
	}

	session, ok := collectSessionParams(w, cfg, topology, mode, load)
	if !ok {
		return SessionOptions{}, false
	}

	if !confirmSession(w, ui, topology, session) {
		return SessionOptions{}, false
	}
	return session, true
}

// collectSessionParams gathers the goal-level parameters for a chosen
// mode+workload (C4): the execution policy resolved from the workload,
// the physical cores, SMT threads, duration, passes and - when the
// workload verifies data in memory-engine regions - the RAM budget.
// The step order and re-prompt semantics are configureSession's
// existing ones: this is a pure extraction, not a redesign. C5's
// category wizards call the same collector per catalog profile, so no
// menu item may copy configureSession. q/EOF at any step cancels with
// no partial state; the plan+confirm screen stays with the caller,
// which owns the save-before-load promise.
func collectSessionParams(w *wizard, cfg config.Config, topology sysinfo.Topology, mode SessionMode, load string) (SessionOptions, bool) {
	// U01 auto-execution (menu plan section 4): the wizard no longer
	// asks "in-process or per-process" - that is a mechanism, not a
	// user goal. The policy resolver picks the mode from the
	// workload's hard requirements (ram-* -> subprocess: no question,
	// no re-ask loop), and the reason rides into the plan the user
	// confirms. A settings override that contradicts the workload is
	// fixed in Settings, not in this form: state it, wait, go back to
	// the menu - never die inside the executor's preflight.
	dec, derr := plan.ResolveExecution(cfg.Execution, load, load, 0)
	if derr != nil {
		fmt.Fprintln(os.Stdout, derr)
		w.ui.waitForEnter(w.in)
		return SessionOptions{}, false
	}
	perProcess, rotate := dec.PerProcess, dec.Rotate
	policy := strings.ToLower(strings.TrimSpace(cfg.Execution))
	if policy == "" {
		policy = config.ExecutionAuto
	}

	// A RAM-family workload verifies data inside memory-engine
	// regions: its budget is collected in the form below. The mode it
	// needs was just resolved from its requirement - never switched
	// silently, and the engine is never promised regions it cannot
	// carry.
	needsMemory := false
	if wl, ok := workload.Get(load); ok {
		needsMemory = wl.RequiresMemory
	}

	nPhysical := len(topology.PhysicalCores)
	w.hints = []string{
		fmt.Sprintf("Detected %d physical cores, numbered 0 through %d.", nPhysical, nPhysical-1),
		"Enter a list such as 1,2,5-8,12. Ranges are inclusive; 'all' selects every core.",
	}
	if !w.ui.hasScreen() {
		fmt.Println("\nPhysical cores")
		for _, hint := range w.hints {
			fmt.Println(hint)
		}
	}
	var physicalCores []int
	for {
		value := w.promptLine("Physical cores", "all")
		selected, err := parseNumberSet(value, nPhysical)
		if err == nil {
			physicalCores = selected
			w.record("Physical cores", value)
			break
		}
		if w.ui.hasScreen() {
			w.note = fmt.Sprintf("Invalid core selection: %v", err)
		} else {
			fmt.Printf("Invalid core selection: %v\n", err)
		}
	}
	w.hints = nil

	threadsPerCore := 1
	if selectedHaveSMT(topology, physicalCores) {
		w.hints = []string{
			"One thread per core runs each physical core's single best thread: cooler, higher boost,",
			"and faults point at one physical core. Two threads load both SMT siblings of every",
			"core: maximum total stress (and much longer Prime95 iterations).",
		}
		threadIndex := w.choose("Threads per core", "Choose thread usage", []string{
			"One logical CPU per physical core — lower heat and usually higher boost",
			"All SMT siblings — load both logical CPUs of each physical core",
		}, 0)
		if threadIndex < 0 {
			return SessionOptions{}, false
		}
		if threadIndex == 1 {
			threadsPerCore = 2
		}
		w.hints = nil
	}

	duration := w.askDuration("Duration", "Duration of each run", cfg.Duration)
	if duration <= 0 {
		return SessionOptions{}, false
	}

	passes := w.askPositiveInt("Passes", "Number of complete passes", 1)
	if passes <= 0 {
		return SessionOptions{}, false
	}

	session := sessionDefaults(cfg)
	session.Mode, session.PhysicalCores, session.Workload = mode, physicalCores, load
	session.ThreadsPerCore, session.Duration, session.Passes = threadsPerCore, duration, passes
	session.PerProcess, session.Rotate = perProcess, rotate
	session.Execution, session.ExecutionReason = policy, dec.Reason

	if needsMemory {
		// The budget the RAM family verifies against is collected here
		// (config value as the default, "auto" when config has none):
		// an invalid answer re-prompts with the reason, it never
		// reaches - and never dies inside - the executor's preflight.
		def := strings.TrimSpace(session.Memory)
		if def == "" {
			def = "auto"
		}
		w.hints = []string{
			"RAM verification needs a memory budget: 'auto' (a bounded share of free",
			"RAM, reserving room for the OS) or an exact size such as 2GiB.",
		}
		if !w.ui.hasScreen() {
			fmt.Println("\nMemory budget")
			for _, hint := range w.hints {
				fmt.Println(hint)
			}
		}
		for {
			value := w.promptLine("Memory budget", def)
			if strings.EqualFold(strings.TrimSpace(value), "q") {
				return SessionOptions{}, false
			}
			if err := plan.ValidateMemorySettings(value, session.MemoryLock); err == nil {
				session.Memory = strings.TrimSpace(value)
				w.record("Memory budget", value)
				break
			} else {
				w.note = fmt.Sprintf("Invalid memory budget: %v", err)
			}
		}
		w.hints = nil
	}
	return session, true
}

func choose(in *bufio.Reader, prompt string, items []string, def int) int {
	for i, item := range items {
		fmt.Printf("  %d) %s\n", i+1, item)
	}
	for {
		fmt.Printf("%s [%d] (q=back): ", prompt, def+1)
		line, _ := in.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return def
		}
		if strings.EqualFold(line, "q") || line == "0" {
			return -1
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(items) {
			return n - 1
		}
		fmt.Printf("Enter a number from 1 to %d, or q to go back.\n", len(items))
	}
}

func chooseWorkload(in *bufio.Reader, defaultName string) (string, bool) {
	names := workload.Names()
	items := make([]string, len(names))
	def := 0
	for i, name := range names {
		w, _ := workload.Get(name)
		items[i] = fmt.Sprintf("%-7s %s", name, w.Description)
		if name == defaultName {
			def = i
		}
	}
	selected := choose(in, "Choose an algorithm", items, def)
	if selected < 0 {
		return "", false
	}
	return names[selected], true
}

func ask(in *bufio.Reader, prompt, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", prompt, def)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	line, _ := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func askDuration(in *bufio.Reader, prompt, defaultValue string) time.Duration {
	for {
		value := ask(in, prompt+" (examples: 30s, 5m, 1h)", defaultValue)
		if strings.EqualFold(value, "q") {
			return 0
		}
		duration, err := time.ParseDuration(value)
		if err == nil && duration > 0 {
			return duration
		}
		fmt.Println("Enter a positive duration such as 30s, 5m, or 1h; q goes back.")
	}
}

func askPositiveInt(in *bufio.Reader, prompt string, defaultValue int) int {
	for {
		value := ask(in, prompt, strconv.Itoa(defaultValue))
		if strings.EqualFold(value, "q") {
			return 0
		}
		n, err := strconv.Atoi(value)
		if err == nil && n > 0 {
			return n
		}
		fmt.Println("Enter a positive whole number; q goes back.")
	}
}

func waitForEnter(in *bufio.Reader) {
	fmt.Print("\nPress Enter to return to the menu...")
	_, _ = in.ReadString('\n')
	fmt.Println()
}
