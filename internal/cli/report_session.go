package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

// reportSession is the U02 session-bound report. The manifest lists
// the journals the session's child runs wrote, and every run carries
// its session id in run_meta (S2), so runs stay attributed to their
// session even when several sessions share one log file. A listed
// journal that went missing stays listed as MISSING - never
// substituted with another file - and runs belonging to other
// sessions (or predating session attribution) are counted and
// excluded, never silently merged into this session's totals.
func reportSession(sessionID string, asJSON, anomalies bool) int {
	m, err := loadSessionManifest(sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read session %q: %v\n", sessionID, err)
		return 2
	}

	type fileView struct {
		Name          string          `json:"name,omitempty"`
		Journal       string          `json:"journal,omitempty"`
		Status        string          `json:"status"` // ok | missing | unnamed
		Attributed    int             `json:"attributed_runs"`
		NotAttributed int             `json:"not_attributed_runs"` // other sessions or predates attribution
		Issues        *journal.Issues `json:"journal_issues,omitempty"`
		attributed    []journal.RunReport
	}
	var files []fileView
	var attributed []journal.RunReport
	for _, r := range m.Runs {
		jp := r.Journal
		if jp == "" && r.Log != "" {
			jp = journal.Path(r.Log) // the journal's naming rule is public; deriving it is not a guess
		}
		fv := fileView{Name: r.Name, Journal: jp}
		switch {
		case jp == "":
			fv.Status = "unnamed" // the manifest never learned this child's files: unknown, not guessed
		default:
			if _, statErr := os.Stat(jp); statErr != nil {
				fv.Status = "missing"
				break
			}
			fv.Status = "ok"
			recs, iss := journal.ReadAll(jp)
			fv.Issues = iss
			for _, rep := range journal.RecoverRuns(recs) {
				if rep.Meta != nil && rep.Meta.SessionID == sessionID {
					fv.attributed = append(fv.attributed, rep)
				} else {
					fv.NotAttributed++
				}
			}
		}
		fv.Attributed = len(fv.attributed)
		files = append(files, fv)
		attributed = append(attributed, fv.attributed...)
	}

	dips := map[int][]journal.ClockDip{}
	if anomalies {
		dips = journal.DetectClockAnomalies(attributed)
	}

	if asJSON {
		agg := summaryToJSON("", journal.SummarizeCores(attributed), dips, nil, wheaSummary(attributed))
		js := struct {
			Schema     int              `json:"schema"`
			SessionID  string           `json:"session_id"`
			Kind       string           `json:"kind"`
			State      string           `json:"state"`
			Started    string           `json:"started"`
			Finished   string           `json:"finished,omitempty"` // absent = never finalized (crash, or still running)
			Exit       *int             `json:"exit,omitempty"`     // null = the session never ended observably
			Validation string           `json:"validation,omitempty"`
			RecipeID   string           `json:"recipe_id,omitempty"`
			ProfileID  string           `json:"profile_id,omitempty"`
			Files      []fileView       `json:"files"`
			Runs       int              `json:"attributed_runs"`
			Whea       *wheaSummaryJSON `json:"whea,omitempty"`
			Provenance []provenanceJSON `json:"provenance,omitempty"`
			RunDetails []runDetailJSON  `json:"run_details,omitempty"`
			Cores      []reportCoreJSON `json:"cores"`
		}{
			Schema: 1, SessionID: m.SessionID, Kind: m.Kind, State: m.State,
			Started: m.Started, Finished: m.Finished, Exit: m.Exit, Validation: m.Validation,
			RecipeID: m.RecipeID, ProfileID: m.ProfileID, Files: files, Runs: len(attributed),
			Whea: wheaSummary(attributed), Provenance: provenanceList(attributed),
			RunDetails: runDetails(attributed), Cores: agg.Cores,
		}
		out, err := json.MarshalIndent(js, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot encode report: %v\n", err)
			return 2
		}
		fmt.Println(string(out))
		return 0
	}

	fmt.Printf("session %s: kind=%s state=%s started=%s", m.SessionID, m.Kind, m.State, m.Started)
	if m.Finished == "" {
		fmt.Print(" finished=unfinished (crashed, or still running?)")
	} else {
		fmt.Printf(" finished=%s", m.Finished)
	}
	if m.Exit == nil {
		fmt.Print(" exit=unknown")
	} else {
		fmt.Printf(" exit=%d", *m.Exit)
	}
	if m.Validation == "" {
		fmt.Print(" verification=unknown")
	} else {
		fmt.Printf(" verification=%s", m.Validation)
	}
	if m.RecipeID != "" {
		fmt.Printf(" recipe=%s", m.RecipeID)
	} else {
		fmt.Print(" recipe=unbound")
	}
	fmt.Println()

	if len(files) == 0 {
		fmt.Println("this session never listed child run files (it died before the plan was built, or predates file listing).")
	}
	for _, fv := range files {
		label := fv.Journal
		if fv.Name != "" {
			label = fmt.Sprintf("%s (%s)", fv.Name, label)
		}
		switch fv.Status {
		case "missing":
			fmt.Printf("  %s: MISSING - the manifest lists this journal, the file is gone; nothing is substituted for it\n", label)
		case "unnamed":
			fmt.Printf("  %s: the manifest never learned this child's files (unknown, not guessed)\n", label)
		default:
			fmt.Printf("  %s: %d run(s) attributed to this session", label, fv.Attributed)
			if fv.NotAttributed > 0 {
				fmt.Printf(", %d run(s) in the same file belong to other sessions or predate attribution (excluded)", fv.NotAttributed)
			}
			fmt.Println()
			printJournalIssues(os.Stdout, fv.Issues)
		}
	}

	if len(attributed) == 0 {
		fmt.Println("no runs attributed to this session: its journals are missing, unreadable, or predate session attribution.")
		return 0
	}
	fmt.Printf("core report for session %s (%d run(s)):\n", m.SessionID, len(attributed))
	printProvenance(os.Stdout, attributed)
	journal.PrintSummary(os.Stdout, journal.SummarizeCores(attributed))
	printWheaSummary(os.Stdout, attributed)
	printAnomalies(os.Stdout, dips)
	return 0
}
