package catalog

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// R39: every built-in card carries a facet record, and every facet
// says something - an empty placeholder is the "unknown presented as
// content" the honesty rules forbid. R40 adds Short: it must exist
// and stay one screen row's worth (the list row is the short form;
// anything longer belongs to the card screen).
func TestFacetsCoverEveryBuiltInCard(t *testing.T) {
	for _, p := range All() {
		f, ok := FacetsFor(p.ID)
		if !ok {
			t.Errorf("card %s has no facet record", p.ID)
			continue
		}
		if f.UseWhen == "" || f.Catches == "" || f.Limits == "" {
			t.Errorf("card %s has an empty facet: %+v", p.ID, f)
		}
		if f.Short == "" {
			t.Errorf("card %s has no short list line", p.ID)
		} else if utf8.RuneCountInString(f.Short) > 74 {
			t.Errorf("card %s short line is %d runes, over one row at 80 cols: %q", p.ID, utf8.RuneCountInString(f.Short), f.Short)
		}
	}
}

// R40, owner-named ("What the heck is P07 - no one understand this"):
// development queue IDs never surface in user-facing wording. This
// scans EVERY display string - card fields and facets alike. The
// first run of this test against the pre-R40 corpus was meant to
// fail, and it did: "the full P07 set" leaked in a card summary and
// in a facet.
func TestNoInternalIDsInDisplayText(t *testing.T) {
	// The pattern discriminates, so the scan above means something:
	// the real pre-R40 leak hits, and legitimate product wording with
	// digits does not.
	for _, bad := range []string{
		"the full P07 set: base cycles",
		"added in P07f",
		"audit A05 rule",
		"see Q06",
	} {
		if InternalIDPattern.FindString(bad) == "" {
			t.Errorf("guard pattern misses the development ID in %q", bad)
		}
	}
	for _, fine := range []string{
		"TestMem5 Extreme1 sequence, 31 steps, 18 phases",
		"FPU/SSE/AVX paths, SSE2",
		"~21 s planned load + setup",
		"128 MiB over 4 MB blocks",
	} {
		if m := InternalIDPattern.FindString(fine); m != "" {
			t.Errorf("guard pattern false-positives %q in %q", m, fine)
		}
	}
	for what, s := range DisplayStrings() {
		if m := InternalIDPattern.FindString(s); m != "" {
			t.Errorf("%s carries development ID %q in user-facing text: %q", what, m, s)
		}
	}
}

// The facet map is keyed exactly by the built-in IDs: a typo'd key
// would silently leave a card without facets while the coverage test
// still "passes" for the typo'd name.
func TestFacetKeysAreAllRealCards(t *testing.T) {
	real := map[string]bool{}
	for _, p := range All() {
		real[p.ID] = true
	}
	for id := range facetsByID {
		if !real[id] {
			t.Errorf("facet record for unknown card id %q", id)
		}
	}
	if len(facetsByID) != len(All()) {
		t.Errorf("facet records %d, built-in cards %d", len(facetsByID), len(All()))
	}
}

// Facets must never leak into the pinned catalog serialization (the
// `catalog` CLI output contract): the marshaled card carries no facet
// keys, and adding a facet changes no card's bytes.
func TestFacetsNeverEnterProfileJSON(t *testing.T) {
	for _, p := range All() {
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal %s: %v", p.ID, err)
		}
		s := string(b)
		for _, key := range []string{"use_when", "catches", "limits", "facet"} {
			if strings.Contains(s, `"`+key+`"`) {
				t.Errorf("card %s JSON carries facet key %q", p.ID, key)
			}
		}
	}
}

// The accepted wording guards (owner decisions, not style):
// a cost never promised as wall-clock, localization stated honestly,
// and no facet drifting into a machine-wide stability claim.
func TestFacetWordingGuards(t *testing.T) {
	for _, p := range All() {
		f, _ := FacetsFor(p.ID)
		joined := strings.ToLower(f.Short + " " + f.UseWhen + " " + f.Catches + " " + f.Limits)
		if strings.Contains(joined, "identifies the faulty core") {
			t.Errorf("card %s claims identification of faulty silicon: the accepted word is 'helps localize'", p.ID)
		}
		if strings.Contains(joined, "the machine is stable") {
			t.Errorf("card %s facet states a machine-wide stability claim", p.ID)
		}
		if strings.Contains(joined, "guarantee") || strings.Contains(joined, "guaranteed") {
			t.Errorf("card %s facet promises a guarantee", p.ID)
		}
	}
}
