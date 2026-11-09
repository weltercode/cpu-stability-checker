//go:build windows

package sysinfo

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

var (
	procPdhGetArray = pdh.NewProc("PdhGetFormattedCounterArrayW")
	kernel32SMBIOS  = syscall.NewLazyDLL("kernel32.dll")
	procFirmwareTbl = kernel32SMBIOS.NewProc("GetSystemFirmwareTable")
)

const (
	// PDH_FMT_DOUBLE: the documented 0x100 writes a LONG on this
	// Windows build (verified live against the running counter); 0x200
	// is what actually yields an IEEE double - and it preserves
	// percentages above 100 (boost), so no NOCAP100 is needed (the
	// array API rejects that flag anyway).
	pdhFmtDouble = 0x00000200
	// PDH_MORE_DATA as actually returned by the size probe on this
	// build (the documented 0xC0000BC6 never appears here).
	pdhMoreData = 0x800007D2
)

// pdhFmtValue mirrors PDH_FMT_COUNTERVALUE: a DWORD status, padding to
// 8-byte alignment, then the value union (a double when PDH_FMT_DOUBLE
// is requested).
type pdhFmtValue struct {
	cStatus uint32
	_       uint32
	double  float64
}

// pdhFmtItem mirrors PDH_FMT_COUNTERVALUE_ITEM_W: instance name plus
// the value struct (24 bytes on amd64).
type pdhFmtItem struct {
	name *uint16
	val  pdhFmtValue
}

// PerfClock turns the OS's own "% Processor Performance" counter -
// the observed clock as a percentage of the firmware-rated base - into
// real per-logical-CPU MHz. It is the honest middle ground between the
// invariant TSC and HWiNFO's SMU readings: the percentage is measured
// by the kernel's perf infrastructure, and the base comes from the
// firmware's own SMBIOS declaration, not from a boot-time registry
// guess.
type PerfClock struct {
	query   uintptr
	counter uintptr
	base    float64
}

// OpenPerfClock prepares the wildcard per-CPU performance counter.
// Failure (no PDH, no counter, ambiguous SMBIOS) means the caller
// keeps its TSC column - never a fabricated MHz.
func OpenPerfClock() (*PerfClock, error) {
	base, err := ratedBaseMHz()
	if err != nil {
		return nil, err
	}
	p := &PerfClock{base: base}
	r, _, e := procPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&p.query)))
	if r != 0 {
		return nil, fmt.Errorf("PdhOpenQuery: status 0x%x: %w", r, e)
	}
	fail := func(format string, a ...any) (*PerfClock, error) {
		procPdhCloseQuery.Call(p.query)
		p.query = 0
		return nil, fmt.Errorf(format, a...)
	}
	path, err := syscall.UTF16PtrFromString(`\Processor Information(*)\% Processor Performance`)
	if err != nil {
		return fail("%w", err)
	}
	r, _, e = procPdhAddCounter.Call(p.query, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&p.counter)))
	if r != 0 {
		return fail("processor performance counter not available: status 0x%x: %w", r, e)
	}
	if r, _, e = procPdhCollect.Call(p.query); r != 0 {
		return fail("PdhCollectQueryData: status 0x%x: %w", r, e)
	}
	return p, nil
}

// Base returns the firmware-rated base clock the percentages are
// scaled against.
func (p *PerfClock) Base() float64 { return p.base }

