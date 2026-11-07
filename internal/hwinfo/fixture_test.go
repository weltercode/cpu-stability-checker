//go:build windows

package hwinfo

import (
	"encoding/binary"
	"math"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// The live HWiNFO feed is optional hardware state, so the published
// layout itself is pinned here against a real Windows mapping object:
// the test builds a synthetic sensor table (same pack=1 layout), maps
// it under a local-namespace name, and parses it back. If HWiNFO ever
// changes the layout, the same offsets break here first.
func TestReadingsFromSyntheticMapping(t *testing.T) {
	const off, stride, count = 4096, 316, 4
	buf := make([]byte, off+count*stride)
	binary.LittleEndian.PutUint32(buf[0:], 0x4E495748) // 'HWIN'
	binary.LittleEndian.PutUint32(buf[hdrReadOffset:], off)
	binary.LittleEndian.PutUint32(buf[hdrReadSize:], stride)
	binary.LittleEndian.PutUint32(buf[hdrReadCount:], count)
	put := func(i uint32, typ uint32, id uint32, sensor, unit string, val float64) {
		rec := buf[off+int(i)*stride:]
		binary.LittleEndian.PutUint32(rec[rdOffType:], typ)
		binary.LittleEndian.PutUint32(rec[rdOffIndex:], i)
		binary.LittleEndian.PutUint32(rec[rdOffID:], id)
		copy(rec[rdOffName:], sensor)
		copy(rec[rdOffUnit:], unit)
		binary.LittleEndian.PutUint64(rec[rdOffValue:], math.Float64bits(val))
	}
	put(0, TypeClock, 100, "Core 0 Clock (perf #4)", "MHz", 5600)
	put(1, TypeTemp, 101, "CPU (Tctl/Tdie)", "\xB0C", 72.5) // ANSI degree sign
	put(2, TypePower, 102, "CPU Package", "W", 118)
	put(3, TypeTemp, 103, "CPU Package", "°C", 70) // live HWiNFO UTF-8 unit

	cleanup := createFixture(t, "cpu_test_hwinfo_fixture", buf)
	defer cleanup()

	s, err := openNamed("cpu_test_hwinfo_fixture")
	if err != nil {
		t.Fatalf("openNamed: %v", err)
	}
	defer s.Close()

	rs := s.Readings()
	if len(rs) != count {
		t.Fatalf("got %d readings, want %d", len(rs), count)
	}
	if rs[0].Name != "Core 0 Clock (perf #4)" || rs[0].Value != 5600 || rs[0].ID != 100 {
		t.Errorf("clock reading parsed wrong: %+v", rs[0])
	}
	if rs[1].Name != "CPU (Tctl/Tdie)" || rs[1].Unit != "°C" {
		t.Errorf("temp reading parsed wrong (ANSI unit?): %+v", rs[1])
	}
	if rs[3].Name != "CPU Package" || rs[3].Unit != "°C" || rs[3].Value != 70 {
		t.Errorf("UTF-8 temperature parsed wrong: %+v", rs[3])
	}
	if clocks := CoreClocks(rs); clocks[0] != 5600 {
		t.Errorf("CoreClocks on real layout: %v", clocks)
	}
	if r, ok := Find(rs, TypeTemp, "tctl"); !ok || r.Value != 72.5 {
		t.Errorf("Find temp on real layout: %+v %v", r, ok)
	}
	if r, ok := Find(rs, TypePower, "cpu package"); !ok || r.Value != 118 {
		t.Errorf("Find power on real layout: %+v %v", r, ok)
	}
}

func TestSensorString(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"°C\x00ignored", "°C"},
		{"\xb0C\x00ignored", "°C"},
		{"Температура CPU\x00", "Температура CPU"},
		{"MHz", "MHz"},
		{"\x00", ""},
	} {
		if got := sensorString([]byte(tc.input)); got != tc.want {
			t.Errorf("sensorString(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestSnapshotPreservesProducerTimeAndRejectsInactiveMapping(t *testing.T) {
	const off, stride = 4096, 316
	for _, signature := range []string{"HWiS", "DEAD"} {
		t.Run(signature, func(t *testing.T) {
			buf := make([]byte, off+stride)
			copy(buf[:4], signature)
			at := time.Now().Truncate(time.Second)
			binary.LittleEndian.PutUint64(buf[12:20], uint64(at.Unix()))
			binary.LittleEndian.PutUint32(buf[hdrReadOffset:], off)
			binary.LittleEndian.PutUint32(buf[hdrReadSize:], stride)
			binary.LittleEndian.PutUint32(buf[hdrReadCount:], 1)
			rec := buf[off:]
			binary.LittleEndian.PutUint32(rec[rdOffType:], TypeTemp)
			copy(rec[rdOffName:], "CPU Package")
			copy(rec[rdOffUnit:], "C")
			binary.LittleEndian.PutUint64(rec[rdOffValue:], math.Float64bits(70))
			name := "cpu_test_timestamp_" + signature
			cleanup := createFixture(t, name, buf)
			defer cleanup()
			s, err := openNamed(name)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			values, stamp := s.Snapshot()
			if signature == "DEAD" {
				if len(values) != 0 || !stamp.IsZero() {
					t.Fatal("inactive mapping returned live samples")
				}
				return
			}
			if len(values) != 1 || !stamp.Equal(at) {
				t.Fatalf("producer timestamp lost: %+v %v", values, stamp)
			}
			time.Sleep(time.Millisecond)
			_, again := s.Snapshot()
			if !again.Equal(stamp) {
				t.Fatal("cached snapshot falsely refreshed producer time")
			}
		})
	}
}

// A header that does not make sense must be refused, not parsed.
func TestOpenRejectsBadHeader(t *testing.T) {
	buf := make([]byte, 64) // zero header: count=0
	cleanup := createFixture(t, "cpu_test_hwinfo_broken", buf)
	defer cleanup()
	if _, err := openNamed("cpu_test_hwinfo_broken"); err == nil {
		t.Fatal("zero-count header must be rejected")
	}
}

// createFixture maps content as a named local-namespace section and
// copies it in via memcpy (the view address never becomes a Go
// pointer, same discipline as the production reader).
func createFixture(t *testing.T, name string, content []byte) (cleanup func()) {
	t.Helper()
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	pCreate := kernel32.NewProc("CreateFileMappingW")
	pMap := kernel32.NewProc("MapViewOfFile")
	pUnmap := kernel32.NewProc("UnmapViewOfFile")
	pClose := kernel32.NewProc("CloseHandle")
	pMove := kernel32.NewProc("RtlMoveMemory")
	namep, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	h, _, e := pCreate.Call(^uintptr(0), 0, 0x04, 0, uintptr(len(content)), uintptr(unsafe.Pointer(namep)))
	if h == 0 {
		t.Skipf("cannot create fixture mapping: %v", e)
	}
	view, _, e := pMap.Call(h, 0x02, 0, 0, uintptr(len(content)))
	if view == 0 {
		pClose.Call(h)
		t.Fatalf("map fixture: %v", e)
	}
	pMove.Call(view, uintptr(unsafe.Pointer(unsafe.SliceData(content))), uintptr(len(content)))
	return func() { pUnmap.Call(view); pClose.Call(h) }
}
