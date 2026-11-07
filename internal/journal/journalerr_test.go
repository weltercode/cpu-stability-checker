package journal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// flakyWriter fails exactly the Write calls listed in failAt; it can
// also report short writes and Sync failures without breaking the run.
type flakyWriter struct {
	buf     bytes.Buffer
	writes  int
	failAt  map[int]bool // these Nth Write calls return an error
	shortAt int          // Nth Write reports n < len(p)
	syncErr error        // every Sync fails with this
}

func (f *flakyWriter) Write(p []byte) (int, error) {
	f.writes++
	if f.failAt[f.writes] {
		return 0, fmt.Errorf("disk on fire (write %d)", f.writes)
	}
	if f.shortAt > 0 && f.writes == f.shortAt {
		n, err := f.buf.Write(p[:len(p)/2])
		return n / 2, err // torn append: no error, fewer bytes
	}
	return f.buf.Write(p)
}

func (f *flakyWriter) Sync() error { return f.syncErr }

func TestWriterErrorTracking(t *testing.T) {
	cases := []struct {
		name     string
		w        *flakyWriter
		actions  func(*Writer)
		wantErrs uint64
		wantOp   string // substring of the first error
	}{
		{
			name:     "intact journal reports nothing",
			w:        &flakyWriter{},
			actions:  func(j *Writer) { j.RunStart("t", "", nil, 1, "1s", false); j.CoreDone(0, OK, "") },
			wantErrs: 0,
		},
		{
			name:     "write failure is kept and counted",
			w:        &flakyWriter{failAt: map[int]bool{2: true, 3: true}},
			actions:  func(j *Writer) { j.CoreStart(0, "int"); j.CoreDone(0, OK, ""); j.CoreDone(1, OK, "") },
			wantErrs: 2, // both core_done writes failed
			wantOp:   "write",
		},
		{
			name:     "short write is a lost record even without an error",
			w:        &flakyWriter{shortAt: 1},
			actions:  func(j *Writer) { j.CoreStart(0, "int") },
			wantErrs: 1,
			wantOp:   "short write",
		},
		{
			name:     "sync failure surfaces for durable records only",
			w:        &flakyWriter{syncErr: errors.New("flush refused")},
			actions:  func(j *Writer) { j.CoreStart(0, "int"); j.CoreBeat(0, 5) },
			wantErrs: 1, // core_start is durable, the beat is disposable
			wantOp:   "sync",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			j := NewWriter(tt.w, "run-1")
			tt.actions(j)
			err, n := j.Errors()
			if n != tt.wantErrs {
				t.Errorf("error count = %d, want %d", n, tt.wantErrs)
			}
			if tt.wantErrs == 0 {
				if err != nil {
					t.Errorf("Errors() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("first error lost")
			}
			if !strings.Contains(err.Error(), tt.wantOp) {
				t.Errorf("first error = %q, want it to mention %q", err, tt.wantOp)
			}
		})
	}
}

// The run_end line must admit its own writer's failures: a reader of
// the raw journal sees the hole count, and recovery turns it into a
// loud warning instead of a clean story.
func TestRunEndCarriesJournalErrors(t *testing.T) {
	fw := &flakyWriter{failAt: map[int]bool{3: true}} // only the 3rd write (core_done) fails; run_end still lands
	j := NewWriter(fw, "run-9")
	j.RunStart("general", "", []int{0}, 1, "1s", false) // write 1 ok
	j.CoreBeat(0, 5)                                    // write 2 ok
	j.CoreDoneElapsed(0, OK, "", time.Second)           // write 3 fails
	j.RunEnd(0)                                         // write 4 ok, carries the count
	if _, n := j.Errors(); n != 1 {
		t.Fatalf("Errors count = %d, want 1", n)
	}
	recs := Parse(bytes.NewReader(fw.buf.Bytes()))
	var end *Record
	for i := range recs {
		if recs[i].Type == RunEnd {
			end = &recs[i]
		}
	}
	if end == nil {
		t.Fatal("run_end should have landed")
	}
	if end.JournalErrs != 1 {
		t.Errorf("run_end JournalErrs = %d, want 1", end.JournalErrs)
	}
}

// Recovery + report: a run whose journal admitted failures prints a
// loud warning, and an old journal without the field stays "0 = not
// measured" rather than a claim of integrity.
func TestRecoveredJournalWarnings(t *testing.T) {
	var buf bytes.Buffer
	for _, line := range []string{
		`{"type":"run_start","run_id":"r1","test":"general","ts":"2026-10-16T00:00:00Z"}`,
		`{"type":"core_start","run_id":"r1","core":0,"workload":"int","ts":"2026-10-16T00:00:01Z"}`,
		`{"type":"core_done","run_id":"r1","core":0,"result":"ok","ts":"2026-10-16T00:00:02Z"}`,
		`{"type":"run_end","run_id":"r1","exit":0,"journal_errs":3,"ts":"2026-10-16T00:00:03Z"}`,
	} {
		buf.WriteString(line + "\n")
	}
	rep := RecoverRuns(Parse(bytes.NewReader(buf.Bytes())))
	if len(rep) != 1 || rep[0].JournalErrs != 3 {
		t.Fatalf("journal error count lost in recovery: %+v", rep)
	}
	var out strings.Builder
	PrintReport(&out, rep)
	if !strings.Contains(out.String(), "JOURNAL INCOMPLETE") {
		t.Errorf("report hides the incomplete journal:\n%s", out.String())
	}

	// Old journal without the field: intact as far as we can tell, and
	// the report says nothing about journal errors.
	buf.Reset()
	for _, line := range []string{
		`{"type":"run_start","run_id":"r2","test":"general","ts":"2026-10-16T00:00:00Z"}`,
		`{"type":"run_end","run_id":"r2","exit":0,"ts":"2026-10-16T00:00:03Z"}`,
	} {
		buf.WriteString(line + "\n")
	}
	rep = RecoverRuns(Parse(bytes.NewReader(buf.Bytes())))
	if len(rep) != 1 || rep[0].JournalErrs != 0 {
		t.Fatalf("old journal gained a phantom error count: %+v", rep)
	}
	out.Reset()
	PrintReport(&out, rep)
	if strings.Contains(out.String(), "JOURNAL INCOMPLETE") {
		t.Errorf("old journal warned without evidence:\n%s", out.String())
	}
}

var _ io.Writer = (*flakyWriter)(nil)
