package cli

import (
	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/i18n"
)

// R41 (owner-named 20.11): the interactive menu wording resolves
// through one language overlay chosen by config "language". The
// English source strings ARE the base: an untranslated string renders
// its English base, never a raw key, never an empty. Scripted and
// machine-facing paths leave uiTexts nil and are byte-identical to
// before R41 - the pinned contracts and outputs never see a
// translation (records on disk stay in one language by design).
var uiTexts *i18n.Catalog

// tx resolves one user-facing string through the active language.
// nil uiTexts (scripted entry points, headless, tests that never
// select a language) is pass-through - the exact pre-R41 behavior.
func tx(s string) string { return uiTexts.T(s) }

// installTexts selects the language for one interactive session.
// It returns what the menu must say, when the honest answer is not
// silent: an invalid code keeps the English base AND says so; a
// valid-but-unshipped code says which files exist instead of
// pretending. Nothing here ever fails an interactive session: the
// base language is always a valid rendering.
func installTexts(cfg config.Config) []string {
	var notes []string
	c, err := i18n.Load(cfg.Language)
	if err != nil {
		c, _ = i18n.Load("") // English base, visibly
		notes = append(notes, err.Error())
	}
	if c.Lang() != "en" && !c.HasFile() {
		notes = append(notes, c.Lang()+": no texts/"+c.Lang()+".json is shipped in this build - the English base is in effect. Shipped languages: "+joinComma(i18n.ShippedLanguages()))
	}
	uiTexts = c
	return notes
}

func joinComma(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}
