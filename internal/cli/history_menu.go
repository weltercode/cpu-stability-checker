package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/tui"
)

// historyMenu is menu item 5 (U02): the recent sessions read from
// their manifests - not from whatever journal happens to be current
// - and every action here is read-only until the user explicitly
// asks for a replay, or names the exact word that confirms the ONE
// destructive action this screen offers (R37: the owner named history
// cleanup as a deliberate, confirmed choice - reading history still
// never rewrites or deletes anything by itself).
const historyShow = 10 // the acceptance line: the last 10 sessions are visible

func historyMenu(in *bufio.Reader, ui *screenUI, worst *int) int {
	note := ""
	for {
		list, warns := listSessionManifests()
		shown := list
		if len(shown) > historyShow {
			shown = shown[:historyShow]
		}
		lines := []string{tx("--- Session history ---"), tx("  (each line starts with the local start time; session ids stay UTC)")}
		for i, m := range shown {
			lines = append(lines, fmt.Sprintf("  %d) %s", i+1, sessionHistoryLine(ui, m)))
		}
		if len(list) > historyShow {
			lines = append(lines, fmt.Sprintf(tx("  showing %d of %d"), historyShow, len(list)))
		}
		if len(list) == 0 {
			lines = append(lines, tx("  No sessions yet: run a test and it will appear here."))
		}
		lines = append(lines, "", tx("  j) Report on the current journal (every run in it)"),
			tx("  c) Clear ALL history: every record plus its log/journal/fragment files (asks for the exact word)"),
			tx("  q) Back to the main menu"))
		for _, w := range warns {
			// A corrupt manifest gets its own loud line; the rest of
			// history stays listed (section 6: one bad entry never
			// breaks the list).
			lines = append(lines, ui.paint(tui.FGYellow, tx("  notice: ")+w))
		}
		if note != "" {
			lines = append(lines, "", note)
			note = ""
		}
		ui.screenPrompt(lines, tx("Action [q]: "))

		choice, eof := navChoice(in, ui, lines, tx("Action [q]: "), "q")
		if eof {
			fmt.Println()
			return 0
		}
		switch {
		case choice == "" || choice == "q":
			return 0
		case choice == "j":
			fmt.Println()
			reportCmd(nil)
			ui.waitForEnter(in)
		case choice == "c":
			// R37: the one destructive screen. Preview first, then
			// the exact word - an accidental keystroke must never
			// empty history, so 'y' is NOT enough: the operator types
			// "clear" itself. Anything else, EOF included, cancels
			// without touching a byte.
			fmt.Println()
			rr := clearHistory(true)
			if rr.records == 0 && rr.unreadable == 0 {
				fmt.Println(tx("Nothing recorded: history is already empty."))
				ui.waitForEnter(in)
				continue
			}
			for _, l := range clearPreview(rr) {
				fmt.Println(l)
			}
			fmt.Print(tx("Type the word clear to remove everything; anything else cancels: "))
			confirm, cerr := in.ReadString('\n')
			if cerr != nil {
				fmt.Println()
			}
			if strings.ToLower(strings.TrimSpace(confirm)) == "clear" && cerr == nil {
				res := clearHistory(false)
				note = strings.Join(clearSummary(res), "\n")
			} else {
				note = tx("cancelled: nothing was removed.")
			}
		default:
			idx, err := strconv.Atoi(choice)
			if err != nil || idx < 1 || idx > len(shown) {
				bad := fmt.Sprintf(tx("Enter a number from 1 to %d, j or q."), len(shown))
				note = ui.paint(tui.FGBrightRed, bad)
				continue
			}
			if code := sessionMenu(in, ui, shown[idx-1].SessionID, worst); code != 0 {
				return code
			}
			// The loop rescans: a replay that just ran added a fresh
			// session to history.
		}
	}
}

// sessionHistoryLine renders one session on one line: the local start
// time first (that is the clock the operator lives by - a 20:29 UTC id
// reads as "afternoon" at 23:29 local otherwise), the full id (it
// doubles as its own start time and as the file name), the entry
// point kind, and how it ended. The wording never upgrades - the same
// no-lie mapping as the repeat screen's outcome line - and color only
// decorates words that already say it.
func sessionHistoryLine(ui *screenUI, m sessionManifest) string {
	state, color := sessionStateWords(m)
	when := ""
	if lt := localClockText(m.Started); lt != "" {
		when = lt + " · "
	}
	return fmt.Sprintf("%s%s · %s · %s", when, m.SessionID, kindWord(m.Kind), ui.paint(color, state))
}

// localClockText renders one UTC RFC3339 stamp in the machine's local
// calendar for HUMAN reading. Manifests, ids and journals stay UTC
// forever (files must not mean different things in different
// timezones, and ids double as file names): only this display converts.
// An unparseable stamp yields "" - an unknown moment never gets an
// invented clock.
func localClockText(utc string) string {
	t, err := time.Parse(time.RFC3339, utc)
	if err != nil {
		return ""
	}
	return t.In(time.Local).Format("02.01.2006 15:04")
}

