//go:build windows

package whea

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// wheaFilter selects the providers that publish hardware error records.
// The names differ by Windows generation; we take all of them rather
// than promise an exact list. The XPath subset of the event service is
// strict: every predicate needs its closing bracket - a missing "]"
// here is not tolerated with a warning, the whole query is rejected
// (wevtutil exit 15001 "syntax error at position <end>", which reads
// like a Windows quirk and is actually our malformed query). The
// textual test in whea_query_windows_test.go pins this shape.
const wheaFilter = "((Provider[@Name='Microsoft-Windows-WHEA-Logger'] or " +
	"Provider[@Name='WHEA-Logger'] or " +
	"Provider[@Name='Machine Check Exception']))"

// probeFilter matches any record with a RecordID: it is used only to
// read the log's oldest/newest surviving EventRecordID, so clearing
// and rollover are noticed without assuming anything about WHEA gaps.
const probeFilter = "(EventRecordID>0)"

var errUnsupported = errors.New("whea: wevtutil is not available")

// wevtutilQuerier reads the System log through wevtutil /f:xml. The
// EventRecordID cursor is expressed directly in the XPath query:
// Windows gives us ordered, ID-bounded reads, so no append-atomicity
// or client-side dedup assumptions are needed.
type wevtutilQuerier struct{}

func newQuerier() (querier, bool) {
	if _, err := exec.LookPath("wevtutil"); err != nil {
		return nil, false
	}
	return wevtutilQuerier{}, true
}

// xpath wraps a filter the way the event service expects. It is its own
// function so the exact query text can be tested: one stray bracket here
// is not a warning, it costs the whole run its WHEA coverage.
func xpath(filter string) string { return "*[System[" + filter + "]]" }

// wheaCursorFilter is the cursor-read predicate: records newer than the
// cursor, published by any of the WHEA-capable providers.
func wheaCursorFilter(after int) string {
	return fmt.Sprintf("((EventRecordID>%d) and %s)", after, wheaFilter)
}

// run executes one query under the caller's context, itself capped at
// 10s. A context kill mid-output still leaves the complete events
// wevtutil managed to print: the decoder keeps partial batches.
// oldestFirst=false asks for the newest records first (/rd:true);
// paging through the cursor uses oldest-first.
func (wevtutilQuerier) run(ctx context.Context, filter string, count int, oldestFirst bool) ([]Event, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "wevtutil",
		"qe", "System",
		"/q:"+xpath(filter),
		"/c:"+strconv.Itoa(count),
		"/rd:"+strconv.FormatBool(!oldestFirst),
		"/f:xml",
	).Output()
	if err != nil {
		// wevtutil's own complaint belongs in the error: an exit code
		// alone (15001: the XPath or query was rejected by the event
		// service) hides WHY, and the monitoring reason a report shows
		// must carry the tool's words, bounded so a multiline dump
		// never floods a log line. %w is kept: callers still see the
		// exit status.
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			tail := strings.TrimSpace(string(ee.Stderr))
			if len(tail) > 200 {
				tail = "..." + tail[len(tail)-197:]
			}
			return nil, fmt.Errorf("%w: wevtutil: %s", err, tail)
		}
		return nil, err
	}
	return parseWevtutilXML(string(out)), nil
}

// query returns WHEA records with EventRecordID > after, oldest first.
func (w wevtutilQuerier) query(ctx context.Context, after, limit int) ([]Event, error) {
	return w.run(ctx, wheaCursorFilter(after), limit, true)
}

// recent returns the newest WHEA records (for the 'whea' command).
func (w wevtutilQuerier) recent(ctx context.Context, limit int) ([]Event, error) {
	return w.run(ctx, wheaFilter, limit, false)
}

// newest reports the overall newest surviving EventRecordID (any
// provider): the bounded high-water mark for the cursor.
func (w wevtutilQuerier) newest(ctx context.Context) (int, error) {
	evs, err := w.run(ctx, probeFilter, 1, false)
	if err != nil {
		return 0, err
	}
	if len(evs) == 0 {
		return 0, nil // empty log: nothing to observe yet
	}
	return evs[0].RecordID, nil
}

// oldest reports the overall oldest surviving EventRecordID: if it has
// passed our cursor, records were evicted before we saw them.
func (w wevtutilQuerier) oldest(ctx context.Context) (int, error) {
	evs, err := w.run(ctx, probeFilter, 1, true)
	if err != nil {
		return 0, err
	}
	if len(evs) == 0 {
		return 0, nil
	}
	return evs[0].RecordID, nil
}
