package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

// S4 contract fixture: one lease step on the wire pins the exact
// record the GUI may rely on. The payload mirrors the journaled
// session_lease evidence field for field; an unreported count is an
// explicit null (unknown is never zeroed into a fact); every key is
// always present, because omitempty would let schema drift sneak in
// unannounced; and with no live stream there is no seam at all, so
// unmanaged runs pay not even an indirect call.
func TestHandoffLeaseStreamRecord(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := currentEvents
	s := newEventSink(w)
	currentEvents = s
	s.setSession("hs-4")
	t.Cleanup(func() { currentEvents = old })

	peer := 3
	emitHandoffLease(5, journal.LeaseObservation{
		Session: "hc5", Seq: 7, Block: 2, Gen: 3, Mode: "lease-write",
		PeerCore: &peer, ToAckMS: 1.25, Acked: true,
	})
	ck := uint64(16384)
	emitHandoffLease(1, journal.LeaseObservation{
		Session: "hc1", Seq: 9, Block: 1, Gen: 2, Mode: "lease-read",
		PeerCore: &peer, ToAckMS: 0.5, Checks: &ck, Acked: false,
		Error: "lease-read fault: mismatch at offset 8",
	})

	// The emitter binds the coordinator's seam to the live stream -
	// and binds nothing when there is no stream.
	f := handoffLeaseEmitter()
	if f == nil {
		t.Fatal("a live stream must bind the lease seam")
	}
	f(7, journal.LeaseObservation{
		Session: "hc7", Seq: 11, Block: 3, Gen: 4, Mode: "lease-write",
		PeerCore: &peer, ToAckMS: 0.25, Acked: true,
	})
	currentEvents = nil
	if f := handoffLeaseEmitter(); f != nil {
		t.Error("no stream, no seam: the coordinator must pay nothing")
	}
	currentEvents = s

	s.close() // drains the queue onto the pipe
	w.Close() // EOF for the reader
	defer r.Close()

	var envs []eventEnvelope
	var payloads []map[string]any
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var ev eventEnvelope
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("stream line not an envelope: %v\n%s", err, sc.Bytes())
		}
		var p map[string]any
		if err := json.Unmarshal(sc.Bytes(), &struct {
			Payload *map[string]any `json:"payload"`
		}{&p}); err != nil {
			t.Fatal(err)
		}
		envs = append(envs, ev)
		payloads = append(payloads, p)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(envs) != 3 {
		t.Fatalf("stream records = %d, want 3", len(envs))
	}

	keys := []string{"core", "session", "seq", "block", "gen", "mode", "peer_core", "to_ack_ms", "checks", "bytes", "acked", "error"}
	for i, ev := range envs {
		if ev.SchemaVersion != eventSchemaVersion || ev.Type != "handoff_lease" || ev.Seq != i+1 || ev.SessionID != "hs-4" || ev.TS == "" {
			t.Errorf("record %d envelope = %+v, want handoff_lease seq %d on session hs-4 with a timestamp", i+1, ev, i+1)
		}
		for _, k := range keys {
			if _, ok := payloads[i][k]; !ok {
				t.Errorf("record %d payload lacks the key %q (schema keys never omit):\n%v", i+1, k, payloads[i])
			}
		}
	}
	num := func(p map[string]any, k string) float64 {
		v, ok := p[k].(float64)
		if !ok {
			t.Errorf("payload key %q = %v, want a number", k, p[k])
		}
		return v
	}

	p := payloads[0]
	if num(p, "core") != 5 || p["session"] != "hc5" || num(p, "seq") != 7 || num(p, "block") != 2 || num(p, "gen") != 3 {
		t.Errorf("record 1 identity fields = %v", p)
	}
	if p["mode"] != "lease-write" || p["acked"] != true || p["error"] != "" || num(p, "peer_core") != 3 {
		t.Errorf("record 1 lease fields = %v, want an acked lease-write with peer core 3", p)
	}
	if p["checks"] != nil || p["bytes"] != nil {
		t.Errorf("an unreported count must be an explicit null, got checks=%v bytes=%v", p["checks"], p["bytes"])
	}

	p = payloads[1]
	if num(p, "core") != 1 || num(p, "block") != 1 || p["mode"] != "lease-read" || p["acked"] != false {
		t.Errorf("record 2 fields = %v", p)
	}
	if num(p, "checks") != 16384 {
		t.Errorf("record 2 checks = %v, want the reported 16384", p["checks"])
	}
	if p["error"] == nil || !strings.Contains(p["error"].(string), "lease-read fault") {
		t.Errorf("record 2 error = %v, want the fault named on the stream too", p["error"])
	}

	if payloads[2]["core"].(float64) != 7 || payloads[2]["mode"] != "lease-write" {
		t.Errorf("record 3 (emitted through the bound seam) = %v, want the core-7 lease-write", payloads[2])
	}
	if s.dropped.Load() != 0 {
		t.Errorf("dropped = %d, want 0 on a drained stream", s.dropped.Load())
	}
}
