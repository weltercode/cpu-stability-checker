//go:build linux

package tui

import (
	"os"
	"syscall"
	"unsafe"
)

// POSIX terminals speak ANSI natively — if the caller already passed the
// terminal check, cursor movement works without any setup.
func enableVT(*os.File) bool { return true }

type winsize struct {
	rows, cols, x, y uint16
}

// Size asks the tty for its dimensions via TIOCGWINSZ.
func Size(f *os.File) (cols, rows int, ok bool) {
	var ws winsize
	r, _, _ := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ,
		uintptr(unsafe.Pointer(&ws)))
	if r != 0 || ws.cols == 0 || ws.rows == 0 {
		return 0, 0, false
	}
	return int(ws.cols), int(ws.rows), true
}

// SizeGeometry reports the tty size as win/buf/max alike: a POSIX tty
// has one geometry, no separate window-rect claim to distrust.
func SizeGeometry(f *os.File) Geometry {
	cols, rows, ok := Size(f)
	if !ok {
		return Geometry{}
	}
	return Geometry{
		WinCols: cols, WinRows: rows,
		BufCols: cols, BufRows: rows,
		MaxCols: cols, MaxRows: rows,
		PickCols: cols, PickRows: rows,
		OK: true,
	}
}

// QueryCONOUT is a Windows concept; there is no second handle here.
func QueryCONOUT() Geometry { return Geometry{} }
