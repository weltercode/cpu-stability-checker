package catalog

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// The catalog is only trustworthy while its invariants hold: stable
// unique IDs, an explicit reason for every non-available card, honest
// shape consistency, and diagnostic cards that cannot leak into a user
// list. These are the checks the menu and the future GUI will rely on.

func TestCatalogIntegrity(t *testing.T) {
	seenID := map[string]bool{}
	idRe := regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	for _, p := range All() {
		t.Run(p.ID, func(t *testing.T) {
			if !idRe.MatchString(p.ID) {
				t.Errorf("ID %q is not a stable lowercase machine ID", p.ID)
			}
			if seenID[p.ID] {
				t.Errorf("duplicate ID %q", p.ID)
			}
			seenID[p.ID] = true
			if p.Version == "" {
				t.Error("Version empty: a journal's profile reference must stay meaningful")
			}
			if p.Title == "" || p.Summary == "" {
				t.Error("Title/Summary empty: every card needs a short honest line")
			}
			switch p.Category {
			case CPU, RAM, MIX:
			default:
				t.Errorf("Category %q outside the menu sections", p.Category)
			}
			switch p.Visibility {
			case Normal, Extended, Diagnostic:
			default:
				t.Errorf("Visibility %q unknown", p.Visibility)
			}
			switch p.Availability {
			case Available:
				if p.Reason != "" {
					t.Errorf("available card carries dead reason text %q", p.Reason)
				}
			case Unavailable, Unknown:
				if p.Reason == "" {
					t.Error("not-available card without a reason: 'why not' is required data")
				}
			default:
				t.Errorf("Availability %q unknown", p.Availability)
			}
			switch p.Validation {
			case ValidationCompute, ValidationMemory, ValidationMemoryLoad,
				ValidationExternal, ValidationSynthetic, ValidationMixed,
				ValidationObservation, ValidationUnknown:
			default:
				t.Errorf("Validation %q outside the honest vocabulary", p.Validation)
			}
			switch p.Shape {
			case ShapeSessionAll, ShapeSessionPerCore:
				if p.Workload == "" {
					t.Error("session shape without a kernel workload")
				}
				if p.Preset != "" {
					t.Error("session shape also names a preset")
				}
			case ShapePreset:
				if p.Preset == "" {
					t.Error("preset shape without a preset ID")
				}
				if p.Workload != "" {
					t.Error("preset shape also names a workload")
				}
			case ShapeNone:
				// Nothing compiles yet: only an honest unavailable (or
				// unknown) card may claim no execution form at all.
				if p.Availability == Available {
					t.Error("available card with no shape: nothing could run it")
				}
			default:
				t.Errorf("Shape %q unknown", p.Shape)
			}
		})
	}
}

// The golden ID set: renaming or dropping a card is a deliberate,
// versioned act, never an accident - journals and saved recipes refer
// to these IDs.
func TestCatalogIDsAreStable(t *testing.T) {
	var got []string
	for _, p := range All() {
		got = append(got, p.ID)
	}
	sort.Strings(got)
	want := []string{
		"cpu-all", "cpu-all-float", "cpu-all-sha256", "cpu-core-cycle",
		"cpu-per-core", "cpu-transitions",
		"diag-fault",
		"mem-bandwidth", "mem-quick",
		"mix-comprehensive", "mix-simultaneous", "mix-transitions-on-ram",
		"ram-address", "ram-constant", "ram-extended", "ram-moving", "ram-random",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("catalog ID set changed:\n got %v\nwant %v", got, want)
	}
}

// The menu plan forbids offering fault, mem-bw and ram-* as equal
// citizens of the CPU list, and forbids developer diagnostics in any
// user list at all.
func TestNormalListsStayHonest(t *testing.T) {
	for _, cat := range []Category{CPU, RAM, MIX} {
		for _, vis := range []Visibility{Normal, Extended} {
			for _, p := range List(cat, vis) {
				if p.Visibility == Diagnostic {
					t.Errorf("%s/%s list contains diagnostic card %s", cat, vis, p.ID)
				}
			}
		}
	}
	for _, p := range List(CPU, Normal) {
		if strings.HasPrefix(p.Workload, "ram-") || p.Workload == "fault" ||
			p.Workload == "mem" || p.Workload == "mem-bw" {
			t.Errorf("CPU normal list offers %q (%s): not a CPU test", p.ID, p.Workload)
		}
	}
	// Not-implemented profiles are present but honestly unavailable -
	// hiding them entirely would promise a menu that was never asked
	// about them; showing them ready would lie. (ram-extended left
	// this list in P07f: it became a real preset profile, v2.)
	for _, id := range []string{"mix-simultaneous", "mix-transitions-on-ram"} {
		p, ok := Get(id)
		if !ok {
			t.Fatalf("card %s missing: unimplemented profiles must be visible as unavailable", id)
		}
		if p.Availability != Unavailable || p.Reason == "" {
			t.Errorf("card %s = %s %q, want unavailable with a reason", id, p.Availability, p.Reason)
		}
	}
	// The sequential comprehensive profile must never call itself
	// simultaneous (audit A05's honesty rule lives in the card text).
	mix, ok := Get("mix-comprehensive")
	if !ok {
		t.Fatal("mix-comprehensive missing")
	}
	summary := strings.ToLower(mix.Summary)
	if !strings.Contains(summary, "sequential") || !strings.Contains(summary, "not a simultaneous") {
		t.Errorf("mix summary does not state sequential-and-not-simultaneous: %q", mix.Summary)
	}
}

