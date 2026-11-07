package cli

import (
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// The profile catalog and the preset catalog are two names for one
// promise: a card that compiles to a preset must find it in the CLI
// generator set, and the memory requirement must agree on both sides
// - the R02 resource gate reads the preset's NeedsMemory, the menu
// will read the card's, and a disagreement would let one path refuse
// what the other promised (or the reverse).
func TestCatalogPresetsResolve(t *testing.T) {
	byName := map[string]preset{}
	for _, p := range presets {
		byName[p.Name] = p
	}
	for _, card := range catalog.All() {
		if card.Preset == "" {
			continue
		}
		pr, ok := byName[card.Preset]
		if !ok {
			t.Errorf("card %s names preset %q that the CLI does not have", card.ID, card.Preset)
			continue
		}
		if pr.NeedsMemory != card.NeedsMemory {
			t.Errorf("card %s NeedsMemory=%t but preset %q says %t: the gate and the menu would disagree",
				card.ID, card.NeedsMemory, card.Preset, pr.NeedsMemory)
		}
	}
}

// A session card's NeedsMemory must equal the kernel registry's
// RequiresMemory for the same workload: the registry is what the
// executor preflight enforces, and the card is what the menu offers.
// Two answers for one fact is exactly the drift the audit punished.
func TestCatalogNeedsMemoryMatchesRegistry(t *testing.T) {
	for _, card := range catalog.All() {
		if card.Workload == "" || card.External != "" || card.Availability != catalog.Available {
			continue
		}
		wl, ok := workload.Get(card.Workload)
		if !ok {
			continue // the catalog package pins resolvability of available cards
		}
		if wl.RequiresMemory != card.NeedsMemory {
			t.Errorf("card %s NeedsMemory=%t but kernel %q RequiresMemory=%t",
				card.ID, card.NeedsMemory, card.Workload, wl.RequiresMemory)
		}
	}
}
