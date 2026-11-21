//go:build windows

package tui

import (
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var procRaiseCtrlC = kernel32TUI.NewProc("GenerateConsoleCtrlEvent")

const (
	enableProcessedInput = 0x0001
	enableLineInput      = 0x0002
	enableEchoInput      = 0x0004
	enableQuickEdit      = 0x0040
	enableExtendedFlags  = 0x0080

	vkLeft   = 0x25
	vkUp     = 0x26
	vkRight  = 0x27
	vkDown   = 0x28
	vkReturn = 0x0D
	vkBack   = 0x08
)

// rawInputMode computes the console mode for a navigating menu from
// the current one. Pure so the flag algebra is pinned by table tests.
//
// Line buffering and console echo go OFF (arrows must arrive as
// events the instant they are pressed; the screen paints its own
// echo). PROCESSED_INPUT also goes OFF: with it ON, Ctrl+C would be
// turned into a CTRL_C_EVENT that kills the process while the mode
// is still raw, leaking line-buffering-off to the shell; OFF, Ctrl+C
// arrives as an ordinary 0x03 key event the caller answers by
// restoring the mode FIRST and re-raising the signal after (see
// nav.go). Quick-edit is turned OFF for the same class of reason - a
// stray mouse drag would pause event delivery while a menu waits -
// which requires ENABLE_EXTENDED_FLAGS; the restore sets the exact
// previous mode back. Mouse and window (resize) records stay on:
// NextKey drops them, the geometry watcher is separate. No other bit
// is touched.
func rawInputMode(mode uint32) uint32 {
	return (mode &^ (enableProcessedInput | enableLineInput | enableEchoInput | enableQuickEdit)) | enableExtendedFlags
}

// keyFromEvent decodes one raw INPUT_RECORD into a Key. Pure so the
// byte layout is pinned by table tests (same discipline as
// escKeyDown). Key-UP records are KeyNone on purpose: acting on both
// halves would move the cursor twice per press. Ctrl+Z decodes to
// KeyLost: in cooked mode it is console EOF and every menu read
// treats exhausted input as quit, so the honest equivalent here is
// exhausted input too - never a bare Enter, which would silently
// fire the screen's default action.
func keyFromEvent(rec []byte) (Key, rune) {
	if len(rec) < inputRecordSize {
		return KeyNone, 0
	}
	if uint16(rec[0])|uint16(rec[1])<<8 != keyEvent {
		return KeyNone, 0
	}
	keyDown := uint32(rec[4]) | uint32(rec[5])<<8 | uint32(rec[6])<<16 | uint32(rec[7])<<24
	if keyDown == 0 {
		return KeyNone, 0
	}
	vk := uint16(rec[4+6]) | uint16(rec[4+7])<<8
	uc := rune(uint16(rec[4+10]) | uint16(rec[4+11])<<8)
	switch vk {
	case vkUp:
		return KeyUp, 0
	case vkDown:
		return KeyDown, 0
	case vkLeft:
		return KeyLeft, 0
	case vkRight:
		return KeyRight, 0
	case vkReturn:
		return KeyEnter, 0
	case vkEscape:
		return KeyEsc, 0
	case vkBack:
		return KeyBackspace, 0
	}
	if uc == 3 { // Ctrl+C arriving as an event (PROCESSED_INPUT off)
		return KeyInterrupt, 0
	}
	if uc == 0x1A { // Ctrl+Z: console EOF, same meaning as a lost handle
		return KeyLost, 0
	}
	if uc >= 32 && uc != 0x7F {
		return KeyRune, uc
	}
	return KeyOther, 0
}

// keyRepeat reads wRepeatCount from a key-down record. Windows
// coalesces the same key held or typed faster than it is read into
// ONE record counting N presses: ignoring the count would silently
// turn a typed "11" into "1". Min one, whatever the console sent.
func keyRepeat(rec []byte) uint32 {
	if len(rec) < inputRecordSize {
		return 1
	}
	n := uint32(uint16(rec[4+4]) | uint16(rec[4+5])<<8)
	if n < 1 {
		return 1
	}
	return n
}

// RawKeys is the ONE owner of the console input events for as long
// as a navigating menu waits. There is no goroutine and no channel:
// the single goroutine that will answer the prompt (navChoice) pulls
// events one record at a time with NextKey and stops pulling the
// moment its answer is complete. Anything the operator queued beyond
// that answer stays untouched in the console queue, reaches the next
// prompt exactly as cooked-mode typeahead reaches it today, and the
// mode is restored before that next prompt starts. The review-driven
// design rule: never read past the answer, never hand events to a
// second party, never leave the mode changed.
type RawKeys struct {
	handle   uintptr
	prevMode uint32
	changed  bool
	restore  sync.Once // the one mode restore, shared with the ctrl handler
}

// activeRaw carries the live RawKeys to the console control handler
// installed for as long as the mode is switched. There is only ever
// one navigating menu on one goroutine, so one atomic slot is the
// whole registration.
var activeRaw atomic.Pointer[RawKeys]

var procSetConsoleCtrlHandler = kernel32TUI.NewProc("SetConsoleCtrlHandler")

// consoleCtrlHandler runs when the console sends CTRL_C_EVENT,
// CTRL_BREAK_EVENT or CTRL_CLOSE_EVENT. Ctrl+Break is processed by
// the console independently of PROCESSED_INPUT, so it can terminate
// the process WITHOUT any Go defer running - the raw mode would leak
// to the shell that owns this console. Restoring here (through the
// same once as Stop) covers every such route; returning 0 (FALSE)
// keeps the runtime's normal signal behavior exactly as before R42.
func consoleCtrlHandler(uint32) uintptr {
	if rk := activeRaw.Load(); rk != nil {
		rk.restoreMode()
	}
	return 0
}

// ctrlHandlerProc is the registered callback address; keeping it in
// a package var pins it alive for the whole process.
var ctrlHandlerProc = syscall.NewCallback(consoleCtrlHandler)

// StartRawKeys switches stdin's console mode for event input (see
// rawInputMode). If stdin is not a console input handle (pipe, file,
// redirected input) nothing changes and nil comes back: the caller
// keeps its buffered line reader, byte-identical to before R42. If
// the Ctrl/Break handler cannot be installed, raw mode is refused
// entirely - navigating without it could leak the mode on Ctrl+Break.
func StartRawKeys() *RawKeys {
	in, _, _ := procGetStdHandleT.Call(stdInputHandle)
	if in == 0 || in == ^uintptr(0) {
		return nil
	}
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(in, uintptr(unsafe.Pointer(&mode))); r == 0 {
		return nil // not a console: keep the old input path entirely
	}
	newMode := rawInputMode(mode)
	rk := &RawKeys{handle: in}
	if newMode != mode {
		if r, _, _ := procSetConsoleMode.Call(in, uintptr(newMode)); r == 0 {
			return nil // mode change refused: keep the old input path
		}
		rk.prevMode, rk.changed = mode, true
	}
	if r, _, _ := procSetConsoleCtrlHandler.Call(ctrlHandlerProc, 1); r == 0 {
		rk.restoreMode() // refuse raw: put the console back exactly
		return nil
	}
	activeRaw.Store(rk)
	return rk
}

// NextKey blocks until the next key-DOWN event and returns it with
// its repeat count (see keyRepeat). Key-UP, mouse and resize records
// are dropped here, so the caller only ever sees presses with
// meaning. KeyLost means the input handle died or the operator sent
// console EOF (Ctrl+Z): the caller treats it exactly like exhausted
// stdin, as every other read path does.
func (rk *RawKeys) NextKey() (Key, rune, uint32) {
	var rec [inputRecordSize]byte
	for {
		var n uint32
		if r, _, _ := procNumInputEvents.Call(rk.handle, uintptr(unsafe.Pointer(&n))); r == 0 {
			return KeyLost, 0, 1
		}
		if n == 0 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		// Exactly one record: reading past the caller's answer would
		// eat the typeahead that belongs to the NEXT prompt (that is
		// how a stray Enter could ever reach a consent screen).
		var got uint32
		if r, _, _ := procReadInputEv.Call(rk.handle, uintptr(unsafe.Pointer(&rec[0])),
			1, uintptr(unsafe.Pointer(&got))); r == 0 {
			return KeyLost, 0, 1
		}
		if got == 0 {
			continue
		}
		if k, ru := keyFromEvent(rec[:]); k != KeyNone {
			return k, ru, keyRepeat(rec[:])
		}
	}
}

// restoreMode puts the console mode back exactly once, from either
// Stop or the ctrl handler.
func (rk *RawKeys) restoreMode() {
	rk.restore.Do(func() {
		if rk.changed {
			procSetConsoleMode.Call(rk.handle, uintptr(rk.prevMode))
		}
	})
}

// Stop ends the raw window: unregisters the ctrl handler, restores
// the console mode exactly once. Safe to call from a defer on every
// return route.
func (rk *RawKeys) Stop() {
	if rk == nil {
		return
	}
	activeRaw.Store(nil)
	procSetConsoleCtrlHandler.Call(ctrlHandlerProc, 0)
	rk.restoreMode()
}

// RaiseCtrlC re-raises Ctrl+C through the console (used after
// Stop has restored the mode on an incoming Ctrl+C key event, so the
// process and its default signal behavior are exactly what they were
// before R42 - and the shell never sees a leaked raw mode).
func RaiseCtrlC() bool {
	r, _, _ := procRaiseCtrlC.Call(0, 0) // CTRL_C_EVENT, own process group
	return r != 0
}
