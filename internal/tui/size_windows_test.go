//go:build windows

package tui

import "testing"

// The one constant that silently disabled the entire screen UI when it
// was wrong: STD_OUTPUT_HANDLE is (DWORD)-11 = 0xFFFFFFF5. A typo here
// makes GetStdHandle fail, and every VT and size probe then honestly
// reports "no screen" — the UI degrades with no error anywhere. Pinned.
func TestStdOutputHandleConstant(t *testing.T) {
	if stdOutputHandle != ^uint32(10) {
		t.Fatalf("stdOutputHandle = %#x, want %#x ((DWORD)-11)",
			stdOutputHandle, ^uint32(10))
	}
}

// pickSize keeps the visible window rect whenever it is a viewport a
// human could actually read, and only then rescues the degenerate
// cases. The 2x2-over-120x30 rows are measured failures: the headless
// console clipped every line to two characters ("lo" and nothing
// else), and Windows Terminal - opened through the default-terminal
// handoff (.lnk) - reported win=2x2 for the WHOLE run while buf held
// a rock-steady 120x30 and maxWindowSize cycled through twenty noise
// values in two minutes. maxWindowSize therefore plays no role: the
// buffer is trusted as-is, because every environment that reaches
// this path was measured with a stable, correct buffer (a conhost
// with tall scrollback has a healthy window rect and never gets here).
func TestPickSize(t *testing.T) {
	cases := []struct {
		name                               string
		winCols, winRows, bufCols, bufRows int
		wantCols, wantRows                 int
		wantOK                             bool
	}{
		{"normal window wins over a tall scrollback buffer", 100, 30, 100, 900, 100, 30, true},
		{"normal small window stays (20x5 real window)", 20, 5, 20, 600, 20, 5, true},
		{"degenerate 2x2 window falls back to the buffer", 2, 2, 120, 30, 120, 30, true},
		{"the buffer is trusted without max-window clamping", 2, 2, 120, 900, 120, 900, true},
		{"zero window with a sane buffer rescues", 0, 0, 80, 25, 80, 25, true},
		{"everything degenerate stays unknown (caller keeps 80x25)", 2, 2, 4, 3, 0, 0, false},
		{"narrow window but sane buffer width (pty artifact)", 4, 40, 120, 40, 120, 40, true},
		{"negative window rect with sane buffer rescues", -1, -1, 100, 30, 100, 30, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cols, rows, ok := pickSize(c.winCols, c.winRows, c.bufCols, c.bufRows)
			if ok != c.wantOK || cols != c.wantCols || rows != c.wantRows {
				t.Errorf("pickSize(%d,%d | %d,%d) = (%d,%d,%v), want (%d,%d,%v)",
					c.winCols, c.winRows, c.bufCols, c.bufRows,
					cols, rows, ok, c.wantCols, c.wantRows, c.wantOK)
			}
		})
	}
}
