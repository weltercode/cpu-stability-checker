// Package whea watches the Windows Hardware Error Architecture records
// that the OS writes to the System event log: corrected machine checks,
// uncorrected errors and the like.
//
// A WHEA record is direct evidence of a hardware problem — unlike a
// workload mismatch it comes from the platform itself. It is also
// system-wide: we cannot honestly attribute it to one core (a CPER APIC
// ID is not our logical CPU numbering), so it is reported at run level.
//
// The events are read with wevtutil (present on every supported Windows)
// in /f:xml form: the XML carries machine-readable fields (provider,
// EventRecordID, EventID, numeric level, EventData) that do not depend
// on the Windows display language, unlike the old /f:text rendering
// whose field names and level strings were localized. Classification
// follows the nature of the error per the WHEA/CPER documentation; an
// unrecognized record stays raw evidence with classification unknown —
// never a guess and never a silent pass. On other platforms every
// entry point degrades to a no-op, like affinity.
package whea

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// DataField is one EventData name/value pair, kept in document order.
// Values are stored as the event log gives them: we interpret only what
// is documented, the rest stays evidence.
type DataField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Event is one WHEA-related event log record, decoded from wevtutil
// /f:xml. Everything policy-relevant is numeric or an English constant:
// the localized description text is never parsed, so classification
// cannot drift with the Windows display language.
type Event struct {
	Time     time.Time // event log SystemTime (UTC); zero when unparseable
	Provider string
	Channel  string
	RecordID int // EventRecordID: the identity the read cursor pages by
	EventID  int
	Level    int // raw event log level number; kept as evidence, never policy
	Data     []DataField
	RawXML   string // the exact XML we decoded (raw evidence for adjudication)

	// ReceivedAt is when this decoder got the record. WHEA events can be
	// published late: Time says when the hardware error happened,
	// ReceivedAt says when we learned about it. Both are kept so a late
	// record is never silently re-dated into (or out of) a phase.
	ReceivedAt time.Time

	Nature   string // cpu, cache, interconnect, memory, pcie, unknown
	Severity string // corrected, uncorrected, unknown
}

// Summary is the one-line human statement of the record: classification
// first, provenance second. It never depends on localized text.
func (ev Event) Summary() string {
	return fmt.Sprintf("WHEA %s %s (provider %s, event %d, record %d)",
		ev.Severity, ev.Nature, ev.Provider, ev.EventID, ev.RecordID)
}

// Classification vocabularies (plan P02): the nature of the failing
// component and the severity of the error. Unknown is a real answer
// that demands adjudication, never a pass and never a guess.
const (
	NatureCPU          = "cpu"
	NatureCache        = "cache"
	NatureInterconnect = "interconnect"
	NatureMemory       = "memory"
	NaturePCIe         = "pcie"
	NatureUnknown      = "unknown"

	SevCorrected   = "corrected"
	SevUncorrected = "uncorrected"
	SevUnknown     = "unknown"
)

const (
	pollInterval = 5 * time.Second
	queryLimit   = 50
	// finalDrainTimeout bounds the last read after Close: late-published
	// records get one more chance, but the run's verdict never waits on
	// an unbounded query.
	finalDrainTimeout = 5 * time.Second
)

// querier fetches event log records in three shapes: WHEA-provider
// records after a cursor (oldest first, bounded), and the overall
// newest/oldest EventRecordID probes that let us notice the log being
// cleared or rolled over without assuming anything about gaps between
// WHEA records (other providers live between them).
//
// Every method takes a context: the final drain must be bounded, and
// a wevtutil call killed mid-output still yields the complete events
// it managed to print (the decoder keeps partial batches).
type querier interface {
	query(ctx context.Context, after, limit int) ([]Event, error)
	recent(ctx context.Context, limit int) ([]Event, error) // newest first, for the 'whea' command
	newest(ctx context.Context) (int, error)
	oldest(ctx context.Context) (int, error)
}

// State is what the Monitor admits about its own observation. A
// "WHEA 0" result only means something while observation was
// available: every failure, rollover or epoch change is counted
// here instead of being swallowed. Delivery uses backpressure, so
// records are never lost to a lagging consumer.
type State struct {
	Available bool   // the last cycle succeeded (wevtutil present)
	Reason    string // why it is currently degraded/unavailable ("" = clean)
	// Proven: at least one FULL query cycle (newest probe + oldest
	// probe + cursor scan) completed successfully. "No WHEA records
	// during the run" may only be claimed for a proven window; a
	// window that only ever answered the baseline probe - or failed
	// every query - is unproven coverage, never a clean bill.
	Proven bool
	// WindowDegraded is sticky: once anything went wrong during this
	// window, the window's observation is permanently suspect even if
	// the current cycle recovered. WindowReason keeps the first cause.
	WindowDegraded bool
	WindowReason   string
	Cursor         int   // high-water EventRecordID already processed
	Queries        int64 // successful query cycles
	Failures       int64 // failed queries/probes
	Epochs         int64 // log clear/epoch transitions seen
	Gaps           int64 // suspected rollover gaps (records evicted unseen)
}

