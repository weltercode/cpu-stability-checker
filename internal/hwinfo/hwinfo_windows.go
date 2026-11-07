//go:build windows

package hwinfo

import (
	"encoding/binary"
	"fmt"
	"math"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"
)

var (
	kernel32HW        = syscall.NewLazyDLL("kernel32.dll")
	procOpenFileMapHW = kernel32HW.NewProc("OpenFileMappingW")
	procMapViewHW     = kernel32HW.NewProc("MapViewOfFile")
	procUnmapHW       = kernel32HW.NewProc("UnmapViewOfFile")
	procCloseHW       = kernel32HW.NewProc("CloseHandle")
	procMoveMemHW     = kernel32HW.NewProc("RtlMoveMemory")
)

const (
	fileMapRead = 0x0004
	// HWiNFO's published mapping name (shared memory support must be
	// enabled in its settings; the name is stable across 5.x+ versions).
	shmName = "Global\\HWiNFO_SENS_SM2"
)

// Published layout offsets (pack=1). Reading records: type, index, id,
// name[128], custom name[128], unit[16], then four float64s.
const (
	hdrSSOffset   = 20
	hdrSSSize     = 24
	hdrSSCount    = 28
	hdrReadOffset = 32
	hdrReadSize   = 36
	hdrReadCount  = 40
	hdrMinSize    = 44
	rdOffType     = 0
	rdOffIndex    = 4
	rdOffID       = 8
	rdOffName     = 12
	rdOffUnit     = 268
	rdOffValue    = 284
	rdMinSize     = 292
	nameLen       = 128
	unitLen       = 16
)

// Source is an open view onto HWiNFO's shared memory.
type Source struct {
	handle uintptr
	view   uintptr
	size   int // reading table extent, re-checked on every copy
	off    uint32
	stride uint32
	count  uint32
}

// Open attaches to HWiNFO's shared memory. The error explains the two
// real causes: HWiNFO is not running, or its shared-memory support is
// off.
func Open() (*Source, error) { return openNamed(shmName) }

func openNamed(name string) (*Source, error) {
	utf16Name, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, _, e := procOpenFileMapHW.Call(fileMapRead, 0, uintptr(unsafe.Pointer(utf16Name)))
	if h == 0 {
		return nil, fmt.Errorf("open %s: %w (is HWiNFO running with Shared Memory Support enabled?)", name, e)
	}
	v, _, e2 := procMapViewHW.Call(h, fileMapRead, 0, 0, 0)
	if v == 0 {
		procCloseHW.Call(h)
		return nil, fmt.Errorf("map view: %w", e2)
	}
	s := &Source{handle: h, view: v}
	hdr := s.copy(0, hdrMinSize)
	off := binary.LittleEndian.Uint32(hdr[hdrReadOffset:])
	stride := binary.LittleEndian.Uint32(hdr[hdrReadSize:])
	count := binary.LittleEndian.Uint32(hdr[hdrReadCount:])
	if count == 0 || stride < rdMinSize {
		s.Close()
		return nil, fmt.Errorf("shared memory layout not understood (readings=%d stride=%d)", count, stride)
	}
	s.off, s.stride, s.count = off, stride, count
	s.size = int(off) + int(count)*int(stride)
	return s, nil
}

// Close releases the mapping view.
func (s *Source) Close() {
	if s.view != 0 {
		procUnmapHW.Call(s.view)
		s.view = 0
	}
	if s.handle != 0 {
		procCloseHW.Call(s.handle)
		s.handle = 0
	}
}

// Readings copies the current reading table. HWiNFO refreshes it on its
// own poll interval; once per panel frame is cheap.
func (s *Source) Readings() []Reading {
	if s.view == 0 {
		return nil
	}
	buf := s.copy(0, s.size)
	out := make([]Reading, 0, s.count)
	for i := 0; i < int(s.count); i++ {
		rec := buf[int(s.off)+i*int(s.stride):]
		out = append(out, Reading{
			Type:  binary.LittleEndian.Uint32(rec[rdOffType:]),
			Index: binary.LittleEndian.Uint32(rec[rdOffIndex:]),
			ID:    binary.LittleEndian.Uint32(rec[rdOffID:]),
			Name:  sensorString(rec[rdOffName : rdOffName+nameLen]),
			Unit:  sensorString(rec[rdOffUnit : rdOffUnit+unitLen]),
			Value: math.Float64frombits(binary.LittleEndian.Uint64(rec[rdOffValue:])),
		})
	}
	return out
}

// Snapshot includes the producer's poll_time (__time64_t at offset 12 in
// the packed HWiNFO shared-memory header). Re-reading an unchanged mapping
// must not turn an old sample into a fresh one after HWiNFO exits or pauses.
func (s *Source) Snapshot() ([]Reading, time.Time) {
	if s.view == 0 {
		return nil, time.Time{}
	}
	header := s.copy(0, hdrMinSize)
	signature := string(header[:4])
	if signature != "HWiS" && signature != "SiWH" {
		return nil, time.Time{}
	}
	seconds := int64(binary.LittleEndian.Uint64(header[12:20]))
	if seconds <= 0 {
		return nil, time.Time{}
	}
	readings := s.Readings()
	after := s.copy(0, hdrMinSize)
	if binary.LittleEndian.Uint64(after[12:20]) != uint64(seconds) || string(after[:4]) != signature {
		return nil, time.Time{} // producer changed the table while we copied it
	}
	return readings, time.Unix(seconds, 0)
}

// copy snapshots [at, at+n) of the mapping into a Go buffer with a
// single memcpy. The mapped address is never converted to a Go pointer
// (it belongs to the OS, not the GC), which keeps the pointer-integrity
// checker honest and gives the parser a stable, non-torn table.
func (s *Source) copy(at, n int) []byte {
	buf := make([]byte, n)
	dst := uintptr(unsafe.Pointer(unsafe.SliceData(buf)))
	procMoveMemHW.Call(dst, s.view+uintptr(at), uintptr(n))
	return buf
}

// sensorString decodes a NUL-terminated fixed-width field. Current HWiNFO
// publishes UTF-8 (including the two-byte degree sign); older feeds can
// contain a single-byte Latin-1 degree sign. Decoding UTF-8 as Latin-1
// corrupts "°C" into "Â°C", which makes temperature selection reject it.
func sensorString(b []byte) string {
	n := 0
	for n < len(b) && b[n] != 0 {
		n++
	}
	if utf8.Valid(b[:n]) {
		return string(b[:n])
	}
	out := make([]rune, n)
	for i := 0; i < n; i++ {
		out[i] = rune(b[i])
	}
	return string(out)
}
