package tui

import (
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SGR color codes for the semantic roles the menu plan (section 7)
// assigns: category accents, errors, unknown/partial states. They
// are only ever applied to text that already says what it means -
// color decorates the words, it never replaces them.
const (
	Reset        = "\x1b[0m"
	FGRed        = "31"
	FGYellow     = "33"
	FGBlue       = "34"
	FGPurple     = "35"
	FGBrightRed  = "91"
	FGBrightBlue = "94"
)

// colorAllowed reads the user's color wishes: the NO_COLOR standard
// (any non-empty value disables color) and TERM=dumb. It gates color
// only - cursor movement and screen clearing stay available, so a
// colorless terminal still gets the full screen UI, just without
// decoration.
func colorAllowed() bool {
	if v, ok := os.LookupEnv("NO_COLOR"); ok && v != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return true
}

// Colorize wraps s in an SGR color when color is on. With color off
// (pipe, file, NO_COLOR, dumb terminal) the output is byte-identical
// to the plain text, which is what keeps scripted tests and
// redirected logs stable.
func Colorize(on bool, code, s string) string {
	if !on || code == "" || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + Reset
}

// DisplayWidth counts the terminal CELLS a string occupies: ANSI
// escape/CSI sequences take none, combining marks ride the cell of
// the character before them, and East Asian wide characters (and
// emoji) take two. Column math on raw byte or rune counts misaligns
// Russian text and styled lines alike - menu columns and tables must
// align on this count.
func DisplayWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			// Skip an escape sequence whole. For CSI (ESC [ params
			// final) the final byte is 0x40..0x7E - but the introducer
			// '[' (0x5B) is in that range too, so it must be consumed
			// before the final-byte scan starts, or the parameters
			// ("31m") would be counted as cells.
			i++
			if i < len(s) && s[i] == '[' {
				i++
				for i < len(s) {
					b := s[i]
					i++
					if b >= 0x40 && b <= 0x7e {
						break
					}
				}
			} else if i < len(s) {
				i++ // simple ESC x form: one more byte
			}
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		if r == 0 {
			continue
		}
		if runeZero(r) {
			continue
		}
		if runeWide(r) {
			w += 2
			continue
		}
		w++
	}
	return w
}

// runeZero reports combining marks and invisible format characters:
// they occupy no cell of their own.
func runeZero(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf)
}

// runeWide reports the characters terminals render two cells wide.
// The ranges cover the practical set (CJK, Hangul, fullwidth forms,
// emoji); Russian, box drawing and punctuation are deliberately
// absent because they are single-cell.
func runeWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo initial consonants
		r >= 0x2E80 && r <= 0x303E, // CJK radicals .. CJK symbols
		r >= 0x3041 && r <= 0x33FF, // Kana .. CJK compatibility
		r >= 0x3400 && r <= 0x4DBF, // CJK extension A
		r >= 0x4E00 && r <= 0x9FFF, // CJK unified
		r >= 0xA000 && r <= 0xA4CF, // Yi
		r >= 0xAC00 && r <= 0xD7A3, // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK compatibility ideographs
		r >= 0xFE10 && r <= 0xFE19,
		r >= 0xFE30 && r <= 0xFE6F,
		r >= 0xFF00 && r <= 0xFF60, // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1F64F, // emoji blocks
		r >= 0x20000 && r <= 0x3FFFD: // CJK extensions B+
		return true
	}
	return false
}

// Truncate cuts PLAIN text to at most width display cells, replacing
// the cut with a single "…" so the user can see the line continues
// (and where to look for the rest). It runs BEFORE Colorize: styling
// must never be sliced mid-escape.
func Truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if DisplayWidth(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := 1
		switch {
		case runeZero(r):
			rw = 0
		case runeWide(r):
			rw = 2
		}
		if w+rw > width-1 { // leave the cell for the ellipsis
			break
		}
		b.WriteRune(r)
		w += rw
	}
	b.WriteRune('…')
	return b.String()
}

// PadRight pads s with spaces to width display cells (ANSI-aware,
// never cutting). This is the alignment primitive for columns that
// mix styled and plain entries.
func PadRight(s string, width int) string {
	pad := width - DisplayWidth(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}