// Monitor delivers WHEA events that appeared after it was started.
type Monitor struct {
	ch   chan Event
	stop chan struct{}
	once sync.Once

	// persist, when set, is the durability gate: an event is only
	// acknowledged (cursor advanced, consumer notified) after persist
	// succeeds. The journal is the authoritative record; a persist
	// failure keeps the cursor put so the next cycle retries it.
	persist func(Event) error

	mu sync.Mutex // guards st (poller writes, anyone reads)
	st State
}

// State returns a snapshot of the observation state. A nil Monitor —
// the platform has no WHEA at all — reports itself unavailable: zero
// events from a nil Monitor is "not observed", never "no errors".
func (m *Monitor) State() State {
	if m == nil {
		return State{Reason: "WHEA is not supported on this platform", WindowDegraded: true, WindowReason: "WHEA is not supported on this platform"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st
}

func (m *Monitor) noteFailure(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st.Failures++
	m.st.Available = false
	m.st.Reason = reason
	if !m.st.WindowDegraded {
		m.st.WindowDegraded = true
		m.st.WindowReason = reason
	}
}

func (m *Monitor) noteEvent(kind string, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch kind {
	case "epoch":
		m.st.Epochs++
	case "gap":
		m.st.Gaps++
	}
	m.st.Reason = reason
	if !m.st.WindowDegraded {
		m.st.WindowDegraded = true
		m.st.WindowReason = reason
	}
}

func (m *Monitor) noteSuccess(cursor int) {
	m.noteCycle(cursor, true)
}

// noteBaseline is the start-of-window newest-record probe succeeding:
// the channel answers, so availability is real, but no cursor scan
// has completed yet - the window's coverage (Proven) is not claimed
// until a full cycle has finished.
func (m *Monitor) noteBaseline(cursor int) {
	m.noteCycle(cursor, false)
}

func (m *Monitor) noteCycle(cursor int, fullCycle bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st.Queries++
	m.st.Available = true
	m.st.Reason = ""
	m.st.Cursor = cursor
	if fullCycle {
		m.st.Proven = true
	}
}

// Start begins polling. It returns nil when the platform has no WHEA
// support; every method is nil-safe and Events() on a nil Monitor returns
// a nil channel, which simply never fires inside a select. controlCore
// confines the polling thread to one CPU (-1 = leave it floating) so the
// poller never preempts a measured core. persist (may be nil) must durably
// keep each event before it is acknowledged: the journal is the
// authoritative record, and a record the journal could not keep keeps
// its cursor slot and is retried.
func Start(controlCore int, persist func(Event) error) *Monitor {
	q, ok := newQuerier()
	if !ok {
		return nil
	}
	m := &Monitor{ch: make(chan Event, 16), stop: make(chan struct{}), persist: persist}
	go m.poll(q, pollInterval, controlCore)
	return m
}

// Events returns the delivery channel (closed when the Monitor closes).
func (m *Monitor) Events() <-chan Event {
	if m == nil {
		return nil
	}
	return m.ch
}

// Close stops polling.
func (m *Monitor) Close() {
	if m == nil {
		return
	}
	m.once.Do(func() { close(m.stop) })
}

// poll pages the event log forward from a RecordID cursor. The cursor
// baseline is the overall newest record at start, so only records
// appended during the run are delivered; RecordID gaps between WHEA
// records are normal (other providers live in between) and are NOT
// treated as loss. What IS reported: the log being cleared (cursor
// above the newest surviving record), rolling over (the oldest
// surviving record jumped past our cursor unseen) and query
// failures — all counted in State, never swallowed. Delivery applies
// backpressure instead of dropping, and Close triggers one bounded
// final cycle so late-published records still reach the journal.
func (m *Monitor) poll(q querier, interval time.Duration, controlCore int) {
	defer close(m.ch)
	if controlCore >= 0 {
		// Confine the poller thread like the runner's helpers: locked
		// without unlocking, the thread dies with this goroutine.
		runtime.LockOSThread()
		if err := sysinfo.SetThreadAffinity(controlCore); err != nil {
			fmt.Fprintf(os.Stderr, "warning: whea poller pinning: %v\n", err)
		}
	}

	const maxPagesPerTick = 10 // bounded work per tick; the rest comes next tick
	cursor := 0
	baseline := false

	deliver := func(ev Event) {
		// Backpressure, not loss: both engines drain this channel
		// until it closes, so a full buffer stalls the poller (its
		// queries pause) instead of dropping records. Memory stays
		// bounded by the channel; nothing is silently lost.
		m.ch <- ev
	}

	cycle := func(ctx context.Context) {
		h, err := q.newest(ctx)
		if err != nil {
			m.noteFailure(fmt.Sprintf("newest-record probe: %v", err))
			return
		}
		if !baseline {
			// Everything at or below the baseline predates this run.
			cursor, baseline = h, true
			m.noteBaseline(cursor)
			return
		}
		if h < cursor {
			// The log was cleared (or shrunk) under us: records between
			// the old baseline and now are unrecoverable. Say so; never
			// pretend the window was fully observed.
			m.noteEvent("epoch", fmt.Sprintf("System log was cleared: newest record %d is below our cursor %d, events in the gap are unrecoverable", h, cursor))
			cursor = 0
		}
		if o, oerr := q.oldest(ctx); oerr != nil {
			m.noteFailure(fmt.Sprintf("oldest-record probe: %v", oerr))
		} else if o > cursor+1 {
			// Records the cursor never passed were evicted unseen. They
			// may or may not have been WHEA records — we report a
			// suspected gap, never an inferred loss count.
			m.noteEvent("gap", fmt.Sprintf("event log rolled over: records below id %d are no longer retrievable (cursor was %d)", o, cursor))
		}
		for page := 0; page < maxPagesPerTick; page++ {
			events, err := q.query(ctx, cursor, queryLimit)
			if err != nil {
				m.noteFailure(fmt.Sprintf("cursor query: %v", err))
				return
			}
			if len(events) == 0 {
				break
			}
			for _, ev := range events {
				if m.persist != nil {
					if perr := m.persist(ev); perr != nil {
						// The journal could not keep this record: the
						// cursor stays put, the next cycle retries it,
						// and the window is marked degraded NOW rather
						// than discovered as a hole later.
						m.noteFailure(fmt.Sprintf("journal persist failed for record %d: %v (cursor held, will retry)", ev.RecordID, perr))
						return
					}
				}
				if ev.RecordID > cursor {
					cursor = ev.RecordID
				}
				deliver(ev)
			}
			if len(events) < queryLimit {
				break // exhausted up to the observed high-water mark
			}
		}
		m.noteSuccess(cursor)
	}

	cycle(context.Background()) // catch events from the very first seconds of the run
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-m.stop:
			// Bounded final read: WHEA records can be published late.
			// One last deadline-bounded cycle gives them a chance to
			// reach the journal before the run's verdict is written;
			// a wevtutil call killed mid-output still yields the
			// complete events it printed. Completeness is never
			// promised — the state and the late flags carry the doubt.
			ctx, cancel := context.WithTimeout(context.Background(), finalDrainTimeout)
			cycle(ctx)
			cancel()
			return
		case <-tick.C:
			cycle(context.Background())
		}
	}
}

