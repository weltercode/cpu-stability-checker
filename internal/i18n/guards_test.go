package i18n

// R41 guards. These are the honesty rails of the language system:
// a translation file may add wording, it may never lose protocol.
// Every shipped language file is checked against the English base:
// format verbs equal, the five result words equal (whole-word,
// same count), typed tokens (q, a, r, d, j, c, y/N, digits) kept,
// the exact word "clear" kept wherever the English asks for it, no
// development-only identifiers (P07-style) anywhere, full coverage
// over the wrapped chrome, no stale keys, and duplicate or empty
// entries refused at decode. A failing guard is a defect in the
// language file, never a reason to weaken the check.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
)

// chromeStrings returns the two sets the coverage rules are built
// from: every tx("...") literal in the interactive CLI sources, and
// every built-in catalog display line (Details paragraphs arrive to
// the card screen one line at a time, so the overlay keys are lines).
func chromeStrings(t *testing.T) (literals map[string]bool, displayLines map[string]bool) {
	t.Helper()
	literals = map[string]bool{}
	displayLines = map[string]bool{}
	files, err := filepath.Glob(filepath.Join("..", "cli", "*.go"))
	if err != nil {
		t.Fatalf("glob cli sources: %v", err)
	}
	var scanned int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		scanned++
		for _, m := range TxCallRE.FindAllStringSubmatch(string(b), -1) {
			s, err := Unquote(m[1])
			if err != nil {
				t.Fatalf("%s: tx literal %q: %v", f, m[1], err)
			}
			if strings.TrimSpace(s) != "" {
				literals[s] = true
			}
		}
	}
	if scanned < 10 {
		t.Fatalf("only %d cli sources scanned - the scan itself is broken", scanned)
	}
	for _, v := range catalog.DisplayStrings() {
		for _, line := range strings.Split(v, "\n") {
			if strings.TrimSpace(line) != "" {
				displayLines[line] = true
			}
		}
	}
	return literals, displayLines
}

func TestNoDuplicateOrEmptyEntries(t *testing.T) {
	// decodeUnique refuses a shadowed key and an empty rendering.
	m := map[string]string{}
	if err := decodeUnique("tst", []byte(`{"a":"x","a":"y"}`), m); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate key must be refused, got %v", err)
	}
	m = map[string]string{}
	if err := decodeUnique("tst", []byte(`{"a":"  "}`), m); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty text must be refused, got %v", err)
	}
	m = map[string]string{}
	if err := decodeUnique("tst", []byte(`{"a":"x","b":"y"}`), m); err != nil || len(m) != 2 {
		t.Fatalf("a clean file must decode: %v", err)
	}
}

func TestLoadRefusesInvalidAndPassesUnknown(t *testing.T) {
	if _, err := Load("RU"); err == nil {
		t.Fatal(`"RU" must be refused (bounds protect the file lookup)`)
	}
	if _, err := Load("ru;rm"); err == nil {
		t.Fatal("a code that could steer a path must be refused")
	}
	c, err := Load("xx") // valid shape, no file shipped
	if err != nil {
		t.Fatalf("an unshipped valid code is not an error: %v", err)
	}
	if c.HasFile() {
		t.Fatal("HasFile must be false for an unshipped code")
	}
	if got := c.T("Plain English"); got != "Plain English" {
		t.Fatalf("pass-through must render the English base, got %q", got)
	}
	if c.MissCount() != 1 {
		t.Fatalf("the pass-through must be counted, got %d", c.MissCount())
	}
	c.T("Plain English")
	if c.MissCount() != 1 {
		t.Fatal("the miss set is per distinct string")
	}
	// "en" is the base: no file, no misses, byte identity.
	e, err := Load("")
	if err != nil || e.Lang() != "en" || e.MissCount() != 0 {
		t.Fatalf("empty language is the English base: %v", err)
	}
	if e.T("x y") != "x y" {
		t.Fatal("en must pass through byte-identical")
	}
}

var tokenRE = regexp.MustCompile(`\b(q|a|r|d|j|c)\)|\[y/N\]|\[\d\]|\[\w+\]`)

func TestShippedFilesKeepProtocol(t *testing.T) {
	langs := ShippedLanguages()
	if len(langs) == 0 {
		t.Fatal("expected at least one overlay file")
	}
	for _, lang := range langs {
		m, err := FileText(lang)
		if err != nil {
			t.Fatalf("texts/%s.json: %v", lang, err)
		}
		for key, val := range m {
			// Format verbs: same sequence, same order (a lost %d
			// would panic the render or swallow a number).
			kv := VerbRE.FindAllString(key, -1)
			vv := VerbRE.FindAllString(val, -1)
			if strings.Join(kv, "|") != strings.Join(vv, "|") {
				t.Errorf("%s: verbs changed for %q: %v -> %v", lang, key, kv, vv)
			}
			// The five result words: whole-word, same count.
			for _, w := range ProtocolWords {
				if kc, vc := CountWord(key, w), CountWord(val, w); kc != vc {
					t.Errorf("%s: protocol word %s count %d -> %d in %q", lang, w, kc, vc, key)
				}
			}
			// Typed tokens that ARE the input protocol.
			for _, tok := range tokenRE.FindAllString(key, -1) {
				if !strings.Contains(val, tok) {
					t.Errorf("%s: typed token %s lost in %q", lang, tok, key)
				}
			}
			// The exact word that confirms the destructive clear.
			if CountWord(key, "clear") > 0 && CountWord(val, "clear") == 0 {
				t.Errorf("%s: the exact word clear vanished from %q", lang, key)
			}
			// Development-only identifiers never surface, in any
			// language (R40 rule extended over translations).
			if mm := catalog.InternalIDPattern.FindString(val); mm != "" {
				t.Errorf("%s: internal id %s in value of %q", lang, mm, key)
			}
		}
	}
}

func TestShippedFilesCoverEveryWrappedString(t *testing.T) {
	literals, displayLines := chromeStrings(t)
	for _, lang := range ShippedLanguages() {
		m, err := FileText(lang)
		if err != nil {
			t.Fatalf("texts/%s.json: %v", lang, err)
		}
		var missing []string
		for s := range literals {
			if _, ok := m[s]; !ok {
				missing = append(missing, s)
			}
		}
		for s := range displayLines {
			if _, ok := m[s]; !ok {
				missing = append(missing, s)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			if len(missing) > 8 {
				missing = missing[:8]
			}
			t.Errorf("%s: %d wrapped UI strings have no entry (first: %q)", lang, len(missing), strings.Join(missing, " | "))
		}
	}
}

func TestNoStaleKeys(t *testing.T) {
	literals, displayLines := chromeStrings(t)
	for _, lang := range ShippedLanguages() {
		m, err := FileText(lang)
		if err != nil {
			t.Fatalf("texts/%s.json: %v", lang, err)
		}
		for key := range m {
			if literals[key] || displayLines[key] {
				continue
			}
			// A key the Go chrome no longer contains: either the
			// English source changed under the file or the entry was
			// invented. Both are lies; say which one cannot be known.
			t.Errorf("%s: key %q is in no tx() literal and in no catalog display line (stale or invented)", lang, key)
		}
	}
}

func TestNilCatalogIsEnglish(t *testing.T) {
	var c *Catalog
	if c.T("hello") != "hello" || c.Lang() != "en" || c.MissCount() != 0 {
		t.Fatal("a nil catalog must be the silent English base")
	}
}
