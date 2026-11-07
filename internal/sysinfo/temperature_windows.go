//go:build windows

package sysinfo

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	pdh                 = syscall.NewLazyDLL("pdh.dll")
	procPdhOpenQuery    = pdh.NewProc("PdhOpenQueryW")
	procPdhAddCounter   = pdh.NewProc("PdhAddEnglishCounterW")
	procPdhCollect      = pdh.NewProc("PdhCollectQueryData")
	procPdhGetFormatted = pdh.NewProc("PdhGetFormattedCounterValue")
	procPdhCloseQuery   = pdh.NewProc("PdhCloseQuery")
)

// pdhNoData is PDH_CSTATUS_NO_DATA: the query is valid but the provider
// has nothing to report — typical when the board exposes no thermal zone.
const pdhNoData = 0x800007D5

// (pdhFmtValue - the PDH_FMT_COUNTERVALUE mirror - lives in
// perf_windows.go; same package, same build tag.)

func newTemperatureSensor() (*TemperatureSensor, error) {
	s := &TemperatureSensor{}
	r, _, e := procPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&s.query)))
	if r != 0 {
		return nil, fmt.Errorf("PdhOpenQuery: status 0x%x: %w", r, e)
	}
	fail := func(format string, a ...any) (*TemperatureSensor, error) {
		procPdhCloseQuery.Call(s.query)
		s.query = 0
		return nil, fmt.Errorf(format, a...)
	}
	path, err := syscall.UTF16PtrFromString(`\Thermal Zone Information(_Total)\Temperature`)
	if err != nil {
		return fail("%w", err)
	}
	r, _, e = procPdhAddCounter.Call(s.query, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&s.counter)))
	if r != 0 {
		return fail("thermal zone temperature counter not available: status 0x%x: %w", r, e)
	}
	if r, _, e = procPdhCollect.Call(s.query); r != 0 {
		if r == pdhNoData {
			return fail("thermal zone provider reports no data on this machine")
		}
		return fail("PdhCollectQueryData: status 0x%x: %w", r, e)
	}
	return s, nil
}

func (s *TemperatureSensor) read() (float64, bool) {
	if s == nil || s.query == 0 {
		return 0, false
	}
	if r, _, _ := procPdhCollect.Call(s.query); r != 0 {
		return 0, false
	}
	var val pdhFmtValue
	// pdhFmtDouble: the flag that actually yields an IEEE double on
	// this build (see perf_windows.go).
	r, _, _ := procPdhGetFormatted.Call(s.counter, pdhFmtDouble, 0, uintptr(unsafe.Pointer(&val)))
	if r != 0 {
		return 0, false
	}
	if val.cStatus != 0 && val.cStatus != 1 { // VALID_DATA / NEW_DATA
		return 0, false
	}
	return kelvinToCelsius(val.double)
}

func (s *TemperatureSensor) close() {
	if s != nil && s.query != 0 {
		procPdhCloseQuery.Call(s.query)
		s.query = 0
	}
}
