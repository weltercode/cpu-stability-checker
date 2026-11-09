package whea

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cperRecord builds a minimal CPER byte record: 128-byte header, one
// 72-byte section descriptor (SectionType at descriptor+18, exactly
// where the scanner looks), and a section body at offset 200. Layout
// per UEFI App. N as carried in edk2 Cper.h and Linux cper.h.
func cperRecord(t *testing.T, sectionGUID string, severity uint32, procErrorType byte, errorTypeValid bool) []byte {
	t.Helper()
	gb := guidBytes(sectionGUID)
	if gb == nil {
		t.Fatalf("bad test GUID %q", sectionGUID)
	}
	rec := make([]byte, 200+128)
	copy(rec[0:], []byte("CPER"))
	binary.LittleEndian.PutUint16(rec[4:], 0x0100) // revision
	binary.LittleEndian.PutUint32(rec[6:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint16(rec[10:], 1) // section count
	binary.LittleEndian.PutUint32(rec[12:], severity)
	binary.LittleEndian.PutUint32(rec[128:], 200) // descriptor: SectionOffset
	binary.LittleEndian.PutUint32(rec[132:], 128) // descriptor: SectionLength
	copy(rec[146:], gb)                           // descriptor+18: SectionType
	if errorTypeValid {
		rec[200] |= 0x4 // ValidFields (LE64) bit2: ErrorType valid
	}
	rec[200+10] = procErrorType
	return rec
}

func xmlEvent(provider string, eventID, level, recID int, sysTime string, dataHex string) string {
	data := ""
	if dataHex != "" {
		data = fmt.Sprintf("<EventData><Data Name='Length'>328</Data><Data Name='RawData'>%s</Data></EventData>", dataHex)
	}
	ts := ""
	if sysTime != "" {
		ts = fmt.Sprintf("<TimeCreated SystemTime='%s'/>", sysTime)
	}
	return fmt.Sprintf(
		"<Event xmlns='http://schemas.microsoft.com/win/2004/08/events/event'><System>"+
			"<Provider Name='%s'/><EventID>%d</EventID><Version>0</Version><Level>%d</Level>"+
			"%s<EventRecordID>%d</EventRecordID><Channel>System</Channel></System>%s</Event>",
		provider, eventID, level, ts, recID, data)
}

// The decoder must read the machine-readable XML fields (locale-free)
// and classify by documented event IDs and recognized CPER section
// GUIDs — never by the numeric Level, never by guessing at unknown
// vendor sections.
func TestParseAndClassify(t *testing.T) {
	const (
		memGUID     = "a5bc1114-6f64-4ede-b863-3e83ed7c83b1"
		pcieGUID    = "d995e954-bbc1-430f-ad91-b44dcb3c6f35"
		procGenGUID = "9876ccad-47b4-4bdb-b65e-16f193c4f3db"
		procIAGUID  = "dc3ea0b0-a144-4797-b95b-53fa242b6e1d"
		dmarGUID    = "71761d37-32b2-45cd-a7d0-b0fedd93e8cf"
		amdVendor   = "93c5a153-f05e-4ca0-9f43-faab774204c6" // vendor section: NOT recognized on purpose
	)
	memHex := hex.EncodeToString(cperRecord(t, memGUID, 2, 0, false))
	cacheHex := hex.EncodeToString(cperRecord(t, procGenGUID, 1, 0x01, true))
	microHex := hex.EncodeToString(cperRecord(t, procGenGUID, 2, 0x08, true))
	busHex := hex.EncodeToString(cperRecord(t, procGenGUID, 2, 0x04, true))
	pcieHex := hex.EncodeToString(cperRecord(t, pcieGUID, 2, 0, false))
	dmarHex := hex.EncodeToString(cperRecord(t, dmarGUID, 2, 0, false))
	procHex := hex.EncodeToString(cperRecord(t, procIAGUID, 1, 0, false))
	amdHex := hex.EncodeToString(cperRecord(t, amdVendor, 2, 0, false))

	tests := []struct {
		name       string
		xml        string
		wantEvents int
		provider   string
		recID      int
		eventID    int
		timeZero   bool
		severity   string
		nature     string
	}{
		{
			name:       "corrected memory (WHEA-Logger 19 + memory section)",
			xml:        xmlEvent("Microsoft-Windows-WHEA-Logger", 19, 3, 101, "2026-10-06T12:34:56.7890123Z", memHex),
			wantEvents: 1, provider: "Microsoft-Windows-WHEA-Logger", recID: 101, eventID: 19,
			severity: SevCorrected, nature: NatureMemory,
		},
		{
			name:       "fatal cache (WHEA-Logger 17 + processor generic ErrorType=cache)",
			xml:        xmlEvent("Microsoft-Windows-WHEA-Logger", 17, 1, 102, "2026-10-06T12:34:56.7890123Z", cacheHex),
			wantEvents: 1, eventID: 17,
			severity: SevUncorrected, nature: NatureCache,
		},
		{
			name:       "corrected micro-architectural stays cpu",
			xml:        xmlEvent("Microsoft-Windows-WHEA-Logger", 19, 3, 103, "2026-10-06T12:34:56.7890123Z", microHex),
			wantEvents: 1, severity: SevCorrected, nature: NatureCPU,
		},
		{
			name:       "processor generic bus section is interconnect",
			xml:        xmlEvent("Microsoft-Windows-WHEA-Logger", 19, 3, 104, "2026-10-06T12:34:56.7890123Z", busHex),
			wantEvents: 1, severity: SevCorrected, nature: NatureInterconnect,
		},
		{
			name:       "PCIe section is pcie",
			xml:        xmlEvent("Microsoft-Windows-WHEA-Logger", 19, 3, 105, "2026-10-06T12:34:56.7890123Z", pcieHex),
			wantEvents: 1, severity: SevCorrected, nature: NaturePCIe,
		},
		{
			name:       "DMAR section is interconnect",
			xml:        xmlEvent("Microsoft-Windows-WHEA-Logger", 19, 3, 106, "2026-10-06T12:34:56.7890123Z", dmarHex),
			wantEvents: 1, severity: SevCorrected, nature: NatureInterconnect,
		},
		{
			name:       "IA32/X64 processor specific is cpu",
			xml:        xmlEvent("Microsoft-Windows-WHEA-Logger", 17, 1, 107, "2026-10-06T12:34:56.7890123Z", procHex),
			wantEvents: 1, severity: SevUncorrected, nature: NatureCPU,
		},
		{
			name:       "unknown vendor section stays unknown, never guessed",
			xml:        xmlEvent("Microsoft-Windows-WHEA-Logger", 19, 3, 108, "2026-10-06T12:34:56.7890123Z", amdHex),
			wantEvents: 1, severity: SevCorrected, nature: NatureUnknown,
		},
		{
			name:       "no CPER at all: severity from event id, nature unknown",
			xml:        xmlEvent("Microsoft-Windows-WHEA-Logger", 19, 3, 109, "2026-10-06T12:34:56.7890123Z", ""),
			wantEvents: 1, severity: SevCorrected, nature: NatureUnknown,
		},
		{
			name:       "legacy MCE provider: cpu by definition, severity unknown",
			xml:        xmlEvent("Machine Check Exception", 17, 3, 110, "2026-10-06T12:34:56.7890123Z", ""),
			wantEvents: 1, severity: SevUnknown, nature: NatureCPU,
		},
		{
			name:       "undocumented event id: severity unknown",
			xml:        xmlEvent("Microsoft-Windows-WHEA-Logger", 46, 3, 111, "2026-10-06T12:34:56.7890123Z", memHex),
			wantEvents: 1, severity: SevUnknown, nature: NatureMemory,
		},
		{
			name:       "numeric level is evidence, not policy: event 19 with odd level still corrected",
			xml:        xmlEvent("WHEA-Logger", 19, 1, 112, "2026-10-06T12:34:56.7890123Z", ""),
			wantEvents: 1, provider: "WHEA-Logger", severity: SevCorrected, nature: NatureUnknown,
		},
		{
			name:       "garbage RawData is not guessed at",
			xml:        xmlEvent("Microsoft-Windows-WHEA-Logger", 19, 3, 113, "2026-10-06T12:34:56.7890123Z", "zzz-not-hex"),
			wantEvents: 1, severity: SevCorrected, nature: NatureUnknown,
		},
		{
			name:       "missing timestamp keeps the record (RecordID is the identity)",
			xml:        xmlEvent("Microsoft-Windows-WHEA-Logger", 19, 3, 114, "", ""),
			wantEvents: 1, timeZero: true, severity: SevCorrected, nature: NatureUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseWevtutilXML(tt.xml)
			if len(got) != tt.wantEvents {
				t.Fatalf("parsed %d events, want %d", len(got), tt.wantEvents)
			}
			ev := got[0]
			if tt.provider != "" && ev.Provider != tt.provider {
				t.Errorf("provider = %q, want %q", ev.Provider, tt.provider)
			}
			if tt.recID != 0 && ev.RecordID != tt.recID {
				t.Errorf("RecordID = %d, want %d", ev.RecordID, tt.recID)
			}
			if tt.eventID != 0 && ev.EventID != tt.eventID {
				t.Errorf("EventID = %d, want %d", ev.EventID, tt.eventID)
			}
			if tt.timeZero != ev.Time.IsZero() {
				t.Errorf("time zero = %v, want %v", ev.Time.IsZero(), tt.timeZero)
			}
			if !tt.timeZero {
				want := time.Date(2026, 10, 6, 12, 34, 56, 789_012_300, time.UTC)
				if !ev.Time.Equal(want) {
					t.Errorf("time = %v, want %v", ev.Time, want)
				}
			}
			if ev.Severity != tt.severity {
				t.Errorf("severity = %q, want %q", ev.Severity, tt.severity)
			}
			if ev.Nature != tt.nature {
				t.Errorf("nature = %q, want %q", ev.Nature, tt.nature)
			}
			if ev.RawXML == "" || !strings.Contains(ev.RawXML, "<Event") {
				t.Errorf("raw XML evidence missing: %q", firstN(ev.RawXML, 60))
			}
			if ev.ReceivedAt.IsZero() {
				t.Error("ReceivedAt not stamped: late events would lose their arrival time")
			}
		})
	}
}

// Multiple events arrive wrapped in <Events>; a single small query comes
// as a bare <Event>. Two records with identical timestamps but
// different RecordIDs are two events, not one duplicate.
func TestParseWevtutilXMLShapes(t *testing.T) {
	ts := "2026-10-06T12:00:00.0000000Z"
	wrapped := "<Events xmlns='http://schemas.microsoft.com/win/2004/08/events/event'>" +
		xmlEvent("Microsoft-Windows-WHEA-Logger", 19, 3, 201, ts, "") +
		xmlEvent("Microsoft-Windows-WHEA-Logger", 19, 3, 202, ts, "") +
		"</Events>"
	got := parseWevtutilXML(wrapped)
	if len(got) != 2 {
		t.Fatalf("wrapped: parsed %d events, want 2 (same timestamp, different RecordIDs)", len(got))
	}
	if got[0].RecordID != 201 || got[1].RecordID != 202 {
		t.Errorf("RecordIDs = %d, %d, want 201, 202", got[0].RecordID, got[1].RecordID)
	}

	bare := parseWevtutilXML(xmlEvent("Microsoft-Windows-WHEA-Logger", 19, 3, 203, ts, ""))
	if len(bare) != 1 || bare[0].RecordID != 203 {
		t.Fatalf("bare event: %+v", bare)
	}

	// A truncated tail (wevtutil killed mid-write): the complete events
	// before it are kept, the broken one is dropped, nothing panics.
	cut := xmlEvent("Microsoft-Windows-WHEA-Logger", 19, 3, 204, ts, "") +
		"<Event><System><Provider Name='X'<" // torn mid-tag
	got = parseWevtutilXML(cut)
	if len(got) != 1 || got[0].RecordID != 204 {
		t.Fatalf("truncated tail: got %d events, want the one complete record", len(got))
	}

	// Empty output ("no events") decodes to nothing, not an error.
	if got := parseWevtutilXML(""); len(got) != 0 {
		t.Errorf("empty input: %d events, want 0", len(got))
	}
}

// fakeQ is a scripted event log: records with RecordIDs, plus probes
// that can be made to fail, clear or roll over. newest() reports the
// baseline id until the first query happens (records in log arrive
// after the run started), then the log's real high-water mark — so
// delivered records never look like a "log went backwards" epoch.
// It is safe for the poller goroutine and the test to use it at once.
type fakeQ struct {
	mu       sync.Mutex
	log      []Event // WHEA records appended after the baseline
	newestID int     // overall newest id at start
	oldestID int
	failQ    error
	failNew  error
	failOld  error

	queriedCalls int // query() calls so far
	newestCalls  int // newest() calls so far
	clearOnCall  int // if >0, the Nth newest() call reports clearTo (log cleared)
	clearTo      int

	// gate: when non-nil, the FIRST oldest() call parks until the gate
	// closes. It makes "a full cycle is in progress but not done" a
	// stable observation point instead of a 10ms lottery: the first
	// Actions runner proved a test goroutine can be starved long enough
	// that the whole cycle completes before the test's first state
	// read, turning an intermediate-state assertion into a flake.
	// Callers must close the gate (or the call times out and names the
	// deadlock honestly).
	gate     chan struct{}
	gateUsed bool
}

// add appends a record as if the OS had just written it to the log.
func (f *fakeQ) add(evs ...Event) {
	f.mu.Lock()
	f.log = append(f.log, evs...)
	f.mu.Unlock()
}

func (f *fakeQ) query(_ context.Context, after, limit int) ([]Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failQ != nil {
		return nil, f.failQ
	}
	f.queriedCalls++
	var out []Event
	for _, e := range f.log {
		if e.RecordID > after {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeQ) recent(_ context.Context, limit int) ([]Event, error) {
	return f.query(nil, 0, limit)
}

func (f *fakeQ) newest(_ context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNew != nil {
		return 0, f.failNew
	}
	f.newestCalls++
	if f.clearOnCall > 0 && f.newestCalls == f.clearOnCall {
		return f.clearTo, nil
	}
	if f.queriedCalls == 0 {
		return f.newestID, nil // baseline: the log has not grown yet
	}
	m := f.newestID
	for _, e := range f.log {
		if e.RecordID > m {
			m = e.RecordID
		}
	}
	return m, nil
}

func (f *fakeQ) oldest(_ context.Context) (int, error) {
	f.mu.Lock()
	if f.gate != nil && !f.gateUsed {
		f.gateUsed = true
		f.mu.Unlock() // the gate waits OUTSIDE the lock: a parked probe must not freeze the whole fake
		select {
		case <-f.gate:
		case <-time.After(10 * time.Second):
			return 0, errors.New("fakeQ: oldest gate never released (test bug)")
		}
		f.mu.Lock()
	}
	defer f.mu.Unlock()
	return f.oldestID, f.failOld
}

func rec(id int) Event { return Event{RecordID: id, Provider: "WHEA"} }

// The cursor baseline is the newest record at start: pre-existing
// records are not this run's story. Paging must drain more than one
// page worth of records, gaps between WHEA IDs are normal, and the
// state must tell the truth about failures, clears, rollovers and
// drops — never silently.
func TestMonitorCursor(t *testing.T) {
	tests := []struct {
		name    string
		fq      *fakeQ
		consume int // how many events to read from the channel (-1 = all expected)
		want    []int
		state   func(State) bool
		reason  string
	}{
		{
			name: "baseline hides pre-run records, delivers new ones",
			fq:   &fakeQ{newestID: 100, oldestID: 50, log: []Event{rec(60), rec(101), rec(102)}},
			want: []int{101, 102},
			state: func(s State) bool {
				return s.Available && !s.WindowDegraded && s.Cursor == 102 && s.Epochs == 0 && s.Gaps == 0
			},
		},
		{
			name: "normal ID gaps between WHEA records are not loss",
			fq:   &fakeQ{newestID: 100, oldestID: 100, log: []Event{rec(101), rec(500), rec(900)}},
			want: []int{101, 500, 900},
			state: func(s State) bool {
				return s.Available && !s.WindowDegraded && s.Gaps == 0
			},
		},
		{
			name: "more than one page is paged out fully",
			fq: func() *fakeQ {
				log := make([]Event, 0, 120)
				for i := 101; i <= 220; i++ {
					log = append(log, rec(i))
				}
				return &fakeQ{newestID: 100, oldestID: 100, log: log}
			}(),
			want: nil, consume: 120,
			state: func(s State) bool { return s.Cursor == 220 && !s.WindowDegraded },
		},
		{
			name: "log clear is an epoch, not a clean zero",
			fq: &fakeQ{newestID: 100, oldestID: 100, log: []Event{rec(101)},
				clearOnCall: 3, clearTo: 5},
			consume: 1, // 101 lands before the clear is noticed
			state:   func(s State) bool { return s.Epochs >= 1 && s.WindowDegraded },
			reason:  "cleared",
		},
		{
			name:   "rollover past the cursor is a suspected gap",
			fq:     &fakeQ{newestID: 100, oldestID: 999, log: nil},
			state:  func(s State) bool { return s.Gaps >= 1 && s.WindowDegraded },
			reason: "rolled over",
		},
		{
			name: "query failure degrades, recovery does not unmark the window",
			fq:   &fakeQ{newestID: 100, oldestID: 100, failQ: errors.New("wevtutil exploded")},
			state: func(s State) bool {
				return !s.Available && s.WindowDegraded && s.Failures >= 1
			},
			reason: "wevtutil exploded",
		},
		{
			name: "probe failure degrades",
			fq:   &fakeQ{failNew: errors.New("access denied")},
			state: func(s State) bool {
				return !s.Available && s.WindowDegraded && s.Failures >= 1
			},
			reason: "access denied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Monitor{ch: make(chan Event, 256), stop: make(chan struct{})}
			go m.poll(tt.fq, 10*time.Millisecond, -1)

			var got []int
			wantN := len(tt.want)
			if tt.consume > 0 {
				wantN = tt.consume
			}
			deadline := time.After(2 * time.Second)
			for len(got) < wantN {
				select {
				case ev, ok := <-m.Events():
					if !ok {
						t.Fatalf("channel closed after %d events, wanted %d", len(got), wantN)
					}
					got = append(got, ev.RecordID)
				case <-deadline:
					t.Fatalf("only %d events delivered, wanted %d (state: %+v)", len(got), wantN, m.State())
				}
			}
			if len(tt.want) > 0 {
				for i, id := range tt.want {
					if got[i] != id {
						t.Errorf("event %d = %d, want %d", i, got[i], id)
					}
				}
				select {
				case ev := <-m.Events():
					t.Fatalf("extra event %d delivered", ev.RecordID)
				case <-time.After(100 * time.Millisecond):
				}
			}

			// State conditions (epochs, gaps, failures) settle on later
			// ticks: wait for the expectation instead of racing it.
			if tt.state != nil {
				deadline := time.After(2 * time.Second)
				for !tt.state(m.State()) {
					select {
					case <-time.After(20 * time.Millisecond):
					case <-deadline:
						t.Fatalf("state never matched: %+v", m.State())
					}
				}
			}
			if tt.reason != "" {
				st := m.State()
				if !strings.Contains(st.WindowReason, tt.reason) {
					t.Errorf("WindowReason = %q, want it to mention %q", st.WindowReason, tt.reason)
				}
			}
			m.Close()
		})
	}
}

// A lagging consumer can no longer lose records (delivery applies
// backpressure), and Close must not lose late ones either: the final
// bounded cycle folds a late-published record into the stream before
// the channel closes, so the engine can still weigh it into the
// run's verdict instead of losing it.
func TestMonitorFinalDrain(t *testing.T) {
	fq := &fakeQ{newestID: 0, oldestID: 0}
	m := &Monitor{ch: make(chan Event, 16), stop: make(chan struct{})}
	go m.poll(fq, time.Hour, -1) // no ticks will fire: only the final drain can deliver

	// Let the baseline cycle fix the cursor, then the OS writes a
	// "late" record, then the run ends.
	time.Sleep(50 * time.Millisecond)
	fq.add(rec(42))
	m.Close()

	deadline := time.After(2 * time.Second)
	var got []int
	for {
		select {
		case ev, open := <-m.Events():
			if !open {
				if len(got) != 1 || got[0] != 42 {
					t.Fatalf("final drain delivered %v, want exactly [42] before close", got)
				}
				return
			}
			got = append(got, ev.RecordID)
		case <-deadline:
			t.Fatalf("final drain delivered %v, want [42] before close", got)
		}
	}
}

// The journal is the durability gate: an event is acknowledged (cursor
// advanced, consumer notified) only after persist succeeds. A persist
// failure holds the cursor, admits the failure NOW, and the next cycle
// retries the same record — delivered exactly once once it works.
func TestMonitorPersistRetry(t *testing.T) {
	fq := &fakeQ{newestID: 0, oldestID: 0, log: []Event{rec(7)}}
	var failures int32 = 1 // first persist attempt fails
	var persisted []int
	m := &Monitor{ch: make(chan Event, 16), stop: make(chan struct{}),
		persist: func(ev Event) error {
			if atomic.AddInt32(&failures, -1) >= 0 {
				return errors.New("disk full")
			}
			persisted = append(persisted, ev.RecordID)
			return nil
		}}
	go m.poll(fq, 10*time.Millisecond, -1)

	select {
	case ev := <-m.Events():
		if ev.RecordID != 7 {
			t.Fatalf("delivered record %d, want the retried record 7", ev.RecordID)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("retried event never delivered: state %+v", m.State())
	}
	select {
	case ev := <-m.Events():
		t.Fatalf("record %d delivered twice, want exactly-once after retry", ev.RecordID)
	case <-time.After(150 * time.Millisecond):
	}

	st := m.State()
	if st.Cursor != 7 {
		t.Errorf("cursor = %d, want 7 only after the successful persist", st.Cursor)
	}
	if st.Failures < 1 || !st.WindowDegraded || !strings.Contains(st.WindowReason, "disk full") {
		t.Errorf("persist failure not admitted in state: %+v", st)
	}
	if len(persisted) != 1 || persisted[0] != 7 {
		t.Errorf("persisted = %v, want exactly [7]", persisted)
	}
	m.Close()
}

// A nil Monitor (no WHEA on the platform) must report itself
// unavailable: its silence is "not observed", never "no errors".
func TestNilMonitorState(t *testing.T) {
	var m *Monitor
	if st := m.State(); st.Available || !st.WindowDegraded || st.WindowReason == "" {
		t.Errorf("nil monitor state = %+v, want unavailable with reason", st)
	}
	if m.Events() != nil {
		t.Error("nil monitor must return a nil channel")
	}
	m.Close() // nil-safe
}

// R21: the shutdown witness rides the same decoder, so the same
// machine-readable envelope is decoded for Kernel-Power 41 and EventLog
// 6008 records - and classify() must NOT smuggle WHEA severity or nature
// into them: these records name the end of a boot, not a hardware error,
// and their embedded fields (BugcheckCode numbers, localized time strings)
// are kept as raw evidence exactly as written. An interpreter-free decoder
// is what lets the CLI witness stay free of diagnosis.
func TestShutdownRecordsDecodeAsEvidence(t *testing.T) {
	fixture := "<Events><Event>" +
		"<System><Provider Name='Microsoft-Windows-Kernel-Power'/><EventID>41</EventID><Version>0</Version><Level>1</Level>" +
		"<TimeCreated SystemTime='2026-11-09T19:44:12.010000000Z'/><EventRecordID>220001</EventRecordID><Channel>System</Channel></System>" +
		"<EventData><Data Name='BugcheckCode'>0</Data><Data Name='BugcheckParameter1'>0x0</Data><Data Name='PowerButtonTimestamp'>0</Data></EventData></Event>" +
		"<Event>" +
		"<System><Provider Name='EventLog'/><EventID>6008</EventID><Version>1</Version><Level>1</Level>" +
		"<TimeCreated SystemTime='2026-11-09T19:44:31.000000000Z'/><EventRecordID>220009</EventRecordID><Channel>System</Channel></System>" +
		"<EventData><Data>2026-11-09</Data><Data>22:41:07</Data><Data>Microsoft Windows 11</Data></EventData></Event>" +
		"</Events>"
	evs := parseWevtutilXML(fixture)
	if len(evs) != 2 {
		t.Fatalf("decoded %d events, want 2", len(evs))
	}
	kp := evs[0]
	if kp.Provider != "Microsoft-Windows-Kernel-Power" || kp.EventID != 41 || kp.RecordID != 220001 {
		t.Errorf("envelope decoded wrong: %+v", kp)
	}
	if kp.Time.UTC().Format(time.RFC3339) != "2026-11-09T19:44:12Z" {
		t.Errorf("TimeCreated not parsed as the UTC write-time of the later boot: %v", kp.Time)
	}
	if kp.Severity != SevUnknown || kp.Nature != NatureUnknown {
		t.Errorf("classify gave a shutdown record WHEA-style meaning: sev=%q nature=%q", kp.Severity, kp.Nature)
	}
	var bugcheck *DataField
	for i := range kp.Data {
		if kp.Data[i].Name == "BugcheckCode" {
			bugcheck = &kp.Data[i]
		}
	}
	if bugcheck == nil || bugcheck.Value != "0" {
		t.Errorf("BugcheckCode not kept as-recorded evidence: %+v", kp.Data)
	}
	el := evs[1]
	if el.Provider != "EventLog" || el.EventID != 6008 || el.RecordID != 220009 {
		t.Errorf("envelope decoded wrong: %+v", el)
	}
	if el.Severity != SevUnknown || el.Nature != NatureUnknown {
		t.Errorf("classify gave a shutdown record WHEA-style meaning: sev=%q nature=%q", el.Severity, el.Nature)
	}
	// The localized strings stay un-named evidence, values verbatim: the
	// decoder never re-dates anything from prose shaped like a time.
	if len(el.Data) != 3 || el.Data[1].Name != "" || el.Data[1].Value != "22:41:07" {
		t.Errorf("6008 embedded strings not kept verbatim as evidence: %+v", el.Data)
	}
	if _, perr := time.Parse(time.RFC3339, el.Data[1].Value); perr == nil {
		t.Errorf("a 6008 embedded string parses as a timestamp: the decoder must not invite re-dating")
	}
}
