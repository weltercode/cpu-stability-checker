//go:build windows

package tui

import (
	"time"
	"unsafe"
)

var (
	procNumInputEvents = kernel32TUI.NewProc("GetNumberOfConsoleInputEvents")
	procReadInputEv    = kernel32TUI.NewProc("ReadConsoleInputW")
)

// stdInputHandle is (DWORD)-10 = STD_INPUT_HANDLE. (The output handle
// constant once cost a whole broken screen UI by being off by one -
// pin this one by test too.)
const stdInputHandle = 0xFFFFFFF6

// INPUT_RECORD on amd64 is 20 bytes: WORD EventType, padding, then a
// 16-byte event union. KEY_EVENT_RECORD inside it: bKeyDown DWORD,
// wRepeatCount WORD, wVirtualKeyCode WORD, wVirtualScanCode WORD,
// UnicodeChar WORD, dwControlKeyState DWORD.
const (
	inputRecordSize = 20
	keyEvent        = 0x0001
	vkEscape        = 0x001B
)

// escKeyDown reports whether one raw INPUT_RECORD is an Escape
// key-down. Pure so the byte layout is pinned by table tests.
func escKeyDown(rec []byte) bool {
	if len(rec) < inputRecordSize {
		return false
	}
	if uint16(rec[0])|uint16(rec[1])<<8 != keyEvent {
		return false
	}
	keyDown := uint32(rec[4]) | uint32(rec[5])<<8 | uint32(rec[6])<<16 | uint32(rec[7])<<24
	vk := uint16(rec[4+6]) | uint16(rec[4+7])<<8
	return keyDown != 0 && vk == vkEscape
}

// EscapeWatcher calls onEscape the first time the user presses Escape
// in the console. While it runs it drains pending input events: the
// menu's line reader is idle during a run, and draining keeps stray
// keystrokes out of the next prompt. Ctrl+C keeps working untouched -
// its signal comes from the console driver, not from reading events.
type EscapeWatcher struct {
	stop chan struct{}
	done chan struct{}
}

// WatchEscape starts watching; Stop ends it. onEscape runs at most
// once.
func WatchEscape(onEscape func()) *EscapeWatcher {
	w := &EscapeWatcher{stop: make(chan struct{}), done: make(chan struct{})}
	go w.run(onEscape)
	return w
}

func (w *EscapeWatcher) run(onEscape func()) {
	defer close(w.done)
	in, _, _ := procGetStdHandleT.Call(stdInputHandle)
	if in == 0 || in == ^uintptr(0) {
		return
	}
	const maxRecs = 16
	buf := make([]byte, maxRecs*inputRecordSize)
	fired := false
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-tick.C:
		}
		var n uint32
		if r, _, _ := procNumInputEvents.Call(in, uintptr(unsafe.Pointer(&n))); r == 0 {
			return // not a console input handle after all
		}
		for n > 0 {
			want := n
			if want > maxRecs {
				want = maxRecs
			}
			var got uint32
			if r, _, _ := procReadInputEv.Call(in, uintptr(unsafe.Pointer(&buf[0])),
				uintptr(want), uintptr(unsafe.Pointer(&got))); r == 0 {
				return
			}
			if got == 0 {
				return
			}
			for i := 0; i < int(got) && !fired; i++ {
				if escKeyDown(buf[i*inputRecordSize : (i+1)*inputRecordSize]) {
					fired = true
					onEscape()
				}
			}
			n -= got
		}
	}
}

// Stop ends the watcher and waits for it to leave.
func (w *EscapeWatcher) Stop() {
	close(w.stop)
	<-w.done
}
