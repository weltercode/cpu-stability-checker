package memtest

import (
	"context"
	"time"
)

// PrepareReport accounts for the first-touch pass separately from the
// measured run, as the plan requires: the bytes touched and the time
// it took are reported, not estimated.
type PrepareReport struct {
	Touched uint64
	Elapsed time.Duration
}

// touchChunk writes fill across b[off:end].
//
// The noinline barrier is load-bearing, not decoration: measured with
// optimizations on, the compiler deleted the whole fill loop (and
// folded a "verify the pattern" read into a constant true) because it
// could prove no later Go code reads the bytes. First touch must be
// REAL memory traffic - the plan's whole point is that the measured
// run must not pay page faults - and the P04 mem-bw lesson says a
// source-level claim is not proof. The objdump test pins that the
// stores survive in the machine code.
//
//go:noinline
func touchChunk(b []byte, off, end int, fill byte) {
	for j := off; j < end; j++ {
		b[j] = fill
	}
}

// Prepare runs the first-touch pass over every region BEFORE the
// measured run. Committed-but-untouched Windows memory is backed by
// the shared zero page until first written, so a run that starts
// cold would pay page faults inside the measurement window - the
// plan's "allocate before the measured run, account first touch
// separately" exists to stop that.
//
// fill is the pre-touch byte pattern; callers pass a seed-derived
// value when it matters (P06), and 0xA5-style constants elsewhere.
// Keeping it a parameter is also what stops the compiler from
// forward-propagating a stored constant through a later verification
// read and deleting the stores as dead (measured on this toolchain -
// see touchChunk).
//
// The pass is cancellable: a cancel mid-touch returns the bytes
// actually touched so far plus ctx.Err - never a partial pass
// pretending to be complete, and the caller releases the regions.
// The hot loop allocates nothing.
//
// Elapsed is honest but machine-limited: measured on this machine,
// the Go monotonic clock advances in ~0.5 ms steps, so a small pass
// over already-resident pages can legitimately measure 0s (a 384 KiB
// pass did, inner AND outer - no folding, the work really fits one
// clock step). Tests therefore assert a non-zero Elapsed only for
// passes that cannot fit one step (hundreds of MiB), never for tiny
// ones.
func (e *Engine) Prepare(ctx context.Context, fill byte) (PrepareReport, error) {
	const chunk = 1 << 16 // touch in 64 KiB runs so cancellation stays responsive
	start := time.Now()
	var touched uint64
	for i := range e.regions {
		b := e.regions[i].Block.Bytes()
		for off := 0; off < len(b); off += chunk {
			if err := ctx.Err(); err != nil {
				return PrepareReport{Touched: touched, Elapsed: time.Since(start)}, err
			}
			end := min(off+chunk, len(b))
			touchChunk(b, off, end, fill)
			touched += uint64(end - off)
		}
	}
	return PrepareReport{Touched: touched, Elapsed: time.Since(start)}, nil
}
