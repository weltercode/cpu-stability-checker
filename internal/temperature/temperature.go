// Package temperature owns the temperature observation shared by the UI
// and the run's cutoff. Provider calls never run on the cutoff's thread.
package temperature

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

const (
	MaxAge  = 5 * time.Second
	Off     = "off"
	Active  = "active"
	Waiting = "waiting for data"
	Lost    = "readings lost"
	Limit   = "limit reached"
)

type Reading struct {
	Celsius float64
	Source  string
	Sensor  string
	At      time.Time // producer's last update, not the time a cached value was read
	CPU     bool      // CPU readings must not silently degrade to a board zone mid-run
}

type Reader interface {
	Read() (Reading, error)
	Close()
}

type Status struct {
	Reading Reading
	Known   bool
	State   string
	LimitC  float64
	Reason  string
	Abort   string
	// ProviderAbsent: this monitor never received a single fresh
	// reading from any provider (no HWiNFO, WMI blocked, no ACPI
	// thermal zone): the sensor source is not on this machine, which
	// is a monitoring fact, never a CPU fact and never a stop by
	// itself. A monitor that HAD readings and lost them is the other
	// case (Reason says lost) - and with control armed that one IS a
	// stop, because silence mid-run cannot be trusted as "cool".
	ProviderAbsent bool
}

func (s Status) Line() string {
	value := "temp n/a"
	if s.Known {
		value = fmt.Sprintf("temp %.1f°C", s.Reading.Celsius)
	}
	if s.Reading.Source != "" {
		value += " [" + s.Reading.Source + ": " + s.Reading.Sensor + "]"
	}
	value += " | control " + s.State
	if s.LimitC > 0 {
		value += fmt.Sprintf(" (limit %.1f°C)", s.LimitC)
	}
	if !s.Known && s.Reason != "" {
		if s.ProviderAbsent {
			// Short, honest, and never alarmist: the raw Win32 error
			// strings (CreateFile pipes, WMI query failures) belong
			// in the journal/log details via Status.Reason, not in a
			// one-line monitor readout that then reads like a fault.
			value += " | no temperature sensor provider on this machine (HWiNFO/WMI/ACPI): thermal control inactive - not a stop"
		} else {
			value += " | " + s.Reason
		}
	}
	return value
}

type observation struct {
	reading Reading
	err     error
}

// Monitor polls each provider independently. A blocked WMI/COM call cannot
// stop HWiNFO or ACPI updates, nor prevent a stale-reading cutoff.
type Monitor struct {
	mu               sync.Mutex
	readers          []Reader
	values           []observation
	maxAge, interval time.Duration
	limit            float64
	armedAt          time.Time
	last             Reading
	tripped          string
	tripReason       string
	cpuRequired      bool
	start            sync.Once
	stopOnce         sync.Once
	stop             chan struct{}
	changed          chan struct{}
	wg               sync.WaitGroup
}

func New() *Monitor { return NewWithReaders(MaxAge, time.Second, defaultReaders()...) }

// NewWithReaders accepts providers in priority order; useful for deterministic
// tests and for callers with a different polling/freshness budget.
func NewWithReaders(maxAge, interval time.Duration, readers ...Reader) *Monitor {
	if maxAge <= 0 {
		maxAge = MaxAge
	}
	if interval <= 0 {
		interval = time.Second
	}
	return &Monitor{readers: readers, values: make([]observation, len(readers)), maxAge: maxAge,
		interval: interval, stop: make(chan struct{}), changed: make(chan struct{}, 1)}
}

func (m *Monitor) ensureStarted() {
	m.start.Do(func() {
		for i, r := range m.readers {
			m.wg.Add(1)
			go func(i int, r Reader) {
				defer m.wg.Done()
				defer r.Close()
				tick := time.NewTicker(m.interval)
				defer tick.Stop()
				for {
					select {
					case <-m.stop:
						return
					default:
					}
					reading, err := r.Read()
					m.mu.Lock()
					m.values[i] = observation{reading, err}
					m.mu.Unlock()
					select {
					case m.changed <- struct{}{}:
					default:
					}
					select {
					case <-m.stop:
						return
					case <-tick.C:
					}
				}
			}(i, r)
		}
	})
}

