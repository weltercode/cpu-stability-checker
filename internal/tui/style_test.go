package tui

import "testing"

// DisplayWidth counts terminal CELLS: ANSI takes none, combining
// marks ride the previous cell, East Asian wide characters and emoji
// take two, Russian letters take one. Menu columns aligned on byte or
// rune counts drift on exactly these strings, so the count is pinned.
func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"привет", 6},
		{"—·…", 3},
		{"\x1b[31mкрасный\x1b[0m", 7},
		{"e\u0301", 1},                 // combining acute rides the e
		{"漢字", 4},                      // two wide characters
		{"漢字漢", 6},                     //
		{"🙂", 2},                       // emoji
		{"\x1b[1m\x1b[34m→\x1b[0m", 1}, // two SGR wraps, one arrow cell
	}
	for _, c := range cases {
		if got := DisplayWidth(c.s); got != c.want {
			t.Errorf("DisplayWidth(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}

// Truncate cuts plain text to a cell budget and marks the cut with
// one "…" - the user sees the line continues and where to find the
// rest («Подробнее»). It never splits mid-escape because it runs
// before Colorize.
func TestTruncate(t *testing.T) {
	cases := []struct {
		s     string
		width int
		want  string
	}{
		{"abcdef", 6, "abcdef"},     // fits exactly: untouched
		{"abcdef", 7, "abcdef"},     // room to spare
		{"abcdef", 4, "abc…"},       // 3 cells + ellipsis
		{"abcdef", 1, "…"},          // only the ellipsis fits
		{"abcdef", 0, ""},           // nothing fits
		{"проверка", 5, "пров…"},    // Cyrillic counts cells, not bytes
		{"漢字漢", 5, "漢字…"},           // wide cells respected
		{"e\u0301x", 2, "e\u0301x"}, // combining mark rides free
	}
	for _, c := range cases {
		if got := Truncate(c.s, c.width); got != c.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", c.s, c.width, got, c.want)
		}
	}
}

// PadRight aligns on display cells, so a styled entry pads like its
// plain twin - the column does not drift when color is on.
func TestPadRightANSI(t *testing.T) {
	plain := PadRight("abc", 6)
	styled := PadRight("\x1b[31mabc\x1b[0m", 6)
	if DisplayWidth(plain) != 6 || DisplayWidth(styled) != 6 {
		t.Fatalf("pad drift: plain %q styled %q", plain, styled)
	}
	if PadRight("abcdefg", 4) != "abcdefg" {
		t.Error("PadRight must never cut")
	}
}

// Colorize with color off (pipe, file, NO_COLOR, dumb terminal) is
// byte-identical to the text: the words are the authority, color is
// decoration that disappears whole.
func TestColorizePlainParity(t *testing.T) {
	if got := Colorize(false, FGRed, "ошибка"); got != "ошибка" {
		t.Errorf("color off: %q", got)
	}
	if got := Colorize(true, "", "ошибка"); got != "ошибка" {
		t.Errorf("empty code: %q", got)
	}
	if got := Colorize(true, FGRed, ""); got != "" {
		t.Errorf("empty text: %q", got)
	}
	got := Colorize(true, FGRed, "ошибка")
	if got != "\x1b[31mошибка\x1b[0m" {
		t.Errorf("color on: %q", got)
	}
	if DisplayWidth(got) != DisplayWidth("ошибка") {
		t.Error("styling must not change the cell count")
	}
}

// colorAllowed honors the NO_COLOR standard (any non-empty value
// disables) and TERM=dumb, and stays on otherwise. It gates color
// only - the screen UI itself keeps working colorless.
func TestColorAllowed(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if colorAllowed() {
		t.Error("NO_COLOR set must disable color")
	}
	t.Setenv("NO_COLOR", "") // empty value does NOT disable (the standard)
	t.Setenv("TERM", "dumb")
	if colorAllowed() {
		t.Error("TERM=dumb must disable color")
	}
	t.Setenv("TERM", "xterm-256color")
	if !colorAllowed() {
		t.Error("clean environment must allow color")
	}
}
