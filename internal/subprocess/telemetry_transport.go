package subprocess

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// The worker measures its own load window (observed cycle rate, idle
// share, window length) exactly once, around the work that ran on its
// pinned core. The parent owns the combined journal Telemetry record —
// only the parent also samples the real producer clocks — so the worker
// ships its own measurements home over a channel it already has: its
// stderr, which the parent captures in full. One structured line, a
// unique prefix, parsed and stripped at reap; a worker that died never
// wrote one, and the parent then records those pieces as unknown rather
// than inventing them.
const workerTelPrefix = "worker-telemetry "

// workerTelemetry is the worker's self-measured window. Absent or zero
// clock and nil idle both mean unknown.
type workerTelemetry struct {
	ClockMHz float64  `json:"clock_mhz,omitempty"`
	IdlePct  *float64 `json:"idle_pct,omitempty"`
	WindowMS int64    `json:"window_ms,omitempty"`
}

// publishWorkerTelemetry is the worker side: stdout and the journal
// stay untouched; a marshal failure only loses the observation.
func publishWorkerTelemetry(t workerTelemetry) {
	b, err := json.Marshal(t)
	if err != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "%s%s\n", workerTelPrefix, b)
}

// takeWorkerTelemetry is the parent side: it extracts (and removes) the
// last telemetry line from the captured stderr buffer so the line never
// surfaces inside an error message, and returns nil when the worker
// never published one. A malformed line is dropped like any unknown.
func takeWorkerTelemetry(buf *bytes.Buffer) *workerTelemetry {
	s := buf.String()
	idx := strings.LastIndex(s, workerTelPrefix)
	if idx < 0 {
		return nil
	}
	rest := s[idx+len(workerTelPrefix):]
	nl := strings.IndexByte(rest, '\n')
	payload, tail := rest, ""
	if nl >= 0 {
		payload, tail = rest[:nl], rest[nl+1:]
	}
	buf.Reset()
	buf.WriteString(s[:idx])
	buf.WriteString(tail)
	var t workerTelemetry
	if err := json.Unmarshal([]byte(payload), &t); err != nil {
		return nil
	}
	return &t
}