func fresh(r Reading, now time.Time, maxAge time.Duration) bool {
	return r.Source != "" && r.Sensor != "" && !r.At.IsZero() && !math.IsNaN(r.Celsius) && !math.IsInf(r.Celsius, 0) &&
		r.Celsius >= -50 && r.Celsius <= 200 && now.Sub(r.At) <= maxAge && r.At.Sub(now) <= time.Second
}

func (m *Monitor) status(now time.Time) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Reading: m.last, State: Off, LimitC: m.limit, Reason: "no fresh temperature sensor"}
	var reasons []string
	for _, v := range m.values {
		if v.err != nil {
			reasons = append(reasons, v.err.Error())
		}
		if v.err == nil && fresh(v.reading, now, m.maxAge) && !(m.limit > 0 && m.cpuRequired && !v.reading.CPU) {
			s.Reading, s.Known, s.Reason = v.reading, true, ""
			m.last = v.reading
			if m.limit > 0 && v.reading.CPU {
				m.cpuRequired = true
			}
			break
		}
	}
	if !s.Known {
		// No reading ever observed by this monitor => the providers
		// are absent on this machine; a monitor that saw readings and
		// went stale keeps the "readings lost" story instead.
		s.ProviderAbsent = m.last.Source == ""
	}
	if !s.Known && len(reasons) > 0 {
		s.Reason = strings.Join(reasons, "; ")
	}
	if m.limit <= 0 {
		return s
	}
	s.State = Active
	if m.tripped != "" {
		s.State, s.Abort = m.tripped, m.tripReason
		return s
	}
	if !s.Known {
		s.State = Waiting
		at := m.last.At
		if at.IsZero() {
			at = m.armedAt
		}
		if now.Sub(at) >= m.maxAge {
			m.tripped = Lost
			m.tripReason = "temperature readings lost: no fresh sensor for " + m.maxAge.String()
			if m.last.Source != "" {
				m.tripReason += " (last " + m.last.Source + ": " + m.last.Sensor + ")"
			}
		}
	} else if s.Reading.Celsius >= m.limit {
		m.tripped = Limit
		m.tripReason = fmt.Sprintf("temperature limit: %s/%s %.1f°C >= %.1f°C", s.Reading.Source, s.Reading.Sensor, s.Reading.Celsius, m.limit)
	}
	if m.tripped != "" {
		s.State, s.Abort = m.tripped, m.tripReason
	}
	return s
}

func (m *Monitor) Snapshot() Status { m.ensureStarted(); return m.status(time.Now()) }

// Prepare arms the cutoff and waits for an initial fresh observation before
// permitting load. A disabled cutoff does not require any sensor.
func (m *Monitor) Prepare(ctx context.Context, limit float64) Status {
	if math.IsNaN(limit) || math.IsInf(limit, 0) || limit < 0 {
		return Status{State: Lost, Abort: "invalid temperature limit: must be finite and non-negative"}
	}
	m.mu.Lock()
	m.limit = limit
	m.armedAt = time.Now()
	m.tripped = ""
	m.tripReason = ""
	m.cpuRequired = m.last.CPU
	m.mu.Unlock()
	if limit <= 0 {
		return m.status(time.Now())
	}
	m.ensureStarted()
	tick := time.NewTicker(min(m.interval, 50*time.Millisecond))
	defer tick.Stop()
	for {
		s := m.Snapshot()
		if limit <= 0 || s.Known || s.Abort != "" {
			return s
		}
		select {
		case <-ctx.Done():
			s.Abort = "temperature startup cancelled: " + ctx.Err().Error()
			return s
		case <-m.changed:
		case <-tick.C:
		}
	}
}

func (m *Monitor) Disarm() {
	m.mu.Lock()
	m.limit = 0
	m.tripped = ""
	m.tripReason = ""
	m.mu.Unlock()
}

func (m *Monitor) Close() {
	m.stopOnce.Do(func() { close(m.stop) })
	// Synchronize with lazy startup before waiting for provider ownership.
	m.ensureStarted()
	m.wg.Wait()
}
