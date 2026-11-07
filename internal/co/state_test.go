package co

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// B11 (audit): the published bool/null TriState shape must decode
// back - and unknown must never silently become false.
func TestTriStateDecodePublished(t *testing.T) {
	var t1 TriState
	if err := json.Unmarshal([]byte("true"), &t1); err != nil || !t1.Known || !t1.Yes {
		t.Errorf("true: %+v err=%v", t1, err)
	}
	if err := json.Unmarshal([]byte("false"), &t1); err != nil || !t1.Known || t1.Yes {
		t.Errorf("false: %+v err=%v", t1, err)
	}
	if err := json.Unmarshal([]byte("null"), &t1); err != nil || t1.Known {
		t.Errorf("null must be unknown, not false: %+v err=%v", t1, err)
	}
	// null RESETS a previously known answer: a stale yes must not
	// survive an unknown.
	t2 := Yes("stale claim")
	if err := json.Unmarshal([]byte("null"), &t2); err != nil || t2.Known || t2.Why != "" {
		t.Errorf("null did not reset the receiver: %+v err=%v", t2, err)
	}
	// Invalid shapes are errors, never defaults.
	for _, bad := range []string{`"maybe"`, `1`, `[]`, `{}`, `"true"`} {
		var t3 TriState
		if err := json.Unmarshal([]byte(bad), &t3); err == nil {
			t.Errorf("%s decoded into %+v without error", bad, t3)
		}
	}
	// Round trip of the published shape keeps Known/Yes (the reason
	// is not in the published shape - it survives via the DTO).
	b, err := json.Marshal(No("elevation refused"))
	if err != nil || string(b) != "false" {
		t.Fatalf("published shape changed: %s err=%v", b, err)
	}
	var t4 TriState
	if err := json.Unmarshal(b, &t4); err != nil || !t4.Known || t4.Yes {
		t.Errorf("published round trip broken: %+v err=%v", t4, err)
	}
}

// B11: the versioned DTO reads back exactly what it wrote - tri-
// states WITH their reasons, fingerprints, snapshots, mutation
// details.
func TestStateRoundTrip(t *testing.T) {
	caps := Capabilities{
		Tool: "ryzen-smu-cli", Version: "0.1.3", SHA256: "abc",
		Supported:   Yes("ZenStates-Core initialized"),
		Read:        Yes("--get-offsets-terse answered and parsed"),
		Apply:       Unknown("offset register only proven by a real apply"),
		Readback:    Yes("readback is the same read path"),
		RangeKnown:  false,
		SyntaxKnown: true, SyntaxMin: -99, SyntaxMax: 99,
		Notes: []string{"hardware-validated span unknown"},
	}
	before := Snapshot{
		Profile:    Profile{Offsets: []Offset{{Core: "core0", Value: -10}, {Core: "core1", Value: 0}}},
		Quality:    "exact",
		HardwareID: "fp-123",
		ReadAt:     time.Now().UTC().Truncate(time.Millisecond),
	}
	desired := Profile{Offsets: []Offset{{Core: "core0", Value: -30}}}
	res := WriteResult{Mutated: MutatedUnknown, Notes: []string{"call log, not hardware proof"}}
	ver := Verification{Match: No("readback differs"), Diff: []Offset{{Core: "core0", Value: -10}}}

	st := NewState(caps, before, desired, res, ver)
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseState(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Caps, capsToV1(caps)) {
		t.Errorf("capabilities drifted:\n got %+v\nwant %+v", got.Caps, capsToV1(caps))
	}
	if !reflect.DeepEqual(got.Before, before) {
		t.Errorf("snapshot drifted:\n got %+v\nwant %+v", got.Before, before)
	}
	if !reflect.DeepEqual(got.Verification(), ver) {
		t.Errorf("verification drifted:\n got %+v\nwant %+v", got.Verification(), ver)
	}
	if !reflect.DeepEqual(got.Capabilities(), caps) {
		t.Errorf("capabilities live-shape drifted: %+v", got.Capabilities())
	}
	// The reason of an UNKNOWN tri-state survives - the whole point
	// of the DTO over the published bool/null shape.
	if got.Caps.Apply.Known || got.Caps.Apply.Why == "" {
		t.Errorf("unknown reason lost: %+v", got.Caps.Apply)
	}
	// A hand-built State can never write an unstamped or wrong-
	// version file.
	hand := State{Version: 99}
	hb, _ := json.Marshal(hand)
	if !strings.Contains(string(hb), `"version":1`) {
		t.Errorf("version stamping broken: %s", hb)
	}
}

// B11: versions are gates, not hints. Missing, wrong-typed or
// unknown versions are refused - never partially decoded, never
// assumed v1.
func TestStateVersionGate(t *testing.T) {
	full, _ := json.Marshal(NewState(Capabilities{RangeKnown: true}, Snapshot{Quality: "exact", HardwareID: "x", Profile: Profile{Offsets: []Offset{{Core: "c", Value: 0}}}}, Profile{}, WriteResult{}, Verification{}))
	cases := []struct{ name, doc string }{
		{"future version", strings.Replace(string(full), `"version":1`, `"version":2`, 1)},
		{"missing version", strings.Replace(string(full), `"version":1,`, ``, 1)},
		{"version wrong type", strings.Replace(string(full), `"version":1`, `"version":"one"`, 1)},
		{"not an object", `[]`},
		{"garbage", `not json`},
		{"unknown field", strings.Replace(string(full), `"saved_at"`, `"saved_att"`, 1)},
	}
	for _, c := range cases {
		if _, err := ParseState([]byte(c.doc)); err == nil {
			t.Errorf("%s: ParseState accepted it", c.name)
		}
	}
}

// B03/B11: an incomplete snapshot never gains restore permission;
// unknown identity or unvalidated support blocks too.
func TestRestorePermissionGate(t *testing.T) {
	good := NewState(
		Capabilities{Supported: Yes("ok"), Read: Yes("ok"), RangeKnown: true, RangeMin: -99, RangeMax: 99},
		Snapshot{Quality: "exact", HardwareID: "fp", Profile: Profile{Offsets: []Offset{{Core: "core0", Value: 0}}}},
		Profile{Offsets: []Offset{{Core: "core0", Value: -10}}}, WriteResult{}, Verification{})
	if ok, why := good.RestorePermission(); !ok {
		t.Errorf("a complete record must be permitted, got: %s", why)
	}
	bad := good
	bad.Before.Quality = "partial"
	if ok, why := bad.RestorePermission(); ok || why == "" {
		t.Errorf("partial snapshot gained restore permission")
	}
	bad = good
	bad.Before.HardwareID = ""
	if ok, _ := bad.RestorePermission(); ok {
		t.Errorf("fingerprint-less snapshot gained restore permission")
	}
	bad = good
	bad.Caps.Read = TriStateV1{Known: true, Why: "read unavailable"}
	if ok, _ := bad.RestorePermission(); ok {
		t.Errorf("known-unavailable read gained restore permission")
	}
	bad = good
	bad.Caps.RangeKnown = false
	if ok, _ := bad.RestorePermission(); ok {
		t.Errorf("unvalidated range gained restore permission")
	}
	bad = good
	bad.Before.Profile.Offsets = nil
	if ok, _ := bad.RestorePermission(); ok {
		t.Errorf("empty snapshot gained restore permission")
	}
}
