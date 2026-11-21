package cli

// R42 pins. The contract these tests guard: arrows move a visible
// cursor and Enter turns the cursor into EXACTLY the number the
// operator could have typed; typed characters beat arrows; Esc is
// only ever the screen's documented back action; the quit row and
// every letter token (a/j/c/r/d/q) stay unreachable by cursor; the
// reader never pulls events past its answer (what comes after stays
// typeahead for the next prompt, exactly as cooked mode delivers it
// today); the input mode is restored exactly once on every route;
// and with no screen or no console input handle the old line read is
// byte-identical.

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/tui"
)

func TestNavItemsSelectsOnlyNumbers(t *testing.T) {
	lines := []string{
		"=== CPU Stability Checker ===",
		"",
		"› 1) Repeat the last test",
		"     some.recipe · 16 cores",
		"  2) Quick check (bounded) — ~21 s planned load + setup",
		"     a clean word here is NOT a stability verdict",
		"  3) CPU check",
		"  0) Quit",
		"  q) Back",
		"  a) mark all as read (3 unread)   q) back",
		"  c) Clear ALL history: every record plus its log files (asks for the exact word)",
		"  showing 5 of 20",
	}
	rows, nums := navItems(lines)
	if strings.Join(nums, ",") != "1,2,3" {
		t.Fatalf("cursor must see only the numbered actions, got %v (rows %v)", nums, rows)
	}
	for _, r := range rows {
		if strings.HasPrefix(lines[r], "  0)") || strings.Contains(lines[r], "Clear ALL") {
			t.Fatalf("cursor row %d is a quit/destructive line", r)
		}
	}
}

func TestNavMarkSingleMarker(t *testing.T) {
	lines := []string{"h", "› 1) A", "  2) B", "  3) C"}
	rows, _ := navItems(lines)

	got := navMark(lines, rows, 1)
	if got[1] != "  1) A" || got[2] != "› 2) B" {
		t.Fatalf("cursor must own exactly one marker, got %q / %q", got[1], got[2])
	}
	if got[0] != "h" || got[3] != "  3) C" {
		t.Fatal("navMark must not touch other rows")
	}
	// No live cursor: the static screen (with its Enter-shortcut
	// marker) is untouched.
	same := navMark(lines, rows, -1)
	if same[0] != "h" || same[1] != "› 1) A" {
		t.Fatal("cursorless redraw must keep the static marker")
	}
}

func TestNavChoiceLegacyPathByteIdentical(t *testing.T) {
	// No screen (append mode): the buffered line contract, exactly.
	ui := &screenUI{}
	in := bufio.NewReader(strings.NewReader("3\n"))
	if got, eof := navChoice(in, ui, []string{"  1) A"}, "Action [1]: ", "0"); got != "3" || eof {
		t.Fatalf("typed number through the legacy path: %q eof=%v", got, eof)
	}
	in = bufio.NewReader(strings.NewReader(""))
	if got, eof := navChoice(in, ui, []string{"  1) A"}, "Action [1]: ", "0"); got != "" || !eof {
		t.Fatalf("exhausted input must still mean quit: %q eof=%v", got, eof)
	}
	in = bufio.NewReader(strings.NewReader("5"))
	if got, eof := navChoice(in, ui, []string{"  1) A"}, "Action [1]: ", "0"); got != "5" || eof {
		t.Fatalf("content without a final newline still dispatches (as before): %q eof=%v", got, eof)
	}
	// Screen present but no console input handle (startRawKeys nil):
	// the same line read, not a dead wait.
	startRawKeys = func() navInput { return nil }
	t.Cleanup(func() { startRawKeys = defaultStartRawKeys })
	sui := &screenUI{p: tui.NewPainter(&bytes.Buffer{})}
	in = bufio.NewReader(strings.NewReader("7\n"))
	if got, eof := navChoice(in, sui, []string{"  1) A"}, "Action [q]: ", "q"); got != "7" || eof {
		t.Fatalf("no-console screen mode must keep the buffered path: %q eof=%v", got, eof)
	}
}

// defaultStartRawKeys keeps the production binding for cleanup.
var defaultStartRawKeys = startRawKeys

// fakeKeys replays a scripted key list. The call counter is the
// discriminator for the one-reader/never-read-past-the-answer rule:
// the fake holds keys BEYOND the answer (like a queued 2 and Enter
// aimed at the next screen) and the test asserts navChoice never
// pulled them - they stay queued for the next owner, exactly the
// cooked-mode typeahead semantics the scripted consent flows rely on.
type fakeKeys struct {
	keys   []tui.Key
	runes  map[int]rune
	rep    map[int]uint32 // coalesced repeat counts per script position
	i      int
	pulls  *int
	stops  *int
	lostAt int // return KeyLost from pull #lostAt onward (-1 = never)
}

func (f *fakeKeys) NextKey() (tui.Key, rune, uint32) {
	*f.pulls++
	if f.lostAt >= 0 && *f.pulls >= f.lostAt {
		return tui.KeyLost, 0, 1
	}
	if f.i >= len(f.keys) {
		return tui.KeyLost, 0, 1 // script exhausted: a lost handle, the test fails on it
	}
	k := f.keys[f.i]
	var r rune
	if f.runes != nil {
		r = f.runes[f.i]
	}
	n := uint32(1)
	if f.rep != nil {
		if v, ok := f.rep[f.i]; ok && v > 0 {
			n = v
		}
	}
	f.i++
	return k, r, n
}

