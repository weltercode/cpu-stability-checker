package plan

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/config"
)

// The Q04 policy surface: where the parking word may be written, what
// it takes effect on, and what happens to a word written somewhere it
// cannot land. The vocabulary itself is parsed once (transition) -
// these tables pin the PLUMBING: global default, phase override, the
// shape rule, and the hash-identity consequences of each.

func parkingBase() config.Config {
	cfg := config.Default()
	cfg.Test = "general"
	cfg.Cores = []int{0, 1}
	cfg.Duration = "5s"
	return cfg
}

func TestParkingPolicyResolution(t *testing.T) {
	cases := []struct {
		name    string
		global  string // config-level parking_policy
		phase   string // phase-level parking_policy
		bursts  bool   // the phase drives prepared sessions
		want    string // resolved Options.Parking ("" = inert)
		errPart string // expected error fragment ("" = must succeed)
	}{
		{"no word anywhere: today's run", "", "", true, "", ""},
		{"global try reaches a bursts phase", "try", "", true, "try", ""},
		{"global required reaches a bursts phase", "required", "", true, "required", ""},
		// The global default shapes transition phases only: an
		// ordinary phase has no measured session idle windows, and
		// an inert word must never fork a run identity.
		{"global try never reaches an ordinary phase", "try", "", false, "", ""},
		{"phase says the global away with off", "try", "off", true, "", ""},
		{"phase raises the global try to required", "try", "required", true, "required", ""},
		{"OFF spelled out normalizes to the empty default", "", "OFF", true, "", ""},
		{"try padded is still try", "", "  try  ", true, "try", ""},
		// A word the phase itself NAMED but cannot carry is the
		// author's promise to break: visible error, never a drop.
		{"named parking on an ordinary phase is refused", "", "try", false, "", "no bursts"},
		{"named parking on an ordinary phase names the word too", "required", "required", false, "", `parking_policy "required"`},
		// Unknown words error regardless of shape (the execution-
		// vocabulary rule), and name the whole vocabulary.
		{"unknown global word is refused by name", "parked", "", true, "", "off|try|required"},
		{"unknown phase word is refused by name", "", "on", true, "", "off|try|required"},
		{"unknown word on a shapeless phase errors before the shape rule", "auto", "", false, "", "off|try|required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := parkingBase()
			cfg.ParkingPolicy = c.global
			ph := config.Phase{Name: "p", ParkingPolicy: c.phase}
			if c.bursts {
				ph.PerProcess, ph.Bursts = true, 2
			}
			opt, err := OptionsFromPhase(cfg, ph)
			if c.errPart != "" {
				if err == nil || !strings.Contains(err.Error(), c.errPart) {
					t.Fatalf("error = %v, want it to name %q", err, c.errPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("OptionsFromPhase() error = %v", err)
			}
			if opt.Parking != c.want {
				t.Errorf("resolved Parking = %q, want %q", opt.Parking, c.want)
			}
			// The ordinary-phase clear must not disturb ANY other
			// resolved field: an inert word changes nothing else.
			if !c.bursts && c.errPart == "" {
				plain, err := OptionsFromPhase(parkingBase(), config.Phase{Name: "p"})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(opt, plain) {
					t.Errorf("a cleared global word still shaped the plan: %+v vs %+v", opt, plain)
				}
			}
		})
	}
}

func TestParkingOptionsFromConfigVocabulary(t *testing.T) {
	cfg := parkingBase()
	for _, c := range []struct {
		word    string
		want    string
		wantErr bool
	}{
		{"yes", "", true},
		{"try!", "", true}, // near-miss words are NOT normalized away
		{"parking", "", true},
		{"REQUIRED", "required", false}, // case and trim normalize, values do not
		{" Try ", "try", false},
	} {
		cfg.ParkingPolicy = c.word
		opt, err := OptionsFromConfig(cfg)
		if c.wantErr {
			if err == nil {
				t.Errorf("OptionsFromConfig() accepted parking word %q (%+v)", c.word, opt)
			}
			continue
		}
		if err != nil {
			t.Fatalf("OptionsFromConfig(%q): %v", c.word, err)
		}
		if opt.Parking != c.want {
			t.Errorf("Parking = %q, want %q (normalized)", opt.Parking, c.want)
		}
	}
	// "off" and absent resolve to the identical Options - spelling the
	// default out loud must not fork a run's identity.
	on := parkingBase()
	on.ParkingPolicy = "off"
	a, err := OptionsFromConfig(on)
	if err != nil {
		t.Fatal(err)
	}
	b, err := OptionsFromConfig(parkingBase())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("off and absent differ: %+v vs %+v", a, b)
	}
}

// The hash-identity contract at the marshaling level (the cli hash
// test pins the end-to-end consequence): Options is what planHash
// marshals wholesale, so the parking field must be INVISIBLE at its
// zero value (every pre-Q04 saved recipe keeps its exact hash) and
// VISIBLE once active (a run that observes parking and one that does
// not can never share an experiment identity).
func TestParkingFieldIdentityContract(t *testing.T) {
	b, err := json.Marshal(Options{TestName: "general", Bursts: 2})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"Parking"`) {
		t.Errorf(`empty Parking must be absent from the marshaled plan (omitempty is a machine contract, not style): %s`, b)
	}
	b, err = json.Marshal(Options{TestName: "general", Bursts: 2, Parking: "try"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"Parking":"try"`) {
		t.Errorf("an active policy must be visible to the plan hash: %s", b)
	}
}

func TestReproductionParametersStatesParking(t *testing.T) {
	opt, err := OptionsFromPhase(parkingBase(), config.Phase{
		Name: "tr", PerProcess: true, Bursts: 2, ParkingPolicy: "try",
	})
	if err != nil {
		t.Fatal(err)
	}
	line := ReproductionParameters(opt)
	if !strings.Contains(line, "parking=try") {
		t.Errorf("reproduction line must state the observation policy: %q", line)
	}
	plain, err := OptionsFromPhase(func() config.Config {
		c := parkingBase()
		c.ParkingPolicy = "required" // global, but this phase cannot carry it
		return c
	}(), config.Phase{Name: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ReproductionParameters(plain), "parking") {
		t.Errorf("an ordinary run must not claim a parking policy it does not run: %q", ReproductionParameters(plain))
	}
}
