//go:build windows

package sysinfo

import (
	"os"
	"testing"
)

// ProcessAffinityOf reads back what the OS really applied - here for
// our own pid, where it must agree with ProcessAffinity(). The point is
// the read-back channel existing at all: per-CPU claims about external
// tools rest on it, never on the mask we hoped for.
func TestProcessAffinityOfSelf(t *testing.T) {
	want, ok := ProcessAffinity()
	if !ok {
		t.Skip("cannot read our own affinity here")
	}
	mask, err := ProcessAffinityOf(os.Getpid())
	if err != nil {
		t.Fatalf("ProcessAffinityOf(self) error = %v", err)
	}
	if mask != want {
		t.Errorf("foreign-read mask %#x != own mask %#x", mask, want)
	}
}

// ProcessorGroups reports the machine's group layout honestly: on any
// real Windows machine the counts must be plausible (at least one
// group, at least as many active CPUs as group 0 has). Our pinning
// speaks group-0 masks only; this is the fact that makes "beyond the
// mask" a stated limit rather than a silent truncation.
func TestProcessorGroups(t *testing.T) {
	maxGroups, activeCPUs, ok := ProcessorGroups()
	if !ok {
		t.Skip("cannot determine processor groups here")
	}
	if maxGroups < 1 {
		t.Errorf("max groups = %d, want at least 1", maxGroups)
	}
	if activeCPUs < 1 {
		t.Errorf("active CPUs = %d, want at least 1", activeCPUs)
	}
	t.Logf("machine layout: %d processor group(s), %d active CPU(s)", maxGroups, activeCPUs)
}
