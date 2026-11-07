//go:build windows

package sysinfo

import (
	"reflect"
	"testing"
)

// A process can inherit a restricted affinity mask from its parent shell;
// thread pinning then fails for every core outside that mask. WidenAffinity
// grows the process mask back over the requested cores, bounded by the
// system mask. The test narrows the whole test binary on purpose and
// restores the original mask afterwards.
func TestWidenAffinityCoversInheritedRestriction(t *testing.T) {
	before, ok := CurrentThreadAffinity()
	if !ok || before == 0 {
		t.Skip("platform cannot report thread affinity")
	}
	var restore []int
	for c := range 64 {
		if before&(uint64(1)<<uint(c)) != 0 {
			restore = append(restore, c)
		}
	}
	t.Cleanup(func() {
		if err := SetAffinity(restore); err != nil {
			t.Errorf("restore process affinity: %v", err)
		}
	})

	if err := SetAffinity([]int{1}); err != nil {
		t.Fatalf("SetAffinity([1]): %v", err)
	}
	if mask, _ := CurrentThreadAffinity(); mask != 1<<1 {
		t.Fatalf("after SetAffinity([1]) mask = %x, want 0x2", mask)
	}

	if missing := WidenAffinity([]int{0, 1}); len(missing) != 0 {
		t.Fatalf("WidenAffinity([0 1]) missing = %v, want none", missing)
	}
	if mask, _ := CurrentThreadAffinity(); mask != 1<<0|1<<1 {
		t.Fatalf("after widening mask = %x, want 0x3", mask)
	}

	// A core the system mask itself does not offer stays reported.
	if missing := WidenAffinity([]int{63}); !reflect.DeepEqual(missing, []int{63}) {
		t.Fatalf("WidenAffinity([63]) missing = %v, want [63] on a 32-core box", missing)
	}
	if mask, _ := CurrentThreadAffinity(); mask != 1<<0|1<<1 {
		t.Fatalf("mask after refused widening = %x, want it unchanged at 0x3", mask)
	}
}