// kindWord names the entry point in the UI language. The machine kind
// and its English word coincide by design; the function still exists
// so an unknown kind displays raw (an honest pass-through) instead of
// a translation that would guess at a shape this build never produced.
func kindWord(kind string) string {
	switch kind {
	case "run":
		return "run"
	case "plan":
		return "plan"
	case "session":
		return "session"
	case "scenario":
		return "scenario"
	case "handoff":
		return "handoff"
	}
	return kind
}

// sessionStateWords maps the manifest state onto honest words plus
// the semantic color code ("" = neutral). A manifest still saying
// "running" reads as unfinished at display time - the manifest
// itself is never rewritten, and no finish time is invented.
func sessionStateWords(m sessionManifest) (text, colorCode string) {
	switch m.State {
	case manifestRunning:
		return "not finished (crash or still running?)", tui.FGYellow
	case "unknown":
		return "result unknown", tui.FGYellow
	case string(plan.ExecCompleted):
		switch m.Validation {
		case string(plan.ValVerified), string(plan.ValExternalConfirmed):
			return "no errors found", ""
		default:
			return "completed, verification not confirmed", tui.FGYellow
		}
	case string(plan.ExecRuntimeError):
		return "verification error", tui.FGBrightRed
	case string(plan.ExecSetupError):
		return "setup failure", tui.FGBrightRed
	case string(plan.ExecInterrupted), string(plan.ExecCancelled):
		return "stopped", tui.FGYellow
	}
	return m.State, tui.FGYellow // states this build does not know keep their own words
}

// sessionMenu offers the four things history exists for: the
// session-bound report, its details, an explicit replay, and a
// read-only compare against another recorded session (R35). The
// report, details and compare are read-only; replay is the only
// action that starts load, and it goes through the ordinary entry
// points (fresh session id, fresh receipt - history replays
// requests, it does not resurrect old sessions).
func sessionMenu(in *bufio.Reader, ui *screenUI, sessionID string, worst *int) int {
	note := ""
	for {
		lines := []string{
			fmt.Sprintf(tx("--- Session %s ---"), sessionID),
			tx("  1) Report on this session"),
			tx("  2) Details (plan and files)"),
			tx("  3) Repeat this request"),
			tx("  4) Compare with another session (read-only)"),
			tx("  q) Back to history"),
		}
		if note != "" {
			lines = append(lines, "", note)
			note = ""
		}
		ui.screenPrompt(lines, tx("Action [q]: "))

		choice, eof := navChoice(in, ui, lines, tx("Action [q]: "), "q")
		if eof {
			fmt.Println()
			return 0
		}
		switch choice {
		case "", "q":
			return 0
		case "1":
			fmt.Println()
			reportSession(sessionID, false, false)
			ui.waitForEnter(in)
		case "2":
			fmt.Println()
			printSessionDetails(os.Stdout, sessionID)
			ui.waitForEnter(in)
		case "3":
			m, err := loadSessionManifest(sessionID)
			if err != nil {
				note = ui.paint(tui.FGBrightRed, err.Error())
				continue
			}
			if m.Request == nil {
				note = ui.paint(tui.FGYellow, tx("Repeat unavailable: this session's manifest stores no request."))
				continue
			}
			// R25: a history replay is a load too - the plan screen and
			// an explicit yes come before anything is armed; a refusal
			// leaves the repeat slot exactly as it was.
			if !confirmRepeat(in, ui, m.Request) {
				note = tx("Cancelled: nothing armed, no load started, the repeat slot keeps the previous test.")
				continue
			}
			if err := saveLastRun(*m.Request); err != nil {
				// The replay slot must actually hold THIS request
				// before repeating: replaying the stale slot would
				// answer a question nobody asked.
				note = ui.paint(tui.FGBrightRed, fmt.Sprintf(tx("Request not armed: %v"), err))
				continue
			}
			before := lastAttemptID()
			code := repeatLastRunWithUI(ui)
			if stop, exit := menuRunOutcome(in, ui, worst, code, before); stop {
				return menuStopExit(in, ui, exit, before)
			}
			return 0 // back to the history list: it rescans and shows the new session
		case "4":
			// R35: the read-only compare, same engine the 'compare'
			// command uses. Choosing a partner is navigation, not a
			// load: nothing is confirmed, nothing is written, and an
			// unresolvable choice just returns to this menu.
			partners := comparePartners(sessionID)
			if len(partners) == 0 {
				note = ui.paint(tui.FGYellow, tx("Nothing to compare with: this is the only recorded session."))
				continue
			}
			shown := partners
			if len(shown) > historyShow {
				shown = shown[:historyShow]
			}
			fmt.Println()
			fmt.Printf(tx("Compare %s with which session (number, or the exact id)?\n"), sessionID)
			for i, pm := range shown {
				fmt.Printf("  %d) %s\n", i+1, sessionHistoryLine(ui, pm))
			}
			if len(partners) > len(shown) {
				fmt.Printf(tx("  (%d older sessions are not listed here - name one by its exact id)\n"), len(partners)-len(shown))
			}
			fmt.Print(tx("Partner [q cancels]: "))
			pline, perr := in.ReadString('\n')
			partner := strings.TrimSpace(pline)
			if perr != nil && partner == "" {
				fmt.Println()
				return 0
			}
			if partner == "" || strings.EqualFold(partner, "q") {
				note = tx("Compare cancelled: nothing was read beyond the history index, nothing changed.")
				continue
			}
			partnerID := ""
			if n, cerr := strconv.Atoi(partner); cerr == nil && n >= 1 && n <= len(shown) {
				partnerID = shown[n-1].SessionID
			} else {
				for _, pm := range partners {
					if pm.SessionID == partner { // exact id only: a prefix could silently merge two sessions
						partnerID = pm.SessionID
						break
					}
				}
			}
			if partnerID == "" {
				note = ui.paint(tui.FGBrightRed, tx("No such session: pick a listed number or name the exact id (ids are not guessed from fragments)."))
				continue
			}
			sideA, err := loadCompareSide(sessionID)
			if err != nil {
				note = ui.paint(tui.FGBrightRed, err.Error())
				continue
			}
			sideB, err := loadCompareSide(partnerID)
			if err != nil {
				note = ui.paint(tui.FGBrightRed, err.Error())
				continue
			}
			fmt.Println()
			printCompareText(os.Stdout, sideA, sideB, compareNotices(sideA.runs, sideB.runs))
			ui.waitForEnter(in)
		default:
			note = ui.paint(tui.FGBrightRed, tx("Enter 1, 2, 3, 4 or q."))
		}
	}
}

