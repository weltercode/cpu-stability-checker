//go:build windows

package tui

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32TUI        = syscall.NewLazyDLL("kernel32.dll")
	procGetStdHandleT  = kernel32TUI.NewProc("GetStdHandle")
	procGetConsoleMode = kernel32TUI.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32TUI.NewProc("SetConsoleMode")
	procGetConsoleInfo = kernel32TUI.NewProc("GetConsoleScreenBufferInfo")
	procCreateFileTUI  = kernel32TUI.NewProc("CreateFileW")
	procCloseHandleTUI = kernel32TUI.NewProc("CloseHandle")
)

const (
	stdOutputHandle                 = 0xFFFFFFF5 // (DWORD)-11 = STD_OUTPUT_HANDLE, truncated by the call
	enableVirtualTerminalProcessing = 0x0004
)

// enableVT turns on ANSI processing for the output handle. Windows
// Terminal and modern conhost accept this; older consoles refuse and the
// caller keeps its plain append-mode fallback.
func enableVT(*os.File) bool {
	handle, _, _ := procGetStdHandleT.Call(stdOutputHandle)
	if handle == 0 || handle == ^uintptr(0) {
		return false
	}
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(handle, uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}
	r, _, _ := procSetConsoleMode.Call(handle, uintptr(mode|enableVirtualTerminalProcessing))
	return r != 0
}

// consoleScreenBufferInfo mirrors CONSOLE_SCREEN_BUFFER_INFO.
type consoleScreenBufferInfo struct {
	size          coord
	maxWindowSize coord
	cursor        coord
	attributes    uint16
	window        smallRect
}

type coord struct{ x, y int16 }
type smallRect struct {
	left, top, right, bottom int16
}

// Size returns the VISIBLE window's columns and rows - not the scroll
// buffer's. A conhost with scrollback keeps a buffer many times taller
// than the window; painting against the buffer height would push the
// prompt below what the user can see and let the live panel believe it
// owns hundreds of rows. Windows Terminal keeps the two equal, which
// is why the difference hides there.
//
// A degenerate window rect is the exception (see pickSize): trusting a
// 2x2 "window" rendered the panel as the fragment "lo" and nothing
// else - the user's screen, measured.
func Size(f *os.File) (cols, rows int, ok bool) {
	g := SizeGeometry(f)
	return g.PickCols, g.PickRows, g.OK
}

// SizeGeometry is Size's full raw observation: window rect, buffer,
// max window and the pick. Tracing needs the raw numbers because the
// pick alone cannot show which claim was wrong.
func SizeGeometry(*os.File) Geometry {
	handle, _, _ := procGetStdHandleT.Call(stdOutputHandle)
	if handle == 0 || handle == ^uintptr(0) {
		return Geometry{}
	}
	var info consoleScreenBufferInfo
	if r, _, _ := procGetConsoleInfo.Call(handle, uintptr(unsafe.Pointer(&info))); r == 0 {
		return Geometry{}
	}
	return geometryFromInfo(info)
}

// QueryCONOUT opens the console screen buffer afresh ("CONOUT$") and
// asks it the same question Size asks of the standard output handle.
// If the two handles disagree about the window rect, the lie is in
// the handle the tool normally paints through; if they agree, the
// host itself is reporting that geometry (a real resize, or the
// terminal choosing to report a collapsed rect while minimized or
// otherwise not paying attention). Worth the extra syscalls only
// while a size trace is on.
func QueryCONOUT() Geometry {
	name, err := syscall.UTF16PtrFromString("CONOUT$")
	if err != nil {
		return Geometry{}
	}
	h, _, _ := procCreateFileTUI.Call(
		uintptr(unsafe.Pointer(name)),
		0x80000000, // GENERIC_READ
		0x3,        // FILE_SHARE_READ|FILE_SHARE_WRITE
		0, 3,       // OPEN_EXISTING
		0, 0)
	if h == 0 || h == ^uintptr(0) {
		return Geometry{}
	}
	defer procCloseHandleTUI.Call(h)
	var info consoleScreenBufferInfo
	if r, _, _ := procGetConsoleInfo.Call(h, uintptr(unsafe.Pointer(&info))); r == 0 {
		return Geometry{}
	}
	return geometryFromInfo(info)
}

// geometryFromInfo turns one CONSOLE_SCREEN_BUFFER_INFO into a
// Geometry, raw numbers and pick included.
func geometryFromInfo(info consoleScreenBufferInfo) Geometry {
	g := Geometry{
		WinCols: int(info.window.right-info.window.left) + 1,
		WinRows: int(info.window.bottom-info.window.top) + 1,
		BufCols: int(info.size.x), BufRows: int(info.size.y),
		MaxCols: int(info.maxWindowSize.x), MaxRows: int(info.maxWindowSize.y),
		OK: true,
	}
	g.PickCols, g.PickRows, g.OK = pickSize(g.WinCols, g.WinRows, g.BufCols, g.BufRows)
	return g
}

// pickSize decides the usable viewport from the console's own numbers.
// Normally that is the visible window rect, exactly as documented
// above. But a window rect of a couple of columns/rows is never a
// viewport a human can read: it is what a console reports when it was
// created without a desktop, and - measured on the user's machine -
// what Windows Terminal keeps reporting for the WHOLE life of a window
// opened through the default-terminal handoff (a .lnk double-click):
// win stayed 2x2 from the first menu frame to the final one while the
// window on the screen was plainly ~120x30. For that case the buffer
// is the fallback: on that machine it read 120x30 and never moved for
// the entire run; conhost keeps buffer WIDTH equal to the window
// width (only the height grows for scrollback), and pty terminals
// keep buffer and window equal anyway.
//
// maxWindowSize deliberately plays no role. The same trace measured
// it walking through twenty different values in under two minutes
// (105x3 -> 105x21 -> 36x2 -> 36x27, with bursts changing every
// millisecond). Clamping the buffer by that noise is exactly what
// made the panel progressively lose its bars, then the clocks, then
// whole lines: every noise stage persisted for seconds - long enough
// for the geometry gate to honestly adopt it. The scrollback case
// the clamp was once meant for cannot reach this path: a conhost with
// a tall scrollback buffer has a healthy window rect, and the rect
// wins first. When even the buffer is degenerate, ok=false lets every
// caller keep its documented 80x25 assumption.
func pickSize(winCols, winRows, bufCols, bufRows int) (cols, rows int, ok bool) {
	if winCols >= 10 && winRows >= 4 {
		return winCols, winRows, true
	}
	if bufCols < 10 || bufRows < 4 {
		return 0, 0, false
	}
	return bufCols, bufRows, true
}
