package cli

import (
	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/config"
)

// C3 (menu plan section 3/5): the recipe records WHICH catalog profile
// the request came from, so a repeat and its receipt carry the profile
// id+version that produced them. The menu (U01 C5) will pick cards
// directly; until then the wizard's goal choices are mapped back to a
// card ONLY when exactly one available card compiles to exactly that
// choice. Two matches or none is unknown (""): provenance is never a
// coin flip, and a raw CLI run has no profile at all.

// profileForSession maps the wizard's mode + workload back to the one
// card that compiles to that session shape.
func profileForSession(mode SessionMode, workloadName string) catalog.Profile {
	shape := catalog.ShapeSessionAll
	switch mode {
	case sessionAllCore:
	case sessionPerCore:
		shape = catalog.ShapeSessionPerCore
	default:
		return catalog.Profile{}
	}
	return uniqueCard(func(p catalog.Profile) bool {
		return p.Shape == shape && p.Workload == workloadName
	})
}

// profileForPreset maps a built-in preset name to the card that hands
// its phases to the scenario pipeline. Presets without a card (and raw
// phase lists) stay unknown.
func profileForPreset(name string) catalog.Profile {
	if name == "" {
		return catalog.Profile{}
	}
	return uniqueCard(func(p catalog.Profile) bool {
		return p.Shape == catalog.ShapePreset && p.Preset == name
	})
}

// uniqueCard returns the single available card matching, or the zero
// Profile when the match is ambiguous or absent - unknown, never a
// guess.
func uniqueCard(match func(catalog.Profile) bool) catalog.Profile {
	var found catalog.Profile
	hits := 0
	for _, p := range catalog.All() {
		if p.Availability != catalog.Available || !match(p) {
			continue
		}
		found, hits = p, hits+1
	}
	if hits != 1 {
		return catalog.Profile{}
	}
	return found
}

// sessionRecipeForProfile stamps a card-driven session (C5): the menu
// picked this exact card, so its id+version is recorded as fact - no
// reverse inference. The reverse mapper stays for the manual wizard,
// where no card was chosen and provenance is only knowable by
// matching the choices back to a single card.
func sessionRecipeForProfile(cfg config.Config, session SessionOptions, card catalog.Profile) lastRun {
	lr := sessionRecipe(cfg, session)
	lr.ProfileID, lr.ProfileVersion = card.ID, card.Version
	return lr
}
