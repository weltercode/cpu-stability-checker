//go:build windows

package tui

import "testing"

// The handle constant and record layout are the whole feature; an
// off-by-one here silently disables Escape (the screen UI once died
// the same way from a wrong output-handle constant).
func TestInputConstants(t *testing.T) {
	if stdInputHandle != ^uint32(9) { // (DWORD)-10
		t.Errorf("stdInputHandle = %#x, want %#x ((DWORD)-10)", stdInputHandle, ^uint32(9))
	}
}

func TestEscKeyDown(t *testing.T) {
	rec := func(typ uint16, down uint32, vk uint16) []byte {
		b := make([]byte, inputRecordSize)
		b[0], b[1] = byte(typ), byte(typ>>8)
		b[4], b[5], b[6], b[7] = byte(down), byte(down>>8), byte(down>>16), byte(down>>24)
		b[10], b[11] = byte(vk), byte(vk>>8)
		return b
	}
	cases := []struct {
		name string
		rec  []byte
		want bool
	}{
		{"escape down", rec(keyEvent, 1, vkEscape), true},
		{"escape up", rec(keyEvent, 0, vkEscape), false},
		{"enter down", rec(keyEvent, 1, 0x0D), false},
		{"mouse event with esc bytes", rec(0x0002, 1, vkEscape), false},
		{"short record", []byte{1, 0, 0, 0}, false},
	}
	for _, tt := range cases {
		if got := escKeyDown(tt.rec); got != tt.want {
			t.Errorf("%s: got %v want %v", tt.name, got, tt.want)
		}
	}
}
