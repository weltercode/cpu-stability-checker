package dashboard

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/weltercode/cpu-stability-checker/internal/temperature"
	"github.com/weltercode/cpu-stability-checker/internal/tui"
)

func TestReportDoesNotMoveOrOverflowHardwarePanel(t *testing.T) {
	for _, size := range [][2]int{{122, 32}, {122, 30}, {100, 40}, {80, 25}, {60, 25}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s := Snapshot{Busy: make([]float64, 32), CoreOf: make([]int, 32), HasRun: true, Label: "running", Color: true,
				Temperature: temperature.Status{State: "off", Known: true, Reading: temperature.Reading{Source: "HWiNFO", Sensor: "CPU (Tctl/Tdie)", Celsius: 85}}}
			for cpu := range s.CoreOf {
				s.CoreOf[cpu] = cpu / 2
			}
			before := renderConsole(s, "Starting session\nRun 1/1\n", "", size[0], size[1])
			s.HasRun = false
			report := strings.Repeat("old run result\n", 300) + "Results:\n  Status: OK\n  Test: general\n  Total: 0 iterations across 16 CPU slots\n  y-cruncher slots=16 total=0 avg=0 range=0..0\n\nSession completed: 1/1 runs passed.\n"
			prompt := "Press Enter to return to the menu..."
			after := renderConsole(s, report, prompt, size[0], size[1])
			if len(after) >= size[1] || len(before) >= size[1] {
				t.Fatalf("frame overflows %d rows: before=%d after=%d", size[1], len(before), len(after))
			}
			if after[len(after)-1] != prompt {
				t.Fatal("Enter prompt lost below report")
			}
			for _, lines := range [][]string{before, after} {
				for _, line := range lines {
					if len([]rune(stripANSI(line))) > size[0] {
						t.Fatalf("line wraps: %q", line)
					}
				}
			}
			labels, _ := coreLabels(s)
			for _, label := range labels {
				label = strings.TrimSpace(label) + " "
				row := func(lines []string) int {
					for i, line := range lines {
						if strings.Contains(stripANSI(line), label) {
							return i
						}
					}
					return -1
				}
				if a, b := row(before), row(after); a < 0 || a != b {
					t.Fatalf("CPU %s disappeared or moved: %d -> %d", label, a, b)
				}
			}
			for _, want := range []string{"Results:", "Status: OK", "Session completed:"} {
				if !strings.Contains(strings.Join(after, "\n"), want) {
					t.Fatalf("summary lost %q", want)
				}
			}
		})
	}
}

func TestTinyViewportIsBoundedAndReportsMissingCPUs(t *testing.T) {
	lines := renderConsole(Snapshot{Busy: make([]float64, 128)}, "report\n", "Enter", 30, 8)
	if len(lines) >= 8 || !strings.Contains(strings.Join(lines, "\n"), "Window too small") {
		t.Fatalf("tiny viewport silently clipped CPUs or overflowed: %q", lines)
	}
}

func TestResultsFollowCPUsWithoutUnusedPanelRows(t *testing.T) {
	for _, rows := range []int{32, 40, 70} {
		s := Snapshot{Busy: make([]float64, 16), CoreOf: make([]int, 16),
			Temperature: temperature.Status{State: "off"}}
		for cpu := range s.CoreOf {
			s.CoreOf[cpu] = cpu / 2
		}
		lines := renderConsole(s, "Test: isolate\nTotal: 0 iterations\nScenario summary:\nexternal aborted\n", "Press Enter", 122, rows)
		lastCPU, result := -1, -1
		for i, line := range lines {
			if strings.Contains(line, "c7*") {
				lastCPU = i
			}
			if line == "Test: isolate" {
				result = i
			}
		}
		if lastCPU < 0 || result != lastCPU+2 {
			t.Fatalf("%d rows: results must follow CPUs with one blank line, CPU=%d result=%d\n%s", rows, lastCPU, result, strings.Join(lines, "\n"))
		}
		if lines[len(lines)-1] != "Press Enter" || len(lines) >= rows {
			t.Fatalf("%d rows: prompt lost or frame overflows: %q", rows, lines)
		}
	}
}

