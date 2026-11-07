package plan

import (
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/config"
)

// max_temp flows from config into Options, and a negative limit is a
// configuration error caught before any load.
func TestOptionsMaxTempInheritance(t *testing.T) {
	cfg := config.Default()
	cfg.MaxTemp = 85
	opt, err := OptionsFromConfig(cfg)
	if err != nil {
		t.Fatalf("OptionsFromConfig: %v", err)
	}
	if opt.MaxTempC != 85 {
		t.Fatalf("Options.MaxTempC = %v, want 85", opt.MaxTempC)
	}

	cfg.MaxTemp = -1
	if _, err := OptionsFromConfig(cfg); err == nil {
		t.Fatal("OptionsFromConfig: expected error for negative max_temp")
	}

	// Phases inherit the config limit (no per-phase override).
	cfg.MaxTemp = 70
	opt, err = OptionsFromPhase(cfg, config.Phase{Name: "p"})
	if err != nil {
		t.Fatalf("OptionsFromPhase: %v", err)
	}
	if opt.MaxTempC != 70 {
		t.Fatalf("phase Options.MaxTempC = %v, want 70 (inherited)", opt.MaxTempC)
	}
}
