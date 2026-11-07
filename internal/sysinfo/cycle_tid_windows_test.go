//go:build windows

package sysinfo

import (
	"runtime"
	"testing"
	"time"
)

// The watchdog path: publish a thread id, open it from another goroutine,
// and watch its cycle counter grow while it spins.
func TestThreadCycleTimeByTID(t *testing.T) {
	tidCh := make(chan int, 1)
	stop := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		tidCh <- CurrentThreadID()
		x := 0
		for {
			select {
			case <-stop:
				return
			default:
				x++
			}
		}
	}()
	tid := <-tidCh
	if tid == 0 {
		t.Fatal("CurrentThreadID() = 0, want a real thread id")
	}
	if _, err := ThreadCycleTime(-1); err == nil {
		t.Error("ThreadCycleTime(-1) unexpectedly succeeded")
	}
	c0, err := ThreadCycleTime(tid)
	if err != nil {
		t.Fatalf("ThreadCycleTime(%d): %v", tid, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c1, err := ThreadCycleTime(tid)
		if err != nil {
			t.Fatalf("second ThreadCycleTime(%d): %v", tid, err)
		}
		if c1 > c0 {
			close(stop)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(stop)
	t.Fatal("thread cycle counter did not grow while spinning")
}
