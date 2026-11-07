//go:build windows

package sysinfo

import (
	"os"
	"testing"
)

// The parent's live telemetry reads children through this counter; on our
// own process it must read, advance, and refuse cleanly after Close.
func TestProcessCycleCounterSelf(t *testing.T) {
	c, err := NewProcessCycleCounter(os.Getpid())
	if err != nil {
		t.Fatalf("NewProcessCycleCounter(self): %v", err)
	}
	n1, err := c.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	x := 0
	for i := 0; i < 1_000_000; i++ {
		x += i
	}
	if x == -1 {
		t.Fatal("unreachable")
	}
	n2, err := c.Read()
	if err != nil {
		t.Fatalf("Read after work: %v", err)
	}
	if n2 <= n1 {
		t.Errorf("cycle count did not advance: %d -> %d", n1, n2)
	}
	c.Close()
	if _, err := c.Read(); err == nil {
		t.Errorf("Read after Close: want an error, got none")
	}
	c.Close() // double close must not panic
}

// soak's leak verdict leans on handle counting; on Windows it must
// actually see the process's handles.
func TestHandleCount(t *testing.T) {
	n, ok := HandleCount()
	if !ok {
		t.Fatal("HandleCount: Windows must report the handle count")
	}
	if n <= 0 {
		t.Errorf("HandleCount = %d, want a positive number", n)
	}
}
