// Package logging writes timestamped, levelled lines to a file.
package logging

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Logger is safe for concurrent use and is the durable record of what ran
// on which core, when, and how — especially useful for figuring out what
// failed where.
type Logger struct {
	mu sync.Mutex
	f  *os.File
}

// New opens the log file for appending. The log is a history: each run
// (and each scenario phase) is delimited by its own start/end markers,
// and a scenario keeps all phases in one file.
func New(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Logger{f: f}, nil
}

// Log writes a single line with a UTC timestamp and level.
func (l *Logger) Log(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	fmt.Fprintln(l.f, ts, level, msg)
}

// Logf writes a formatted line.
func (l *Logger) Logf(level, format string, args ...any) {
	l.Log(level, fmt.Sprintf(format, args...))
}

// Flush is a no-op for *os.File but keeps the interface explicit.
func (l *Logger) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.f.Sync()
}

func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.f.Close()
}
