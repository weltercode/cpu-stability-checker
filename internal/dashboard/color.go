package dashboard

import "fmt"

// Minimal SGR decoration, used only when Snapshot.Color is set - which
// only the VT console panel ever does. Pipes and classic consoles never
// see these codes, and stripping them reproduces the plain layout
// byte-for-byte. Basic 16-color foregrounds only: every VT-enabled
// conhost and Windows Terminal renders them; truecolor is not assumed.

func sgr(code int, s string) string {
	if s == "" {
		return s
	}
	return fmt.Sprintf("\x1b[%dm%s\x1b[0m", code, s)
}

// coreHues cycle so SMT siblings share one hue while neighbouring cores
// differ. Black (30) is excluded as invisible on dark terminals; gray
// (90) is reserved for "idle/unknown" values, so it stays out of the
// identity palette too. With more cores than hues, repeats are
// unavoidable - the cN/cN* labels carry the grouping regardless.
var coreHues = []int{36, 33, 35, 34, 32, 31, 37, 96}

func coreHue(core int) int { return coreHues[core%len(coreHues)] }

// tempHue shades the CPU temperature: comfortable, warm, hot.
func tempHue(t float64) int {
	switch {
	case t >= 75:
		return 91 // bright red
	case t >= 60:
		return 33 // yellow
	default:
		return 32 // green
	}
}

// mhzHue shades a measured clock against the firmware-rated base:
// clearly boosting, at base, in the middle band, or idle/deep-throttled
// (gray). Callers must only use it for real MHz with a known base.
func mhzHue(mhz, base float64) int {
	switch {
	case mhz >= 1.1*base:
		return 92 // bright green: boosting
	case mhz >= base:
		return 32 // green: at the rated base
	case mhz >= 0.5*base:
		return 33 // yellow: mid band
	default:
		return 90 // gray: idle or far below base
	}
}

// loadHue shades a busy% bar like htop does: calm, busy, pinned.
func loadHue(pct float64) int {
	switch {
	case pct >= 80:
		return 91
	case pct >= 50:
		return 33
	default:
		return 32
	}
}