// Every session card names a kernel that actually exists in the
// registry: the catalog may not reference a kernel nobody implements
// (preset cards carry no workload, and external cards name
// registered tools, not registry kernels).
func TestCatalogWorkloadsResolve(t *testing.T) {
	for _, p := range All() {
		if p.Workload == "" || p.External != "" {
			continue
		}
		if p.Availability != Available {
			continue // unavailable cards keep their named-but-not-yet workloads honest
		}
		if _, ok := workload.Get(p.Workload); !ok {
			t.Errorf("card %s names kernel %q that the registry does not have", p.ID, p.Workload)
		}
	}
}

// NeedsMemory is explicit card data (the R02 gate reads it), so the
// RAM-family and mixed cards must carry it and the CPU cards must
// not. Preset cards carry no workload prefix to key on - their
// memory promise is stated on the card itself (ram-extended joined
// as a real preset profile in P07f).
func TestCatalogNeedsMemory(t *testing.T) {
	for _, p := range All() {
		want := strings.HasPrefix(p.Workload, "ram-") || p.ID == "mix-comprehensive" || p.ID == "ram-extended"
		if p.NeedsMemory != want {
			t.Errorf("card %s NeedsMemory=%t, want %t", p.ID, p.NeedsMemory, want)
		}
	}
}

func TestListOrderingIsDeterministic(t *testing.T) {
	for _, cat := range []Category{CPU, RAM, MIX} {
		a := List(cat, Extended)
		b := List(cat, Extended)
		if len(a) != len(b) {
			t.Fatalf("List(%s) changed size between calls", cat)
		}
		for i := range a {
			if a[i].ID != b[i].ID {
				t.Fatalf("List(%s) order unstable: %s vs %s", cat, a[i].ID, b[i].ID)
			}
		}
	}
	// Normal never shows more than Extended.
	if len(List(CPU, Normal)) > len(List(CPU, Extended)) {
		t.Error("Normal list is wider than Extended")
	}
}

func TestExternalCard(t *testing.T) {
	present := External("yc", `C:\tools\yc.exe`, true)
	if present.Availability != Available || present.Reason != "" {
		t.Errorf("present tool card = %s %q, want available without reason", present.Availability, present.Reason)
	}
	if present.Validation != ValidationExternal {
		t.Errorf("external validation = %q, want the tool's own protocol", present.Validation)
	}
	missing := External("yc", `C:\tools\yc.exe`, false)
	if missing.Availability != Unavailable || !strings.Contains(missing.Reason, `C:\tools\yc.exe`) {
		t.Errorf("missing tool card = %s %q, want unavailable naming the path", missing.Availability, missing.Reason)
	}
}

// The product UI language is English (audit A05): every display field
// of every card - built-in and the External() factory alike - must be
// free of Cyrillic. Comments and test fixtures may keep the project's
// documentation language; CARD TEXT is the product. This is the
// machine-checked half of "all user-facing strings are English" here;
// the cli package pins its own half over its sources.
func TestCardTextIsEnglish(t *testing.T) {
	cards := append(All(),
		External("yc", `C:\tools\yc.exe`, false),
		External("p95", `C:\tools\p95.exe`, true))
	for _, p := range cards {
		texts := map[string]string{"Title": p.Title, "Summary": p.Summary, "Details": p.Details, "Reason": p.Reason}
		cyrillic := func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }
		for what, s := range texts {
			if strings.IndexFunc(s, cyrillic) >= 0 {
				t.Errorf("card %s: %s carries Cyrillic while the UI language is English: %q", p.ID, what, s)
			}
		}
	}
}

// A card may not speak about OS core parking at all, even now that the
// Q04 evidence contract makes the observation real: parking observation
// is a configuration policy (parking_policy in config.json) that is OFF
// by default, so a run of THIS profile without that policy never
// observes anything - the card describes what a profile always does,
// and it is not parking. The idle→load profile therefore speaks
// "prepared idle/load bursts", which is exactly what this build
// commands and journals. parkwatch and the telemetry caps report
// OBSERVED CPU Set states - honest observation, not a card promise -
// and live outside the catalog; pinning the word out of card text
// keeps a promise from sneaking back with the next wording tweak.
func TestCardsNeverPromiseParking(t *testing.T) {
	parkRe := regexp.MustCompile(`(?i)park`)
	for _, p := range append(All(), External("yc", `C:\tools\yc.exe`, false)) {
		for _, s := range []string{p.Title, p.Summary, p.Details, p.Reason} {
			if parkRe.MatchString(s) {
				t.Errorf("card %s speaks parking in a profile promise: parking evidence is a configuration policy, never a profile claim: %q", p.ID, s)
			}
		}
	}
}