func TestClippedColorDoesNotLeakToNextLine(t *testing.T) {
	lines := clipAll([]string{"\x1b[91m" + strings.Repeat("x", 50) + "\x1b[0m", "plain"}, 20)
	if !strings.HasSuffix(lines[0], "\x1b[0m") || lines[1] != "plain" {
		t.Fatalf("clipping leaked color: %q", lines)
	}
}

// Replay the actual Console output through a finite terminal, including
// fragmented summary writes and several cooldown frames. A string-only
// test would miss scrolling, cursor drift and stale rows left on screen.
func TestConsoleReportAndCooldownStayInsideTerminal(t *testing.T) {
	var output bytes.Buffer
	s := Snapshot{Busy: make([]float64, 32), HasRun: true, Label: "running", Color: true,
		Temperature: temperature.Status{State: "off"}}
	c := &Console{out: &output, file: os.Stdout, painter: tui.NewPainter(&output), vt: true,
		lastCols: 122, lastRows: 32, lastSnap: s}
	screen := finiteTerminal{cols: 122, rows: 32, cells: make([][]rune, 32)}
	check := func() {
		t.Helper()
		screen.consume(t, output.String())
		output.Reset()
		want := renderConsole(c.lastSnap, c.messages, c.prompt, 122, 32)
		for row := 0; row < screen.rows; row++ {
			expected := ""
			if row < len(want) {
				expected = stripANSI(want[row])
			}
			if got := strings.TrimRight(string(screen.cells[row]), " "); got != strings.TrimRight(expected, " ") {
				t.Fatalf("row %d drifted: got %q, want %q", row, got, expected)
			}
		}
	}
	c.Print("Run 1/1 | " + strings.Repeat("long CPU list ", 40) + "\n")
	check()
	for _, fragment := range []string{"\nResults:\n", "  Status: ", "OK\n", strings.Repeat("old result\n", 200), "\nSession completed: 1/1 runs passed.\n"} {
		c.Print(fragment)
		check()
	}
	c.ClearRun()
	c.SetPrompt("Press Enter to return to the menu...")
	check()
	for _, busy := range []float64{50, 15, 3} {
		c.lastSnap.Busy[0] = busy
		c.drawLocked(c.lastSnap)
		check()
	}
}

type finiteTerminal struct {
	cols, rows, x, y int
	cells            [][]rune
}

func (v *finiteTerminal) consume(t *testing.T, text string) {
	t.Helper()
	for len(text) > 0 {
		if strings.HasPrefix(text, "\x1b[") {
			end := 2
			for end < len(text) && (text[end] < '@' || text[end] > '~') {
				end++
			}
			if end == len(text) {
				t.Fatal("incomplete terminal escape")
			}
			arg := text[2:end]
			switch text[end] {
			case 'H':
				v.x, v.y = 0, 0
			case 'm': // foreground color changes do not move the cursor
			case 'A':
				n, _ := strconv.Atoi(arg)
				v.y = max(0, v.y-n)
			case 'J':
				start := v.y
				if arg == "2" {
					start = 0
				} else if v.x < len(v.cells[v.y]) {
					v.cells[v.y] = v.cells[v.y][:v.x]
					start++
				} else {
					start++
				}
				for row := start; row < v.rows; row++ {
					v.cells[row] = nil
				}
			default:
				t.Fatalf("unsupported terminal command: %q", text[:end+1])
			}
			text = text[end+1:]
			continue
		}
		r, size := utf8.DecodeRuneInString(text)
		text = text[size:]
		switch r {
		case '\r':
			v.x = 0
		case '\n':
			v.y++
			if v.y >= v.rows {
				t.Fatal("output scrolled past terminal bottom")
			}
		default:
			if v.x >= v.cols {
				t.Fatal("output wrapped past terminal width")
			}
			for len(v.cells[v.y]) <= v.x {
				v.cells[v.y] = append(v.cells[v.y], ' ')
			}
			v.cells[v.y][v.x] = r
			v.x++
		}
	}
}
