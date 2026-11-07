package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

func journalCmd(args []string) int {
	path := ""
	if len(args) >= 1 {
		path = args[0]
	}
	if path == "" {
		path = journal.Path(config.Load(config.DefaultPath).LogFile)
	}
	recs, iss := journal.ReadAll(path)
	if len(recs) == 0 && iss.ReadErr != "" {
		fmt.Fprintf(os.Stderr, "cannot open journal %q: %v\n", path, iss.ReadErr)
		return 2
	}
	reports := journal.RecoverRuns(recs)
	if len(reports) == 0 && iss.Empty() {
		fmt.Printf("journal %s: no valid records\n", path)
		return 0
	}
	printJournalIssues(os.Stdout, iss)
	journal.PrintReport(os.Stdout, reports)
	return 0
}

// printJournalIssues states loudly what the journal could not tell us:
// torn lines, corrupt middles, truncated tails. The story below is
// then known-incomplete, and saying so is the whole point.
func printJournalIssues(w io.Writer, iss *journal.Issues) {
	if iss.Empty() {
		return
	}
	fmt.Fprintf(w, "JOURNAL DAMAGE: %d malformed line(s)", iss.Malformed)
	if iss.MissingField > 0 {
		fmt.Fprintf(w, ", %d record(s) without type/run id", iss.MissingField)
	}
	if iss.Unterminated {
		fmt.Fprint(w, ", final line truncated (writer died mid-append)")
	}
	if iss.Unreadable > 0 {
		fmt.Fprintf(w, ", %d file(s) could not be opened", iss.Unreadable)
	}
	if iss.ReadErr != "" {
		fmt.Fprintf(w, ", read error: %s", iss.ReadErr)
	}
	fmt.Fprintln(w, " — the story below is missing pieces")
	for _, pi := range iss.Found {
		fmt.Fprintf(w, "  %s line %d: %s\n", pi.Source, pi.Line, pi.Reason)
	}
}
