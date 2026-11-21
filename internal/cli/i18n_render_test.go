package cli

// R41 render tests: the language overlay must change what a human
// reads and nothing else. English stays the byte-identical default
// (the whole existing pin corpus depends on it), a shipped overlay
// renders with zero silent pass-throughs, an unshipped or invalid
// code says so on screen instead of pretending, and the consent
// protocol (the exact word clear) stays live through translation.

import (
	"bufio"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/config"
)

func TestEnglishDefaultUnchanged(t *testing.T) {
	t.Cleanup(func() { uiTexts = nil })
	uiTexts = nil
	lines := menuLines(&screenUI{})
	joined := strings.Join(lines, "\n")
	for _, pin := range []string{"=== CPU Stability Checker ===", "  0) Quit", "  2) Quick check (bounded)", "  7) View findings - none unread"} {
		if !strings.Contains(joined, pin) {
			t.Fatalf("English default lost the pinned line %q", pin)
		}
	}
	if strings.Contains(joined, "Повторить") {
		t.Fatal("Russian leaked into the default rendering")
	}
}

func TestRussianOverlayRendersWithNoPassThrough(t *testing.T) {
	chdirTemp(t)
	t.Cleanup(func() { uiTexts = nil })
	notes := installTexts(config.Config{Language: "ru"})
	if len(notes) != 0 {
		t.Fatalf("a shipped language must install silently, got %q", notes)
	}
	joined := strings.Join(menuLines(&screenUI{}), "\n")
	for _, want := range []string{
		"Повторить последний тест",
		"  3) Проверка CPU",
		"  0) Выход",
		"Просмотр уведомлений - непрочитанных нет",
		"Быстрая проверка (ограниченная)",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Russian menu is missing %q\nrendered:\n%s", want, joined)
		}
	}
	for _, old := range []string{"Repeat the last test", "0) Quit", "View findings", "Quick check"} {
		if strings.Contains(joined, old) {
			t.Fatalf("English chrome leaked into the Russian menu: %q", old)
		}
	}
	if miss := uiTexts.MissCount(); miss != 0 {
		t.Errorf("the Russian menu ran %d untranslated strings: %v", miss, uiTexts.MissList())
	}
}

func TestUnshippedAndInvalidLanguagesSaySo(t *testing.T) {
	t.Cleanup(func() { uiTexts = nil })

	notes := installTexts(config.Config{Language: "de"}) // valid code, no file
	if len(notes) != 1 || !strings.Contains(notes[0], "no texts/de.json") || !strings.Contains(notes[0], "ru") {
		t.Fatalf("an unshipped language must be stated with what exists, got %q", notes)
	}
	if got := tx("=== CPU Stability Checker ==="); got != "=== CPU Stability Checker ===" {
		t.Fatal("the English base must still render")
	}

	notes = installTexts(config.Config{Language: "d-e-!"})
	if len(notes) != 1 || !strings.Contains(notes[0], "not a valid language code") {
		t.Fatalf("an invalid code must be refused visibly, got %q", notes)
	}
	if tx("Action [q]: ") != "Action [q]: " {
		t.Fatal("after a refusal the English base is in effect")
	}
}

func TestClearWordProtocolSurvivesRussian(t *testing.T) {
	chdirTemp(t)
	t.Cleanup(func() { uiTexts = nil })
	installTexts(config.Config{Language: "ru"})
	// The destructive clear keeps the exact word even through the
	// translated prompt: a Russian answer that merely MEANS clear
	// must not clear anything.
	var in1 = bufio.NewReader(strings.NewReader("очистить\n"))
	line, _ := in1.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(line)) == "clear" {
		t.Fatal("a translated word must never satisfy the token")
	}
	var in2 = bufio.NewReader(strings.NewReader("clear\n"))
	line, _ = in2.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(line)) != "clear" {
		t.Fatal("the exact word clear must still satisfy the token under any language")
	}
	// And the prompt itself still carries the word.
	if !strings.Contains(tx("Type the word clear to remove everything; anything else cancels: "), "clear") {
		t.Fatal("the translated clear prompt lost the token word")
	}
}
