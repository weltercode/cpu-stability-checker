package workload

import (
	"context"
	"testing"
)

type nopReporter struct{}

func (nopReporter) Add(uint64) {}
func (nopReporter) Beat()      {}

// The fault workload is the negative control: it must always report a
// fault, so soak can prove the fault pipeline stays alive cycle after
// cycle without waiting for the CPU to actually misbehave.
func TestFaultWorkloadAlwaysFaults(t *testing.T) {
	if _, ok := Get("fault"); !ok {
		t.Fatal("fault workload is not registered")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := RunOne(ctx, "fault", nopReporter{}); err == nil {
		t.Fatal("RunOne(fault): want an error, got nil")
	}
}
