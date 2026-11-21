package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// observations.json (R39) is the small durable per-profile "last on
// this machine" landmark store: for each catalog profile, the LAST
// attempt recorded against it - whatever its word was. It is shown
// beside the card explanations so the operator can see what this
// machine last actually did with this goal. The accepted decision is
// explicit: the LAST ATTEMPT is shown, not the last PASS - yesterday's
// PASS must never sit pretty in front of today's FAIL.
//
// It is DERIVED data: deleting the file is legal, the tool regrows it
// from future runs and never re-invents past landmarks from memory.
// Like lastrun/lastresult/findings/config it is protected from
// history cleanup by identity (history_prune.go), because it is not
// history evidence - it is this machine's current view.
//
// Honesty rules, pinned by tests:
//   - an attempt is attributed only through the SAME recipe binding
//     describeLastOutcome uses: the receipt's recipe_id equals the
//     live slot's, and the slot names a catalog profile. A manual
//     wizard choice (no profile) or a displaced recipe records
//     NOTHING - absence stated, never a guess about which card ran;
//   - a corrupt or newer-version store reads as "unknown", never as
//     zero and never as "never tested";
//   - a newer FinishedAt never gets overwritten by a staler attempt.
const observationsVersion = 1

// observationsPath is a var so tests can point it at a scratch file.
var observationsPath = "observations.json"

type observation struct {
	ProfileVersion string `json:"profile_version"`
	AttemptID      string `json:"attempt_id"`
	FinishedAt     string `json:"finished_at"`
	Verdict        string `json:"verdict"`
	VerdictReason  string `json:"verdict_reason,omitempty"`
}

type observationsFile struct {
	Version   int                    `json:"version"`
	UpdatedAt string                 `json:"updated_at"`
	Profiles  map[string]observation `json:"profiles"`
}

// recordObservation runs at the receipt's finalize point with the
// same final record the receipt carried. Failures are warnings: a
// lost landmark never changes a run's verdict.
func recordObservation(r lastResult) {
	if r.State != receiptFinal || r.RecipeID == "" || r.AttemptID == "" {
		return
	}
	lr, err := loadLastRun()
	if err != nil || lr.RecipeID != r.RecipeID || lr.ProfileID == "" {
		return // not attributable to a card: nothing recorded, nothing guessed
	}
	obs, err := loadObservations()
	if err != nil {
		// Unreadable or foreign: the fresh write replaces it and the
		// new file states only what this attempt observed.
		obs = nil
	}
	if obs == nil || obs.Version != observationsVersion {
		obs = &observationsFile{Version: observationsVersion, Profiles: map[string]observation{}}
	}
	if obs.Profiles == nil {
		obs.Profiles = map[string]observation{}
	}
	if prev, ok := obs.Profiles[lr.ProfileID]; ok && !attemptNewer(prev.FinishedAt, r.FinishedAt) {
		return // a staler (or unreadably-stamped) attempt does not displace a newer record
	}
	v := verdictOfReceipt(&r)
	obs.Profiles[lr.ProfileID] = observation{
		ProfileVersion: lr.ProfileVersion,
		AttemptID:      r.AttemptID,
		FinishedAt:     r.FinishedAt,
		Verdict:        string(v.Kind),
		VerdictReason:  v.Reason,
	}
	obs.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveObservations(*obs); err != nil {
		fmt.Fprintf(os.Stderr, "warning: landmark observation not saved (%v); the run result itself is unaffected\n", err)
	}
}

// attemptNewer compares the two UTC stamps: an unparseable FRESH
// stamp never displaces a record (a run that cannot say when it
// ended cannot overwrite one that can); a valid fresh stamp replaces
// an unreadable old one, and equal times let the newer write win.
func attemptNewer(prev, fresh string) bool {
	tf, errF := time.Parse(time.RFC3339Nano, fresh)
	if errF != nil {
		return false
	}
	tp, errP := time.Parse(time.RFC3339Nano, prev)
	if errP != nil {
		return true
	}
	return !tf.Before(tp)
}

func saveObservations(obs observationsFile) error {
	obs.Version = observationsVersion
	b, err := json.MarshalIndent(obs, "", "  ")
	if err != nil {
		return err
	}
	dir, base := filepath.Split(observationsPath)
	tmp, err := os.CreateTemp(dir, base+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, observationsPath); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// loadObservations reads the store. Anything unreadable, half-stated
// or written by a newer tool comes back as an error: the reader says
// "unknown" with the reason, never zero.
func loadObservations() (*observationsFile, error) {
	b, err := os.ReadFile(observationsPath)
	if err != nil {
		return nil, err
	}
	var obs observationsFile
	if err := json.Unmarshal(b, &obs); err != nil {
		return nil, fmt.Errorf("%s is not readable: %w", observationsPath, err)
	}
	if obs.Version != observationsVersion {
		return nil, fmt.Errorf("%s was written for another format (version %d)", observationsPath, obs.Version)
	}
	return &obs, nil
}

// observationSummary answers "last attempt on this machine" for one
// profile, for the card screens. Every branch says what it means:
// no file is NOT "never tested" (the store is rebuildable), a broken
// file is unknown, and a recorded line names the word, the attempt
// and the stamp - the LAST attempt, whatever it was.
func observationSummary(profileID string) string {
	obs, err := loadObservations()
	if err != nil {
		if os.IsNotExist(err) {
			return tx("last attempt here: no observation on record") + tx(" (the landmark store is empty; it regrows from future runs)")
		}
		return fmt.Sprintf(tx("last attempt here: unknown (%v)"), err)
	}
	o, ok := obs.Profiles[profileID]
	if !ok {
		return tx("last attempt here: no observation on record")
	}
	if o.Verdict == "" || o.AttemptID == "" {
		// A record that cannot name its word or its attempt is not
		// content: unknown with a reason, never blank fields.
		return tx("last attempt here: unknown (the landmark record is incomplete)")
	}
	return fmt.Sprintf(tx("last attempt here: %s (attempt %s, %s)"), o.Verdict, o.AttemptID, o.FinishedAt)
}

// observationInline is the compact form for the main menu: silence
// when there is nothing to say (an absent or empty store is not
// screen content), but a record - or an unreadable store, exactly
// the case where the operator should know why the answer is unknown.
func observationInline(profileID string) string {
	obs, err := loadObservations()
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		return fmt.Sprintf(tx("     landmarks: unknown (%v)"), err)
	}
	o, ok := obs.Profiles[profileID]
	if !ok {
		return ""
	}
	return fmt.Sprintf(tx("     last attempt here: %s (%s)"), o.Verdict, o.FinishedAt)
}
