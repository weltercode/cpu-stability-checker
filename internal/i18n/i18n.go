// Package i18n resolves user-facing UI wording for the interactive
// menus (R41, owner-named 20.11: user-facing texts as a data file,
// translations added by creating their own JSON files, the language
// selected in the config).
//
// The model is deliberately small and fail-toward-English:
//
//   - The English text in the Go source IS the base language and the
//     lookup key. There is no separate key registry that could drift.
//   - A language file is an overlay: exact English source string ->
//     translated string, shipped as internal/i18n/texts/<lang>.json
//     and embedded at build time. Adding a language = adding such a
//     file (plus rebuilding the binary); nothing in the code needs a
//     new branch.
//   - A missing translation passes through as the English base. The
//     UI NEVER renders a raw key name and never renders a silent
//     empty: an untranslated string is still a real, readable string.
//     What IS counted honestly is how many strings passed through
//     (MissCount/MissList), so the menu can state that the selected
//     language is incomplete instead of pretending.
//   - Machine-facing wording (results, reports, JSON contracts,
//     consent tokens like the exact word clear) is not translated by
//     this mechanism: on-disk records stay in one language. The
//     guards below pin that translated chrome never loses the five
//     result words, the typed token letters, the format verbs, or the
//     exact word that confirms a destructive action.
package i18n

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed texts/*.json
var textsFS embed.FS

// Catalog is one loaded language overlay over the English base.
type Catalog struct {
	lang string
	file bool // a texts/<lang>.json was actually found
	m    map[string]string
	mu   sync.Mutex
	miss map[string]bool // counted pass-through keys (set, order-free)
}

// langPattern bounds language codes so a config value can never steer
// the loader toward arbitrary paths.
var langPattern = regexp.MustCompile(`^[a-z]{2}(?:-[A-Za-z0-9]{2,8})*$`)

// Load builds the overlay for lang. An empty or "en" language needs no
// file (the base IS English). A syntactically valid but unshipped code
// (no texts/<lang>.json) is NOT an error: the caller must say so on
// screen, and the base is used. An invalid code is refused visibly.
func Load(lang string) (*Catalog, error) {
	if lang == "" {
		lang = "en"
	}
	if !langPattern.MatchString(lang) {
		return nil, fmt.Errorf("language %q is not a valid language code (expected like \"en\" or \"ru\")", lang)
	}
	c := &Catalog{lang: lang, m: map[string]string{}, miss: map[string]bool{}}
	data, err := textsFS.ReadFile("texts/" + lang + ".json")
	if err != nil {
		if lang == "en" || errors.Is(err, fs.ErrNotExist) {
			return c, nil // pass-through base; File() reports the gap
		}
		return nil, err
	}
	if err := decodeUnique(lang, data, c.m); err != nil {
		return nil, err
	}
	c.file = true
	return c, nil
}

// decodeUnique decodes the flat object and refuses duplicate keys:
// encoding/json would silently keep the last value, and a silent
// shadowed entry is exactly how a translation file rots.
func decodeUnique(lang string, data []byte, out map[string]string) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("texts/%s.json: %v", lang, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("texts/%s.json: expected one flat JSON object", lang)
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return fmt.Errorf("texts/%s.json: %v", lang, err)
		}
		key, ok := kt.(string)
		if !ok {
			return fmt.Errorf("texts/%s.json: non-string key", lang)
		}
		var v string
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("texts/%s.json: value for %q: %v", lang, key, err)
		}
		if _, dup := out[key]; dup {
			return fmt.Errorf("texts/%s.json: duplicate key %q (a shadowed entry never says so at runtime)", lang, key)
		}
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("texts/%s.json: key %q has empty text (empty renders as nothing - never allowed)", lang, key)
		}
		out[key] = v
	}
	return nil
}

// T resolves one English source string. Hit -> the translation; miss ->
// the English base passed through (counted, never silent at the menu
// level: the menu states the miss count once per session).
func (c *Catalog) T(s string) string {
	if c == nil {
		return s
	}
	if v, ok := c.m[s]; ok {
		return v
	}
	if c.lang != "en" {
		c.mu.Lock()
		c.miss[s] = true
		c.mu.Unlock()
	}
	return s
}

// Lang is the selected language code.
func (c *Catalog) Lang() string {
	if c == nil {
		return "en"
	}
	return c.lang
}

// HasFile says whether a texts/<lang>.json was found for this language.
// False for an unshipped code: the caller must state that openly.
func (c *Catalog) HasFile() bool { return c != nil && c.file }

// MissCount is how many distinct English strings have passed through
// untranslated so far (0 for "en").
func (c *Catalog) MissCount() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.miss)
}

// MissList returns the counted pass-through keys, sorted, for tests
// and diagnostics.
func (c *Catalog) MissList() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.miss))
	for k := range c.miss {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Languages lists the shippable language codes found in texts/ ("en"
// is implicit: the base needs no file).
func Languages() []string {
	out := []string{"en"}
	_ = fs.WalkDir(textsFS, "texts", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := strings.TrimSuffix(strings.TrimPrefix(p, "texts/"), ".json")
		if name != "en" {
			out = append(out, name)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// FileText exposes one shipped language file's decoded map for the
// guards (keys and values both get honesty-checked).
func FileText(lang string) (map[string]string, error) {
	data, err := textsFS.ReadFile("texts/" + lang + ".json")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := decodeUnique(lang, data, m); err != nil {
		return nil, err
	}
	return m, nil
}

// ShippedLanguages returns every language code with a file (ru etc.),
// for guard loops.
func ShippedLanguages() []string {
	var out []string
	_ = fs.WalkDir(textsFS, "texts", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := strings.TrimSuffix(strings.TrimPrefix(p, "texts/"), ".json")
		if name != "en" {
			out = append(out, name)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// VerbRE and ProtocolWords back the guards that every language file
// must pass; they are exported so the cli guards can reuse one rule.
var VerbRE = regexp.MustCompile(`%[+\-# 0-9.]*[a-z]`)

// ProtocolWords are the honesty result words. They are protocol, not
// prose: every language keeps them exactly, in exactly the same
// occurrence count they have in the English source.
var ProtocolWords = []string{"PASS", "FAIL", "INCOMPLETE", "UNVERIFIED", "UNKNOWN"}

// CountWord counts a word on whole-word boundaries (PASS never counts
// inside PASSPORT).
func CountWord(s, word string) int {
	return len(regexp.MustCompile(`\b`+regexp.QuoteMeta(word)+`\b`).FindAllString(s, -1))
}

// QuotedLiterals extracts Go string literals passed to tx() in source
// text, for the coverage and staleness guards.
var TxCallRE = regexp.MustCompile(`\btx\("((?:[^"\\]|\\.)*)"\)`)

// Unquote decodes one captured Go literal.
func Unquote(raw string) (string, error) {
	return strconv.Unquote(`"` + raw + `"`)
}
