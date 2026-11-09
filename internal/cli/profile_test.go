package cli

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

// C3: the recipe records the catalog profile it came from - but only
// when exactly one available card compiles to the entry point's
// choices. Ambiguity or absence stays unknown, never a guess.
func TestProfileForSessionChoice(t *testing.T) {
	for _, tc := range []struct {
		mode     SessionMode
		workload string
		want     string // card ID, "" = unknown
	}{
		{sessionAllCore, "int", "cpu-all"},
		{sessionPerCore, "int", "cpu-per-core"},
		{sessionAllCore, "float", "cpu-all-float"},
		{sessionAllCore, "ram-constant", "ram-constant"},
		{sessionAllCore, "ram-extended", ""}, // preset card: never a session-workload match
		{sessionAllCore, "no-such-workload", ""},
		{sessionAllCore, "", ""},
		{SessionMode("bogus"), "int", ""},
	} {
		got := profileForSession(tc.mode, tc.workload)
		if got.ID != tc.want {
			t.Errorf("profileForSession(%q, %q) = %q, want %q", tc.mode, tc.workload, got.ID, tc.want)
		}
		if tc.want != "" && got.Version == "" {
			t.Errorf("matched card %q lost its version", tc.want)
		}
	}
}

func TestProfileForPreset(t *testing.T) {
	for _, tc := range []struct{ preset, want string }{
		{"core-cycle", "cpu-core-cycle"},
		{"transitions", "cpu-transitions"},
		{"mixed-validation", "mix-comprehensive"},
		{"ram-extended", "ram-extended"}, // P07f: the preset now has an available card
		{"quick-check", "quick-check"},   // R17: the quick preset's card stamps recipe provenance too
		{"ram-quick", ""},                // profiles without a card stay unknown provenance, never a guess
		{"", ""},
		{"no-such-preset", ""},
	} {
		if got := profileForPreset(tc.preset); got.ID != tc.want {
			t.Errorf("profileForPreset(%q) = %q, want %q", tc.preset, got.ID, tc.want)
		}
	}
}

// uniqueCard's guard is what keeps provenance honest: a match that
// finds two cards (or none) is unknown, never a coin flip.
func TestUniqueCardRejectsAmbiguity(t *testing.T) {
	if p := uniqueCard(func(catalog.Profile) bool { return true }); p.ID != "" {
		t.Errorf("match-all resolved to %q, want unknown", p.ID)
	}
	if p := uniqueCard(func(catalog.Profile) bool { return false }); p.ID != "" {
		t.Errorf("match-none resolved to %q, want unknown", p.ID)
	}
}

// The wizard save site stamps the profile; an unmappable choice stays
// unknown instead of borrowing a similar card.
func TestSessionRecipeCarriesProfileMetadata(t *testing.T) {
	cfg := config.Default()
	s := SessionOptions{Mode: sessionPerCore, Workload: "int", Passes: 1}
	lr := sessionRecipe(cfg, s)
	if lr.ProfileID != "cpu-per-core" || lr.ProfileVersion != "v1" {
		t.Errorf("metadata = %q %q, want cpu-per-core v1", lr.ProfileID, lr.ProfileVersion)
	}
	s2 := SessionOptions{Mode: sessionAllCore, Workload: "no-such-workload", Passes: 1}
	if lr2 := sessionRecipe(cfg, s2); lr2.ProfileID != "" || lr2.ProfileVersion != "" {
		t.Errorf("unmappable choice fabricated metadata: %q %q", lr2.ProfileID, lr2.ProfileVersion)
	}
}

// Profile metadata is provenance, not experiment identity: the same
// compiled request must keep one plan_hash with or without the label,
// or a profile card rename would split coverage history that is
// actually the same experiment.
func TestPlanHashIgnoresProfileMetadata(t *testing.T) {
	cfg := config.Default()
	s := SessionOptions{Mode: sessionAllCore, Workload: "int", Passes: 1}
	bare := sessionRecipe(cfg, s)
	bare.ProfileID, bare.ProfileVersion = "", ""
	label := sessionRecipe(cfg, s)
	if label.ProfileID == "" {
		t.Fatal("fixture lost its profile")
	}
	opt := plan.Options{TestName: "general", A: "int"}
	if a, b := planHash(bare, opt), planHash(label, opt); a != b {
		t.Errorf("profile label changed the experiment identity: %s vs %s", a, b)
	}
}

// Each saved request gets its own id; a hand-written v2 recipe without
// one loads as unknown (""), never as an assumed binding.
func TestRecipeIDStampedAndDistinct(t *testing.T) {
	chdirTemp(t)
	cfg := config.Default()
	fixture := func(mode SessionMode) lastRun {
		return sessionRecipe(cfg, SessionOptions{Mode: mode, Workload: "int",
			Duration: time.Second, Passes: 1, PhysicalCores: []int{0}})
	}
	first := fixture(sessionAllCore)
	if err := saveLastRun(first); err != nil {
		t.Fatal(err)
	}
	got, err := loadLastRun()
	if err != nil {
		t.Fatal(err)
	}
	if got.RecipeID == "" {
		t.Fatal("saved recipe lost its id")
	}
	second := fixture(sessionPerCore)
	if err := saveLastRun(second); err != nil {
		t.Fatal(err)
	}
	got2, err := loadLastRun()
	if err != nil {
		t.Fatal(err)
	}
	if got2.RecipeID == got.RecipeID {
		t.Errorf("two saves share recipe id %q", got2.RecipeID)
	}

	chdirTemp(t)
	body := `{"version":2,"kind":"scenario","config":{},"phases":[{"name":"x","test":"general","a":"int","duration":"1s"}]}`
	if err := os.WriteFile(lastrunPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	lr, err := loadLastRun()
	if err != nil {
		t.Fatal(err)
	}
	if lr.RecipeID != "" {
		t.Errorf("v2 recipe without an id loaded as %q, want unknown", lr.RecipeID)
	}
}

// The repeat line shows the profile when the recipe knows it, and
// says nothing invented when it does not.
func TestDescribeShowsKnownProfile(t *testing.T) {
	cfg := config.Default()
	lr := sessionRecipe(cfg, SessionOptions{Mode: sessionAllCore, Workload: "int",
		Duration: time.Second, Passes: 1, PhysicalCores: []int{0}})

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	describeSession(&lr)
	w.Close()
	os.Stdout = old
	var buf strings.Builder
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	line := buf.String()
	if !strings.Contains(line, "profile cpu-all v1") {
		t.Errorf("describeSession lost the profile:\n%s", line)
	}

	lr.ProfileID, lr.ProfileVersion = "", ""
	old = os.Stdout
	r, w, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	describeSession(&lr)
	w.Close()
	os.Stdout = old
	buf.Reset()
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "profile") {
		t.Errorf("unknown profile fabricated a label:\n%s", buf.String())
	}
}
