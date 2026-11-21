//go:build windows

package tui

import "testing"

// rawRec builds one 20-byte INPUT_RECORD with a KEY_EVENT payload:
// bKeyDown at 4..7, wVirtualKeyCode at 10..11, UnicodeChar at 14..15
// (same layout escKeyDown pins).
func rawRec(eventType uint16, keyDown uint32, vk, uc uint16) []byte {
	b := make([]byte, inputRecordSize)
	b[0] = byte(eventType)
	b[1] = byte(eventType >> 8)
	b[4] = byte(keyDown)
	b[5] = byte(keyDown >> 8)
	b[6] = byte(keyDown >> 16)
	b[7] = byte(keyDown >> 24)
	b[10] = byte(vk)
	b[11] = byte(vk >> 8)
	b[14] = byte(uc)
	b[15] = byte(uc >> 8)
	return b
}

func TestKeyFromEventTable(t *testing.T) {
	down := uint32(1)
	cases := []struct {
		name string
		rec  []byte
		want Key
		r    rune
	}{
		{"down arrow", rawRec(keyEvent, down, vkDown, 0), KeyDown, 0},
		{"up arrow", rawRec(keyEvent, down, vkUp, 0), KeyUp, 0},
		{"left arrow", rawRec(keyEvent, down, vkLeft, 0), KeyLeft, 0},
		{"right arrow", rawRec(keyEvent, down, vkRight, 0), KeyRight, 0},
		{"enter", rawRec(keyEvent, down, vkReturn, 13), KeyEnter, 0},
		{"escape", rawRec(keyEvent, down, vkEscape, 27), KeyEsc, 0},
		{"backspace", rawRec(keyEvent, down, vkBack, 8), KeyBackspace, 0},
		{"digit", rawRec(keyEvent, down, '3', '3'), KeyRune, '3'},
		{"letter", rawRec(keyEvent, down, 'Q', 'q'), KeyRune, 'q'},
		{"cyrillic typed", rawRec(keyEvent, down, 0, 0x44F), KeyRune, 'я'},
		{"ctrl+c as event", rawRec(keyEvent, down, 'C', 3), KeyInterrupt, 0},
		{"ctrl+z is EOF", rawRec(keyEvent, down, 'Z', 0x1A), KeyLost, 0},
		{"f5 no text", rawRec(keyEvent, down, 0x74, 0), KeyOther, 0},
		{"tab ignored", rawRec(keyEvent, down, 0x09, 9), KeyOther, 0},
		// Key-UP must be KeyNone: acting on both halves would move
		// the cursor twice per press.
		{"arrow key-up", rawRec(keyEvent, 0, vkDown, 0), KeyNone, 0},
		{"enter key-up", rawRec(keyEvent, 0, vkReturn, 0), KeyNone, 0},
		{"resize record", rawRec(0x0010, 0, 0, 0), KeyNone, 0},
		{"mouse record", rawRec(0x0002, 0, 0, 0), KeyNone, 0},
		{"short record", rawRec(keyEvent, down, vkDown, 0)[:10], KeyNone, 0},
	}
	for _, c := range cases {
		got, r := keyFromEvent(c.rec)
		if got != c.want || r != c.r {
			t.Errorf("%s: got (%d, %q), want (%d, %q)", c.name, got, r, c.want, c.r)
		}
	}
}

func TestKeyRepeatTable(t *testing.T) {
	// Windows coalesces held or fast-typed same-key presses into ONE
	// record counting them (wRepeatCount at payload offset 4..5, the
	// WORD right after bKeyDown). A decoder that ignores the count
	// turns a typed "11" into "1" and selects the wrong action - the
	// implementation review's must-fix, pinned here.
	rec := rawRec(keyEvent, 1, '1', '1')
	rec[4+4] = 3 // repeat = 3 presses
	if n := keyRepeat(rec); n != 3 {
		t.Fatalf("repeat 3 read as %d", n)
	}
	if n := keyRepeat(rawRec(keyEvent, 1, '1', '1')); n != 1 {
		t.Fatalf("single press must count 1, got %d", n)
	}
	// A console that claims zero still means the one press it sent.
	zero := rawRec(keyEvent, 1, '1', '1')
	zero[4+4], zero[4+5] = 0, 0
	if n := keyRepeat(zero); n != 1 {
		t.Fatalf("repeat 0 must floor to 1, got %d", n)
	}
	if n := keyRepeat(rec[:10]); n != 1 {
		t.Fatalf("short record must floor to 1, got %d", n)
	}
}

func TestRawInputModeTable(t *testing.T) {
	// The navigating menu mode must switch off exactly: processed
	// input (Ctrl+C must arrive as a key event we can answer with a
	// mode restore BEFORE re-raising - a raw-mode leak to the shell
	// is the failure this pins), line input, echo and quick-edit;
	// extended flags must be ON for quick-edit control to apply;
	// mouse, window and every unrelated bit must survive untouched.
	const unrelated = 0xF000
	got := rawInputMode(enableProcessedInput | enableLineInput | enableEchoInput |
		0x0008 | 0x0010 | enableQuickEdit | unrelated)
	want := uint32(0x0008 | 0x0010 | enableExtendedFlags | unrelated)
	if got != want {
		t.Fatalf("rawInputMode = %016b, want %016b", got, want)
	}
	// Already-extended input keeps its other extended-era bits.
	got = rawInputMode(enableExtendedFlags | enableLineInput | 0x0100)
	want = enableExtendedFlags | 0x0100
	if got != want {
		t.Fatalf("rawInputMode(extended) = %016b, want %016b", got, want)
	}
}
