package cli

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/tui"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// wizard drives one interactive flow as a screen widget: every step
// repaints the whole screen — completed answers stay as plain lines at
// the top, the current option list replaces the previous one, the prompt
// waits at the bottom. Without a VT screen every method delegates to the
// legacy append-mode prompts, byte for byte, so scripted input, pipes
// and today's tests behave exactly as before.
type wizard struct {
	in    *bufio.Reader
	ui    *screenUI
	title string
	done  []string // completed "Label: answer" lines
	hints []string // context lines for the current step
	note  string   // validation error, shown once on the next frame
}

func newWizard(in *bufio.Reader, ui *screenUI, title string) *wizard {
	return &wizard{in: in, ui: ui, title: title}
}

func (w *wizard) record(label, answer string) {
	w.done = append(w.done, fmt.Sprintf("%-18s %s", label+":", answer))
}

// draw repaints the whole screen for one prompt.
func (w *wizard) draw(prompt string, items []string) {
	lines := append([]string{"=== " + w.title + " ==="}, w.done...)
	lines = append(lines, w.hints...)
	lines = append(lines, items...)
	if w.note != "" {
		// A validation failure is an error: red plus the same words
		// (menu plan section 7 - color decorates, the text is the
		// authority; NO_COLOR keeps the note byte-identical).
		lines = append(lines, "", w.ui.paint(tui.FGBrightRed, w.note))
		w.note = ""
	}
	w.ui.screenPrompt(lines, prompt)
}

// choose mirrors the free choose(), recording the picked item.
func (w *wizard) choose(label, prompt string, items []string, def int) int {
	if !w.ui.hasScreen() {
		fmt.Println("\n" + label)
		return choose(w.in, prompt, items, def)
	}
	numbered := make([]string, len(items))
	for i, item := range items {
		numbered[i] = fmt.Sprintf("%d) %s", i+1, item)
	}
	for {
		w.draw(fmt.Sprintf("%s [%d] (q=back): ", prompt, def+1), numbered)
		line, _ := w.in.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			w.record(label, shortItem(items[def]))
			return def
		}
		if strings.EqualFold(line, "q") || line == "0" {
			return -1
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(items) {
			w.record(label, shortItem(items[n-1]))
			return n - 1
		}
		w.note = fmt.Sprintf("Enter a number from 1 to %d, or q to go back.", len(items))
	}
}

// chooseWorkload mirrors the free chooseWorkload().
func (w *wizard) chooseWorkload(defaultName string) (string, bool) {
	names := workload.Names()
	items := make([]string, len(names))
	def := 0
	for i, name := range names {
		work, _ := workload.Get(name)
		items[i] = fmt.Sprintf("%-7s %s", name, work.Description)
		if name == defaultName {
			def = i
		}
	}
	selected := w.choose("Load algorithm", "Choose an algorithm", items, def)
	if selected < 0 {
		return "", false
	}
	return names[selected], true
}

// promptLine shows the screen with a text prompt and returns the raw
// answer (default applied when empty). Recording is the caller's job —
// usually after validation.
func (w *wizard) promptLine(prompt, def string) string {
	if !w.ui.hasScreen() {
		return ask(w.in, prompt, def)
	}
	w.draw(askPrompt(prompt, def), nil)
	line, _ := w.in.ReadString('\n')
	value := strings.TrimSpace(line)
	if value == "" {
		value = def
	}
	return value
}

// askDuration mirrors the free askDuration(), recording the answer. An
// empty label means the caller had no section header in append mode.
func (w *wizard) askDuration(label, prompt, def string) time.Duration {
	if !w.ui.hasScreen() {
		if label != "" {
			fmt.Println("\n" + label)
		}
		return askDuration(w.in, prompt, def)
	}
	for {
		w.draw(askPrompt(prompt+" (examples: 30s, 5m, 1h)", def), nil)
		line, _ := w.in.ReadString('\n')
		value := strings.TrimSpace(line)
		if strings.EqualFold(value, "q") {
			return 0
		}
		if value == "" {
			value = def
		}
		duration, err := time.ParseDuration(value)
		if err == nil && duration > 0 {
			w.record(label, value)
			return duration
		}
		w.note = "Enter a positive duration such as 30s, 5m, or 1h; q goes back."
	}
}

// askPositiveInt mirrors the free askPositiveInt(), recording the answer.
// An empty label means the caller had no section header in append mode.
func (w *wizard) askPositiveInt(label, prompt string, def int) int {
	if !w.ui.hasScreen() {
		if label != "" {
			fmt.Println("\n" + label)
		}
		return askPositiveInt(w.in, prompt, def)
	}
	for {
		w.draw(askPrompt(prompt, strconv.Itoa(def)), nil)
		line, _ := w.in.ReadString('\n')
		value := strings.TrimSpace(line)
		if strings.EqualFold(value, "q") {
			return 0
		}
		if value == "" {
			value = strconv.Itoa(def)
		}
		n, err := strconv.Atoi(value)
		if err == nil && n > 0 {
			w.record(label, value)
			return n
		}
		w.note = "Enter a positive whole number; q goes back."
	}
}

// confirm mirrors confirmRun on the wizard screen.
func (w *wizard) confirm() bool {
	if !w.ui.hasScreen() {
		return confirmRun(w.in, os.Stdout)
	}
	w.draw("Start this run? [y/N]: ", nil)
	line, _ := w.in.ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// showPlan replaces the screen with a finished plan the user reads
// before confirming.
func (w *wizard) showPlan(lines []string) {
	if !w.ui.hasScreen() {
		for _, l := range lines {
			fmt.Println(l)
		}
		return
	}
	w.ui.p.Screen(lines)
}

func askPrompt(prompt, def string) string {
	if def != "" {
		return fmt.Sprintf("%s [%s]: ", prompt, def)
	}
	return prompt + ": "
}

// shortItem trims an option's explanation for the answer line:
// "All-core — load all selected..." becomes "All-core".
func shortItem(item string) string {
	if idx := strings.Index(item, " — "); idx > 0 {
		return item[:idx]
	}
	return item
}