// comparePartners is the read-only partner list for the session
// menu: every recorded session except this one, newest first (the
// list itself is a fresh scan - history can never go stale).
func comparePartners(sessionID string) []sessionManifest {
	list, _ := listSessionManifests()
	out := make([]sessionManifest, 0, len(list))
	for _, m := range list {
		if m.SessionID != sessionID {
			out = append(out, m)
		}
	}
	return out
}

// printStamp prints one UTC stamp as stored, plus the local reading
// in parentheses when the stamp parses - the file truth and the human
// truth on one line, neither replaced by the other.
func printStamp(w io.Writer, label, utc string) {
	line := utc
	if lt := localClockText(utc); lt != "" {
		line += fmt.Sprintf(" (local %s)", lt)
	}
	fmt.Fprintf(w, "%s: %s\n", label, line)
}

// printSessionDetails states everything the manifest knows and
// honestly marks what it does not: no finish time after a crash,
// unknown exit, unbound recipe, files that went missing. It reads
// and stats only - nothing is rewritten, nothing is deleted.
func printSessionDetails(w io.Writer, sessionID string) {
	m, err := loadSessionManifest(sessionID)
	if err != nil {
		fmt.Fprintf(w, "%v\n", err)
		return
	}
	fmt.Fprintf(w, "id: %s\nkind: %s\n", m.SessionID, kindWord(m.Kind))
	state, _ := sessionStateWords(m)
	fmt.Fprintf(w, "state: %s\n", state)
	printStamp(w, "start", m.Started)
	if m.Finished == "" {
		fmt.Fprintln(w, "end: not recorded (power loss or still running?)")
	} else {
		printStamp(w, "end", m.Finished)
	}
	if m.Exit == nil {
		fmt.Fprintln(w, "exit code: unknown")
	} else {
		fmt.Fprintf(w, "exit code: %d\n", *m.Exit)
	}
	if m.Validation == "" {
		fmt.Fprintln(w, "verification: unknown")
	} else {
		fmt.Fprintf(w, "verification: %s\n", m.Validation)
	}
	if m.RecipeID != "" {
		fmt.Fprintf(w, "recipe: %s\n", m.RecipeID)
	} else {
		fmt.Fprintln(w, "recipe: unbound")
	}
	if m.ProfileID != "" {
		fmt.Fprintf(w, "profile: %s (version %s)\n", m.ProfileID, m.ProfileVersion)
	}
	if m.Request != nil {
		fmt.Fprintf(w, "request: %s\n", recipeSummaryLine(m.Request))
	} else {
		fmt.Fprintln(w, "request: not saved (unknown)")
	}
	if len(m.Runs) == 0 {
		fmt.Fprintln(w, "files: the session never listed its runs")
	}
	for _, r := range m.Runs {
		jp := r.Journal
		if jp == "" && r.Log != "" {
			jp = journal.Path(r.Log)
		}
		status := "present"
		if _, statErr := os.Stat(jp); statErr != nil {
			status = "missing"
		}
		fmt.Fprintf(w, "  %s: log %s; journal %s (%s)\n", r.Name, r.Log, jp, status)
	}
}