// Read collects one sample and returns logical-CPU -> real MHz. The
// percentage is a rate over the collection interval, so consecutive
// calls need to be at least a second apart (the panel repaints at
// 1 Hz). Instances without valid data are left out rather than
// guessed at.
func (p *PerfClock) Read() map[int]float64 {
	if p == nil || p.query == 0 {
		return nil
	}
	if r, _, _ := procPdhCollect.Call(p.query); r != 0 {
		return nil
	}
	var need, items uint32
	// The size probe needs a real numitems pointer: NULL is rejected
	// (PDH_INVALID_ARGUMENT), verified live.
	r, _, _ := procPdhGetArray.Call(p.counter, pdhFmtDouble,
		uintptr(unsafe.Pointer(&need)), uintptr(unsafe.Pointer(&items)), 0)
	if r != pdhMoreData || need == 0 {
		return nil
	}
	buf := make([]byte, need)
	r, _, _ = procPdhGetArray.Call(p.counter, pdhFmtDouble,
		uintptr(unsafe.Pointer(&need)), uintptr(unsafe.Pointer(&items)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 || items == 0 {
		return nil
	}
	recs := unsafe.Slice((*pdhFmtItem)(unsafe.Pointer(&buf[0])), int(items))
	out := map[int]float64{}
	for _, it := range recs {
		if it.val.cStatus != 0 && it.val.cStatus != 1 { // VALID_DATA / NEW_DATA
			continue
		}
		cpu, ok := perfInstanceCPU(instanceName(it.name))
		if !ok {
			continue
		}
		pct := it.val.double
		if math.IsNaN(pct) || pct <= 0 || pct > 400 {
			continue // no clock, or an obviously broken sample
		}
		out[cpu] = p.base * pct / 100
	}
	return out
}

// Close releases the PDH query.
func (p *PerfClock) Close() {
	if p != nil && p.query != 0 {
		procPdhCloseQuery.Call(p.query)
		p.query = 0
	}
}

// perfInstanceCPU maps a "Processor Information" instance name to the
// logical CPU number: "0,7" (socket,processor) or "7"; "_Total" and
// anything unparseable are not per-CPU instances.
func perfInstanceCPU(name string) (int, bool) {
	if i := strings.LastIndexByte(name, ','); i >= 0 {
		name = name[i+1:]
	}
	n, err := strconv.Atoi(name)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// instanceName decodes a PDH-owned UTF-16 instance name (capped walk:
// the memory belongs to PDH, we must not run off the end of a
// malformed string).
func instanceName(p *uint16) string {
	if p == nil {
		return ""
	}
	units := make([]uint16, 0, 32)
	for i := 0; i < 512; i++ {
		u := *(*uint16)(unsafe.Add(unsafe.Pointer(p), i*2))
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	return string(utf16.Decode(units))
}

// ratedBaseMHz reads the firmware-declared rated maximum core speed
// from SMBIOS Type 4 (processor) records. Every populated record must
// agree; disagreement is ambiguous calibration, and guessing would
// put fake MHz numbers on the panel - so we refuse instead and the
// caller stays on the honest tsc label.
//
// Rewrite note (2026-11-09, verified live against this dev board):
// the old inline parser misread the RawSMBIOSData DWORD (a
// structure-data BYTE count) as a record count - harmless in practice
// because the loop was also bounded by the buffer - and skipped
// string areas by scanning for any adjacent NUL pair. On this
// firmware (which pads some records with an extra NUL) those two
// quirks happened to cancel, and the old code did reach the Type 4
// record; a STRICT sequential walk, by contrast, desyncs on this
// board, which is what the memory collector's development probe
// proved. The shared scan+validate path replaces both with behavior
// that is robust by design instead of by luck, and it yields the same
// 4300 MHz on this machine (verified live). One firmware fact the
// rewrite had to accommodate: this board's Type 4 record carries
// corrupted string indices (Processor ID 141, Version 100 in a
// six-string set) and a garbage Processor Type byte, while its
// socket string and Max Speed are correct - so Type 4 validation
// checks only what this firmware demonstrably writes correctly, and
// disagreement still refuses rather than guesses.
func ratedBaseMHz() (float64, error) {
	data, _, err := rsmbTableData()
	if err != nil {
		return 0, err
	}
	mhz, err := parseRatedProcessorMHz(data)
	if err != nil {
		return 0, err
	}
	return float64(mhz), nil
}
