package cli

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/whea"
)

// wheaCmd lists the most recent WHEA hardware error records, newest
// first, with their classified severity and nature. During runs the
// same records are watched automatically: uncorrected ones abort the
// run, corrected and unknown ones are kept as visible evidence.
func wheaCmd(args []string) int {
	limit := 20
	if len(args) >= 1 {
		if n, err := strconv.Atoi(args[0]); err == nil && n > 0 {
			limit = n
		}
	}

	events, err := whea.QueryRecent(limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if len(events) == 0 {
		fmt.Println("no WHEA events found")
		return 0
	}
	for _, ev := range events {
		fmt.Printf("%s  id=%-5d rec=%-7d %-11s %-12s %s\n",
			ev.Time.Format(time.RFC3339), ev.EventID, ev.RecordID, ev.Severity, ev.Nature, ev.Provider)
	}
	return 0
}
