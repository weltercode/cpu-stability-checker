package memtest

import (
	"errors"
	"testing"
)

func TestParseLockMode(t *testing.T) {
	cases := []struct {
		in      string
		want    LockMode
		wantErr bool
	}{
		{"", LockOff, false},
		{"off", LockOff, false},
		{"try", LockTry, false},
		{"required", LockRequired, false},
		{"OFF", "", true},    // wrong case is a typo, not a quiet default
		{"yes", "", true},    // made-up mode: visible setup error
		{"locked", "", true}, // ditto
	}
	for _, c := range cases {
		got, err := ParseLockMode(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseLockMode(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if err == nil && got != c.want {
			t.Errorf("ParseLockMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// off never attempts, and the report still states the large-pages
// capability honestly either way.
func TestLockOffReportsNothingAttempted(t *testing.T) {
	e := &Engine{regions: []AllocatedRegion{{Block: Block{Addr: 0x1000, Allocated: 4096}}}}
	rep, err := e.Lock(LockOff)
	if err != nil {
		t.Fatalf("off mode must not error: %v", err)
	}
	if rep.Attempted || rep.LockedBytes != 0 || rep.FailedRegions != 0 {
		t.Errorf("off mode attempted something: %+v", rep)
	}
	if rep.LargePages != LargePagesP05 {
		t.Errorf("LargePages = %q, want the explicit P05 statement %q", rep.LargePages, LargePagesP05)
	}
}

// required on a region the OS refuses (a garbage address cannot be
// locked) must return an error wrapping ErrLockFailed - the failure
// is surfaced, never downgraded to "locked anyway".
func TestLockRequiredSurfacesFailure(t *testing.T) {
	e := &Engine{regions: []AllocatedRegion{
		{Block: Block{Addr: 0x1, Requested: 4096, Allocated: 4096}},
	}}
	rep, err := e.Lock(LockRequired)
	if err == nil {
		t.Fatalf("required mode accepted a failed lock: %+v", rep)
	}
	if !errors.Is(err, ErrLockFailed) {
		t.Errorf("error %v does not wrap ErrLockFailed", err)
	}
	if rep.FailedRegions != 1 || rep.Failure == "" {
		t.Errorf("report hides the failure: %+v", rep)
	}
}

// try on the same garbage region reports the failure honestly and
// returns no error - the run may continue unlocked, but the report
// says it is unlocked.
func TestLockTryReportsFailureWithoutError(t *testing.T) {
	e := &Engine{regions: []AllocatedRegion{
		{Block: Block{Addr: 0x1, Requested: 4096, Allocated: 4096}},
	}}
	rep, err := e.Lock(LockTry)
	if err != nil {
		t.Fatalf("try mode must not error on failure: %v", err)
	}
	if rep.FailedRegions != 1 || rep.Failure == "" || rep.LockedBytes != 0 {
		t.Errorf("try mode hid the failure: %+v", rep)
	}
}
