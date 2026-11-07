//go:build !windows && !linux

package tui

import "os"

func enableVT(*os.File) bool { return false }

func Size(*os.File) (cols, rows int, ok bool) { return 0, 0, false }

// SizeGeometry keeps the raw-observation shape honest on platforms
// with no console query: everything unknown.
func SizeGeometry(*os.File) Geometry { return Geometry{} }

// QueryCONOUT is a Windows concept; there is no second handle here.
func QueryCONOUT() Geometry { return Geometry{} }