// QueryRecent returns the newest WHEA events regardless of when they
// happened (for the 'whea' command).
func QueryRecent(limit int) ([]Event, error) {
	q, ok := newQuerier()
	if !ok {
		return nil, errUnsupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return q.recent(ctx, limit)
}

// eventXML mirrors the wevtutil /f:xml shape. No namespace is pinned:
// the events/event schema is matched by local name, so the decoder
// tolerates the xmlns attribute and any wrapping <Events> element.
type eventXML struct {
	System struct {
		Provider struct {
			Name string `xml:"Name,attr"`
		} `xml:"Provider"`
		EventID       int    `xml:"EventID"`
		Level         int    `xml:"Level"`
		Channel       string `xml:"Channel"`
		EventRecordID int    `xml:"EventRecordID"`
		TimeCreated   struct {
			SystemTime string `xml:"SystemTime,attr"`
		} `xml:"TimeCreated"`
	} `xml:"System"`
	EventData struct {
		Data []struct {
			Name  string `xml:"Name,attr"`
			Value string `xml:",chardata"`
		} `xml:"Data"`
	} `xml:"EventData"`
}

// parseWevtutilXML decodes the /f:xml output of wevtutil qe: either one
// bare <Event> element (small queries) or an <Events> wrapper holding
// many. A truncated or malformed tail stops decoding, but everything
// decoded before it is kept: a half-read batch is still evidence.
// Events without a parsable timestamp are kept too — the RecordID, not
// the clock, is the identity now.
func parseWevtutilXML(text string) []Event {
	dec := xml.NewDecoder(strings.NewReader(text))
	dec.Strict = false
	var out []Event
	prev := dec.InputOffset() // start of the next token in the stream
	for {
		tok, err := dec.Token()
		if err != nil {
			break // io.EOF or a broken tail: keep what we have
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Event" {
			prev = dec.InputOffset()
			continue
		}
		start := prev
		var xe eventXML
		if err := dec.DecodeElement(&xe, &se); err != nil {
			prev = dec.InputOffset()
			continue // undecodable event: skip it, the batch keeps the rest
		}
		end := dec.InputOffset()
		ev := Event{
			Provider:   xe.System.Provider.Name,
			Channel:    xe.System.Channel,
			RecordID:   xe.System.EventRecordID,
			EventID:    xe.System.EventID,
			Level:      xe.System.Level,
			RawXML:     text[start:end],
			ReceivedAt: time.Now().UTC(),
		}
		if ts, terr := time.Parse(time.RFC3339Nano, xe.System.TimeCreated.SystemTime); terr == nil {
			ev.Time = ts
		}
		for _, d := range xe.EventData.Data {
			ev.Data = append(ev.Data, DataField{Name: d.Name, Value: strings.TrimSpace(d.Value)})
		}
		classify(&ev)
		out = append(out, ev)
	}
	return out
}

// classify assigns severity and nature from documented facts only:
// the WHEA-Logger event IDs (17/18 fatal, 19 corrected) and recognized
// CPER section type GUIDs inside the raw record. The numeric Level is
// deliberately NOT used: it is not the hardware severity, and the old
// policy of keying on the localized "Critical" text is exactly what
// this replaces. Anything unrecognized stays unknown and goes to
// adjudication — an honest "we could not tell", never a pass.
func classify(ev *Event) {
	switch ev.Provider {
	case "Microsoft-Windows-WHEA-Logger", "WHEA-Logger":
		switch ev.EventID {
		case 17, 18:
			ev.Severity = SevUncorrected // documented: "a fatal hardware error has occurred"
		case 19:
			ev.Severity = SevCorrected // documented: "a corrected hardware error has occurred"
		default:
			ev.Severity = SevUnknown
		}
	case "Machine Check Exception":
		// Legacy MCE alerts do not say corrected vs uncorrected.
		ev.Severity = SevUnknown
	default:
		ev.Severity = SevUnknown
	}

	if raw := rawDataBytes(ev.Data); raw != nil {
		ev.Nature = natureFromCPER(raw)
	}
	if ev.Nature == "" {
		if ev.Provider == "Machine Check Exception" {
			ev.Nature = NatureCPU // a machine check is CPU by definition
		} else {
			ev.Nature = NatureUnknown
		}
	}
}

// rawDataBytes extracts the RawData EventData field (the CPER record
// blob) as bytes. wevtutil renders binary fields as hex; anything that
// is not clean hex is left alone — we do not guess at encodings.
func rawDataBytes(data []DataField) []byte {
	for _, d := range data {
		if !strings.EqualFold(d.Name, "RawData") || d.Value == "" {
			continue
		}
		clean := strings.NewReplacer(" ", "", ":", "", "-", "").Replace(d.Value)
		if len(clean)%2 != 0 {
			return nil
		}
		raw, err := hex.DecodeString(clean)
		if err != nil {
			return nil
		}
		return raw
	}
	return nil
}

// sectionGUIDs maps recognized CPER section type GUIDs to the component
// nature they describe. The values are the constants fixed by UEFI
// Appendix N as carried in edk2 (MdePkg/Include/Guid/Cper.h) and the
// Linux kernel (include/linux/cper.h). Vendor-specific sections (for
// example AMD's processor-specific record) are deliberately NOT here:
// an unrecognized GUID stays unknown and goes to adjudication instead
// of being guessed into a category.
var sectionGUIDs = []struct {
	guid   string
	nature string
}{
	{"9876ccad-47b4-4bdb-b65e-16f193c4f3db", NatureCPU}, // processor generic (refined below)
	{"dc3ea0b0-a144-4797-b95b-53fa242b6e1d", NatureCPU}, // processor specific IA32/X64
	{"e19e3d16-bc11-11e4-9caa-c2051d5d46b0", NatureCPU}, // processor specific ARM
	{"a5bc1114-6f64-4ede-b863-3e83ed7c83b1", NatureMemory},
	{"61ec04fc-48e6-d813-25c9-8daa44750b12", NatureMemory}, // memory error v2
	{"d995e954-bbc1-430f-ad91-b44dcb3c6f35", NaturePCIe},
	{"c5753963-3b84-4095-bf78-eddad3f9c9dd", NaturePCIe},         // PCI/PCIX bus
	{"eb5e4685-ca66-4769-b6a2-26068b001326", NaturePCIe},         // PCI device
	{"5b51fef7-c79d-4434-8f1b-aa62de3e2c64", NatureInterconnect}, // DMAR generic
	{"71761d37-32b2-45cd-a7d0-b0fedd93e8cf", NatureInterconnect}, // DMAR (VT-d)
	{"036f84e1-7f37-428c-a79e-575fdfaa84ec", NatureInterconnect}, // DMAR (IOMMU)
	{"81212a96-09ed-4996-9471-8d729c8e69ed", NatureUnknown},      // firmware record ref: stays unknown
}

// guidBytes converts a GUID string to the mixed-endian byte order used
// inside CPER records (first three fields little-endian, the rest big).
func guidBytes(guid string) []byte {
	parts := strings.Split(guid, "-")
	if len(parts) != 5 {
		return nil
	}
	b := make([]byte, 0, 16)
	for i, p := range parts[:3] {
		v, err := strconv.ParseUint(p, 16, 32)
		if err != nil {
			return nil
		}
		if i == 0 {
			var b4 [4]byte
			binary.LittleEndian.PutUint32(b4[:], uint32(v))
			b = append(b, b4[:]...)
		} else {
			var b2 [2]byte
			binary.LittleEndian.PutUint16(b2[:], uint16(v))
			b = append(b, b2[:]...)
		}
	}
	for _, p := range []string{parts[3], parts[4]} {
		for i := 0; i+1 < len(p); i += 2 {
			v, err := strconv.ParseUint(p[i:i+2], 16, 8)
			if err != nil {
				return nil
			}
			b = append(b, byte(v))
		}
	}
	if len(b) != 16 {
		return nil
	}
	return b
}

// naturePriority orders natures so a record with several sections
// reports the most central failing component (a CPU error matters more
// for our purpose than a PCIe error logged alongside it).
var naturePriority = map[string]int{
	NatureCPU: 5, NatureCache: 4, NatureInterconnect: 3,
	NatureMemory: 2, NaturePCIe: 1, NatureUnknown: 0,
}

// natureFromCPER scans the raw CPER record for recognized section type
// GUIDs. Scanning (instead of trusting header offsets) is deliberate:
// CPER header layouts drift across revisions, but a section type GUID
// is what it is wherever it appears. The APIC IDs inside the record are
// never read: they are not our logical CPU numbering.
func natureFromCPER(raw []byte) string {
	if len(raw) < 128 || !bytes.HasPrefix(raw, []byte("CPER")) {
		return ""
	}
	best := ""
	for _, s := range sectionGUIDs {
		pat := guidBytes(s.guid)
		if pat == nil {
			continue
		}
		for off := 0; ; {
			i := bytes.Index(raw[off:], pat)
			if i < 0 {
				break
			}
			i += off
			off = i + 1
			n := s.nature
			if s.guid == "9876ccad-47b4-4bdb-b65e-16f193c4f3db" {
				n = refineProcGeneric(raw, i-18) // SectionType sits at descriptor+18
			}
			if naturePriority[n] > naturePriority[best] {
				best = n
			}
		}
	}
	return best
}

// refineProcGeneric reads the ErrorType of a Processor Generic section
// (cache / TLB / bus / micro-architectural) from its body, so a cache
// error is reported as cache, not as a generic CPU error. Any field we
// cannot read bounds-checks back to plain cpu: the section GUID already
// proved it is a processor error.
func refineProcGeneric(raw []byte, descOff int) string {
	const procErrorTypeValid = 0x4 // UEFI N.2.4.1 validation bit
	if descOff < 0 || descOff+8 > len(raw) {
		return NatureCPU
	}
	body := int(binary.LittleEndian.Uint32(raw[descOff:])) // SectionOffset
	if body < 0 || body+11 >= len(raw) {
		return NatureCPU
	}
	if binary.LittleEndian.Uint64(raw[body:])&procErrorTypeValid == 0 {
		return NatureCPU // ErrorType not marked valid: stay at cpu
	}
	switch raw[body+10] { // proc_error_type
	case 0x01: // cache
		return NatureCache
	case 0x02: // TLB (on-die translation structures: cache class)
		return NatureCache
	case 0x04: // bus (interconnect)
		return NatureInterconnect
	case 0x08: // micro-architectural
		return NatureCPU
	default: // unknown or not specified: processor error stands
		return NatureCPU
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
