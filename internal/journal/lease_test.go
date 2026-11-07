package journal

import (
	"bytes"
	"strings"
	"testing"
)

// P07e: session_lease records must survive recovery as the parent's
// per-slot lease view (block, generation, ack state, counts as
// reported), and the report must carry the lease summary with its
// upper-bound interval label. The payload is checked field by field:
// a lease story that loses which block at which generation failed
// cannot support the reproduction claim.
func TestRecoverLeaseRecords(t *testing.T) {
	var buf bytes.Buffer
	jw := NewWriter(&buf, "lease-run")
	jw.RunStart("handoff", "", []int{0, 1}, 2, "", true)
	jw.CoreStart(0, "ram-handoff")
	jw.CoreStart(1, "ram-handoff")
	jw.CoreDone(0, "ok", "")
	jw.CoreDone(1, "fault", "lease-read fault")

	peer := 1
	jw.SessionLease(0, LeaseObservation{Session: "s0", Seq: 1, Block: 0, Gen: 1, Mode: "lease-write", PeerCore: &peer, ToAckMS: 1.5, Acked: true})
	ck := uint64(16384)
	jw.SessionLease(1, LeaseObservation{Session: "s1", Seq: 2, Block: 0, Gen: 1, Mode: "lease-read", PeerCore: &peer, ToAckMS: 2.5, Acked: true, Checks: &ck})
	peer0 := 0
	jw.SessionLease(1, LeaseObservation{Session: "s1", Seq: 3, Block: 1, Gen: 2, Mode: "lease-read", PeerCore: &peer0, ToAckMS: 3.5, Acked: false, Error: "lease-read fault"})
	jw.RunEndValidation(1, "not_performed")
	jw.Close()

	recs, iss := ParseAll(bytes.NewReader(buf.Bytes()), "lease")
	if iss.ReadErr != "" {
		t.Fatalf("parse: %s", iss.ReadErr)
	}
	reports := RecoverRuns(recs)
	if len(reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(reports))
	}
	byCore := map[int][]LeaseObservation{}
	for _, c := range reports[0].Cores {
		byCore[c.Core] = c.LeaseObs
	}
	if len(byCore[0]) != 1 || len(byCore[1]) != 2 {
		t.Fatalf("lease records per core = %d/%d, want 1/2", len(byCore[0]), len(byCore[1]))
	}
	w := byCore[0][0]
	if w.Block != 0 || w.Gen != 1 || w.Mode != "lease-write" || !w.Acked || w.Error != "" {
		t.Errorf("write lease payload = %+v", w)
	}
	if w.PeerCore == nil || *w.PeerCore != 1 {
		t.Errorf("write lease peer core = %v, want 1", w.PeerCore)
	}
	ok := byCore[1][0]
	if ok.Checks == nil || *ok.Checks != 16384 || !ok.Acked {
		t.Errorf("acked read lease payload = %+v, want checks 16384 acked", ok)
	}
	bad := byCore[1][1]
	if bad.Block != 1 || bad.Gen != 2 || bad.Acked || bad.Error != "lease-read fault" || bad.Checks != nil {
		t.Errorf("failed read lease payload = %+v (counts must stay nil=unknown, never zero)", bad)
	}

	var out bytes.Buffer
	PrintReport(&out, reports)
	s := out.String()
	if !strings.Contains(s, "handoff leases: 2 (1 acked, 1 failed)") {
		t.Errorf("report missing the lease summary:\n%s", s)
	}
	if !strings.Contains(s, "upper bound, incl IPC round trip") {
		t.Errorf("lease intervals must stay labeled as upper bounds:\n%s", s)
	}
}

// A journal predating P07e carries no lease records: the handoff
// story is unknown (nil), never "zero handoffs happened".
func TestRecoverLeaseUnknownOnOldJournal(t *testing.T) {
	var buf bytes.Buffer
	jw := NewWriter(&buf, "old-run")
	jw.RunStart("general", "", []int{0}, 1, "30s", false)
	jw.CoreStart(0, "int")
	jw.CoreDone(0, "ok", "")
	jw.RunEnd(0)
	jw.Close()

	recs, iss := ParseAll(bytes.NewReader(buf.Bytes()), "old")
	if iss.ReadErr != "" {
		t.Fatalf("parse: %s", iss.ReadErr)
	}
	reports := RecoverRuns(recs)
	if len(reports) != 1 || len(reports[0].Cores) != 1 {
		t.Fatalf("recovery lost the old story: %+v", reports)
	}
	if reports[0].Cores[0].LeaseObs != nil {
		t.Errorf("old journal invented lease records: %+v", reports[0].Cores[0].LeaseObs)
	}
}