func (f *fakeKeys) Stop() { *f.stops++ }

func runNav(t *testing.T, lines []string, prompt, esc string, f *fakeKeys) (string, bool) {
	t.Helper()
	startRawKeys = func() navInput { return f }
	t.Cleanup(func() { startRawKeys = defaultStartRawKeys })
	ui := &screenUI{p: tui.NewPainter(&bytes.Buffer{})}
	in := bufio.NewReader(strings.NewReader(""))
	return navChoice(in, ui, lines, prompt, esc)
}

func navScript(keys []tui.Key, runes map[int]rune) *fakeKeys {
	return &fakeKeys{keys: keys, runes: runes, pulls: new(int), stops: new(int), lostAt: -1}
}

func TestNavCursorSelectsExactlyTypedNumbers(t *testing.T) {
	cases := []struct {
		name  string
		keys  []tui.Key
		runes map[int]rune
		rep   map[int]uint32
		want  string
	}{
		{"Down Enter selects item one", []tui.Key{tui.KeyDown, tui.KeyEnter}, nil, nil, "1"},
		{"two Downs select item two", []tui.Key{tui.KeyDown, tui.KeyDown, tui.KeyEnter}, nil, nil, "2"},
		{"Up wraps to the last item", []tui.Key{tui.KeyUp, tui.KeyEnter}, nil, nil, "3"},
		{"bare Enter keeps the default semantics", []tui.Key{tui.KeyEnter}, nil, nil, ""},
		{"typed digit beats the cursor", []tui.Key{tui.KeyDown, tui.KeyRune, tui.KeyEnter}, map[int]rune{1: '5'}, nil, "5"},
		{"Backspace edits the buffer", []tui.Key{tui.KeyRune, tui.KeyRune, tui.KeyBackspace, tui.KeyRune, tui.KeyEnter}, map[int]rune{0: '5', 1: 'x', 3: '3'}, nil, "53"},
		{"Esc is the documented back action", []tui.Key{tui.KeyEsc}, nil, nil, "q"},
		{"Left and Right move like Up and Down", []tui.Key{tui.KeyRight, tui.KeyRight, tui.KeyEnter}, nil, nil, "2"},
		// The console coalesces held/fast keys into one record with a
		// repeat count; the review caught both halves of this pin.
		{"coalesced typed pair is the typed number", []tui.Key{tui.KeyRune, tui.KeyEnter}, map[int]rune{0: '1'}, map[int]uint32{0: 2}, "11"},
		{"coalesced held arrow moves twice", []tui.Key{tui.KeyDown, tui.KeyEnter}, nil, map[int]uint32{0: 2}, "2"},
		{"coalesced held Enter answers once", []tui.Key{tui.KeyDown, tui.KeyEnter}, nil, map[int]uint32{1: 5}, "1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Trailing keys beyond the answer: queued input aimed at
			// LATER screens (a consent "y\n" would follow in real
			// life). They must never be pulled by this menu.
			all := append(append([]tui.Key{}, c.keys...), tui.KeyRune, tui.KeyEnter)
			f := navScript(all, c.runes)
			f.rep = c.rep
			got, eof := runNav(t, []string{"head", "  1) A", "  2) B", "  3) C", "  0) Quit", "  q) Back"},
				"Action [q]: ", "q", f)
			if got != c.want || eof {
				t.Fatalf("nav script: got (%q, eof=%v), want (%q, eof=false)", got, eof, c.want)
			}
			if *f.pulls != len(c.keys) {
				t.Fatalf("reader pulled %d keys, must stop at the answer (%d) and leave the queue to the next owner",
					*f.pulls, len(c.keys))
			}
			if *f.stops != 1 {
				t.Fatalf("mode restored %d times, must be exactly once on every route", *f.stops)
			}
		})
	}
}

func TestNavLostHandleMeansQuit(t *testing.T) {
	f := navScript([]tui.Key{tui.KeyDown, tui.KeyDown}, nil)
	f.lostAt = 3 // two real Downs, then the third pull reports the dead handle
	got, eof := runNav(t, []string{"head", "  1) A", "  2) B"}, "Action [1]: ", "0", f)
	if got != "" || !eof {
		t.Fatalf("dead input handle must mean exactly what exhausted stdin meant: got (%q, eof=%v)", got, eof)
	}
	if *f.stops != 1 {
		t.Fatalf("mode must be restored once even on the lost-handle route, got %d", *f.stops)
	}
}

func TestNavDeepScriptNoDeadlock(t *testing.T) {
	// The old channel design deadlocked when events outran the
	// answer (the implementation review's P1). The pull design has no
	// queue to overflow: a 5 000-key script plus the answering Enter
	// must still return exactly the typed choice.
	keys := make([]tui.Key, 0, 5002)
	for i := 0; i < 5000; i++ {
		keys = append(keys, tui.KeyDown)
	}
	keys = append(keys, tui.KeyRune, tui.KeyEnter)
	f := navScript(keys, map[int]rune{5000: '7'})
	got, eof := runNav(t, []string{"head", "  1) A", "  2) B"}, "Action [q]: ", "q", f)
	if got != "7" || eof {
		t.Fatalf("deep script: got (%q, eof=%v), want (\"7\", eof=false)", got, eof)
	}
}
