package cli

// R42 (owner-named 20.11): arrow keys move the selection on the
// numbered menu screens, Enter selects, Esc goes back. The contract
// that must NOT break, in the owner's own words from the queue:
// typed numbers and the scripted stdin flows stay valid, EOF keeps
// its meaning, and destructive actions keep their full guard - an
// arrow can never replace the plan screen, the y or the exact word
// clear.
//
// Design (Sol kickoff review 21.11, corrected by the same-day
// implementation review): key events come from the console through
// tui.RawKeys, pulled ONE RECORD AT A TIME by this goroutine - no
// callback, no channel, no second reader - and the pulling stops the
// instant the answer is complete, so events still queued beyond the
// answer reach the next prompt exactly as cooked-mode typeahead does
// today. While raw is active RawKeys is the ONLY stdin reader; its
// console mode is restored (once, idempotent, and also from the
// console Ctrl/Break handler) before anything downstream reads
// again. The answer handed back is exactly the string the old bufio
// ReadString would have produced, so every downstream dispatch,
// consent gate and scripted test path is untouched. Where no console
// input handle exists (piped input, files, non-Windows, plain
// terminals) StartRawKeys returns nil and navChoice is
// byte-identical to the old line read.

import (
	"bufio"
	"os"
	"regexp"
	"strings"

	"github.com/weltercode/cpu-stability-checker/internal/tui"
)

// navInput is the injectable seam over *tui.RawKeys for tests.
type navInput interface {
	NextKey() (tui.Key, rune, uint32)
	Stop()
}

// startRawKeys binds the production console path; tests replace it.
var startRawKeys = func() navInput {
	if r := tui.StartRawKeys(); r != nil {
		return r
	}
	return nil
}

// navRowRE matches a numbered action row after the optional leading
// two spaces or the "› " selected marker: "  3) CPU check",
// "› 1) Repeat the last test". Words ("a)", "q)"), notices and
// paragraphs never match, so the cursor can never land on the
// destructive history action (c) or any letter token.
var navRowRE = regexp.MustCompile(`^([1-9][0-9]*)\) `)

// navItems returns the row indexes and number strings of the
// selectable items. The quit row "0)" is deliberately NOT selectable:
// exit and cancellation stay typed (or EOF), never one arrow away.
func navItems(lines []string) (rows []int, nums []string) {
	for i, l := range lines {
		body := l
		if b := strings.TrimPrefix(body, "› "); b != body {
			body = b
		} else if b := strings.TrimPrefix(body, "  "); b != body {
			body = b
		}
		if m := navRowRE.FindStringSubmatch(body); m != nil {
			rows = append(rows, i)
			nums = append(nums, m[1])
		}
	}
	return rows, nums
}

// navMark draws exactly one "› " marker on the cursor row. The
// static Enter-shortcut marker menuLines puts on item 1 is suppressed
// while the cursor is live, so the screen never shows two selections
// at once.
func navMark(lines []string, rows []int, cursor int) []string {
	out := make([]string, len(lines))
	copy(out, lines)
	if cursor < 0 || cursor >= len(rows) {
		return out
	}
	for i := range out {
		if s := strings.TrimPrefix(out[i], "› "); s != out[i] {
			out[i] = "  " + s
		}
	}
	row := rows[cursor]
	if !strings.HasPrefix(out[row], "› ") && strings.HasPrefix(out[row], "  ") {
		out[row] = "› " + strings.TrimPrefix(out[row], "  ")
	}
	return out
}

// navChoice reads the answer to one already-drawn numbered menu. The
// returned string obeys the old contract exactly: lower-case,
// trimmed, "" for a bare Enter with the cursor untouched (the
// per-screen default-action semantics survive), and eof=true only
// where the old code saw an exhausted input (piped EOF, Ctrl+Z, or
// a closed console).
func navChoice(in *bufio.Reader, ui *screenUI, lines []string, prompt, escChoice string) (choice string, eof bool) {
	readLine := func() (string, bool) {
		l, err := in.ReadString('\n')
		c := strings.ToLower(strings.TrimSpace(l))
		if err != nil && c == "" {
			return "", true // exhausted input means quit, exactly as before
		}
		return c, false
	}
	if !ui.hasScreen() {
		return readLine()
	}
	raw := startRawKeys()
	if raw == nil {
		return readLine()
	}
	defer raw.Stop()

	rows, nums := navItems(lines)
	cursor := -1 // no live cursor yet: the screen keeps its static default marker
	var buf []rune
	redraw := func() {
		ui.screenPrompt(navMark(lines, rows, cursor), prompt+string(buf))
	}
	for {
		k, r, n := raw.NextKey()
		switch k {
		case tui.KeyUp, tui.KeyLeft:
			if len(rows) > 0 {
				// A held arrow arrives coalesced as one record with a
				// repeat count; honour it so the cursor moves as many
				// times as the operator pressed, without overrunning.
				for i := uint32(0); i < n; i++ {
					if cursor <= 0 {
						cursor = len(rows) - 1
					} else {
						cursor--
					}
				}
				redraw()
			}
		case tui.KeyDown, tui.KeyRight:
			if len(rows) > 0 {
				for i := uint32(0); i < n; i++ {
					if cursor < 0 || cursor == len(rows)-1 {
						cursor = 0
					} else {
						cursor++
					}
				}
				redraw()
			}
		case tui.KeyRune:
			// A held or fast-typed digit arrives coalesced as ONE
			// record counting N presses. Append every press: ignoring
			// the count would turn a typed "11" into "1" and select
			// the wrong action.
			for i := uint32(0); i < n; i++ {
				buf = append(buf, r)
			}
			redraw()
		case tui.KeyBackspace:
			// n backspaces delete up to n characters, never below
			// empty.
			if len(buf) > 0 {
				del := int(n)
				if del > len(buf) {
					del = len(buf)
				}
				buf = buf[:len(buf)-del]
				redraw()
			}
		case tui.KeyEnter:
			// One Enter event answers the prompt exactly once, no
			// matter the repeat count: a coalesced held-Enter must not
			// chain into the NEXT screen (that is how a stray Enter
			// could otherwise reach a consent prompt or silently
			// re-fire this menu's default action). The trailing
			// presses live inside the record already dequeued, so they
			// are consumed and dropped here - safe, and recorded as a
			// deliberate, non-chaining improvement over cooked-mode
			// typeahead.
			s := strings.ToLower(strings.TrimSpace(string(buf)))
			if s == "" && cursor >= 0 {
				s = nums[cursor] // Enter on a moved cursor = typing that number
			}
			return s, false
		case tui.KeyEsc:
			// Esc is only ever the screen's documented back action;
			// repeat count ignored for the same non-chaining reason as
			// Enter.
			return strings.ToLower(escChoice), false
		case tui.KeyInterrupt:
			// Ctrl+C at a menu prompt must end the process exactly as
			// before R42. The raw mode is restored FIRST (the raw
			// mode, not the deferred Stop, must not be what the shell
			// sees if the process dies here), then the real console
			// event is re-raised so the runtime's default interrupt
			// behavior takes over; a raise failure is a plain exit.
			// The loop then simply waits for a signal already in
			// flight.
			raw.Stop()
			if !tui.RaiseCtrlC() {
				os.Exit(1)
			}
		case tui.KeyLost:
			// Console EOF (Ctrl+Z) or a dead handle: the same meaning
			// exhausted stdin has always had.
			return "", true
		}
	}
}
