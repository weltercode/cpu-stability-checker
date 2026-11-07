//go:build windows

package hwinfo

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func TestDumpLive(t *testing.T) {
	s, err := Open()
	if err != nil {
		t.Skipf("no hwinfo: %v", err)
	}
	defer s.Close()
	header := s.copy(0, hdrMinSize)
	t.Logf("signature=%q version=%d revision=%d poll_time=%d now=%d header=%x", header[:4], binary.LittleEndian.Uint32(header[4:]), binary.LittleEndian.Uint32(header[8:]), binary.LittleEndian.Uint64(header[12:20]), time.Now().Unix(), header)
	_, at := s.Snapshot()
	t.Logf("snapshot time=%v age=%v", at, time.Since(at))
	rs := s.Readings()
	t.Logf("readings=%d", len(rs))
	for _, r := range rs {
		n := strings.ToLower(r.Name)
		if r.Type == TypeTemp || strings.Contains(n, "clock") || strings.Contains(n, "tctl") ||
			strings.Contains(n, "tdie") || r.Type == TypePower ||
			strings.Contains(n, "vid") || strings.Contains(n, "package") {
			t.Logf("type=%d idx=%d id=%d name=%q unit=%q value=%.2f", r.Type, r.Index, r.ID, r.Name, r.Unit, r.Value)
		}
	}
	t.Logf("core clocks: %v", CoreClocks(rs))
	if temp, ok := Find(rs, TypeTemp, "tctl", "tdie", "cpu package"); ok {
		t.Logf("temp: %q = %.1f %s", temp.Name, temp.Value, temp.Unit)
	}
	if p, ok := Find(rs, TypePower, "cpu package", "package power"); ok {
		t.Logf("power: %q = %.1f %s", p.Name, p.Value, p.Unit)
	}
	if v, ok := Find(rs, TypeVolt, "cpu core vid", "core vid", "vcore"); ok {
		t.Logf("volt: %q = %.3f %s", v.Name, v.Value, v.Unit)
	}
}
