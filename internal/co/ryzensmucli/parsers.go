package ryzensmucli

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/weltercode/cpu-stability-checker/internal/co"
)

// Parsers pinned to Program.cs of the probed version. Anything that
// does not match the pinned shapes is malformed_output - the adapter
// never guesses an unknown answer, and a different tool version that
// changed its output fails loudly instead of misreading.

var (
	// reVersion matches the MEASURED --version output of the pinned
	// build (read-only smoke on real hardware, 2026-10-18): the .NET
	// informational version printed bare, as <semver>+<SourceLink git
	// sha>. The earlier shape (^ryzen-smu-cli <ver>) was inferred from
	// Program.cs and was WRONG: the real build prints no name prefix,
	// so it parsed to "" (unknown) and the fingerprint never formed.
	// The embedded 40-hex commit is the build's own SourceLink
	// identity, so the version string itself carries source proof.
	reVersion   = regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+)\+([0-9a-f]{40})$`)
	reCoreLine  = regexp.MustCompile(`^Core (\d+): (Enabled|Disabled)$`)
	reSetLine   = regexp.MustCompile(`^Set logical core (\d+), physical core (\d+) offset to (-?\d+)!$`)
	reDisabled  = regexp.MustCompile(`^You have manually disabled cores\. It is not possible to retrieve offset values while they are disabled\.$`)
	offsetsHead = "Current PBO offsets:"
	fusedHead   = "Fused status of physical cores:"
)

// parseVersion returns the tool version or "" (unknown) when the
// answer does not match the pinned shape - never a guessed default.
// The version is the semver part of the informational version; the
// +<commit> metadata is read separately (parseVersionCommit).
func parseVersion(out string) string {
	for _, line := range nonEmptyLines(out) {
		if m := reVersion.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// parseVersionCommit returns the SourceLink git sha embedded in the
// informational version (the +<40-hex> build metadata), or "" when
// the answer does not carry it. It is the build's own claim of the
// exact source commit it was compiled from.
func parseVersionCommit(out string) string {
	for _, line := range nonEmptyLines(out) {
		if m := reVersion.FindStringSubmatch(line); m != nil {
			return m[2]
		}
	}
	return ""
}

// parseCoreList reads "Core N: Enabled|Disabled" lines (with the
// optional fused header) into a dense per-physical-core bool slice.
// Indices must be dense from 0; any other line is malformed.
func parseCoreList(out string) ([]bool, error) {
	var res []bool
	for _, line := range nonEmptyLines(out) {
		if line == fusedHead {
			continue
		}
		m := reCoreLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("unrecognized line %q", line)
		}
		idx, err := strconv.Atoi(m[1])
		if err != nil || idx != len(res) {
			return nil, fmt.Errorf("core index %q not dense at %d", m[1], len(res))
		}
		res = append(res, m[2] == "Enabled")
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("no core lines")
	}
	return res, nil
}

// parseOffsetsTerse reads the header plus one comma-separated value
// line. The disabled-cores note makes the read PARTIAL (some cores
// unreadable), not an error and not a full read.
func parseOffsetsTerse(out string) (vals []int, partial bool, note string, err error) {
	lines := nonEmptyLines(out)
	if len(lines) == 0 || lines[0] != offsetsHead {
		return nil, false, "", fmt.Errorf("missing %q header", offsetsHead)
	}
	if len(lines) < 2 {
		return nil, false, "", fmt.Errorf("no offsets line")
	}
	csv := lines[1]
	if csv != "" {
		for _, tok := range strings.Split(csv, ",") {
			v, cerr := strconv.Atoi(strings.TrimSpace(tok))
			if cerr != nil {
				return nil, false, "", fmt.Errorf("offset token %q not an integer", tok)
			}
			vals = append(vals, v)
		}
	}
	for _, extra := range lines[2:] {
		if reDisabled.MatchString(extra) {
			partial = true
			note = "some cores disabled: their offsets are unreadable while disabled"
			continue
		}
		return nil, false, "", fmt.Errorf("unrecognized trailing line %q", extra)
	}
	return vals, partial, note, nil
}

// checkApplyLog cross-reads the tool's "Set logical core ..." lines
// against the request and the topology (audit B05). The lines are a
// CALL LOG, not confirmation: Program.cs prints them after
// SetPsmMarginSingleCore without checking the result, so agreement
// only shows the tool believes it called the SMU for that core and
// value. Disagreements - unrequested cores, wrong values, wrong
// physical indices, duplicates, missing lines - come back as notes
// for the caller to surface. The count is the number of parsed
// lines, nothing more.
func checkApplyLog(out string, want co.Profile, slots []co.CoreSlot) (int, []string) {
	type exp struct {
		val  int
		smu  int
		seen bool
	}
	wantMap := map[int]*exp{}
	for _, o := range want.Offsets {
		idx, ok := toolIDX(o.Core)
		if !ok {
			continue // Validate guarantees the shape; defensive anyway
		}
		smu := -1
		if idx < len(slots) && slots[idx].SMUIndex != nil {
			smu = *slots[idx].SMUIndex
		}
		wantMap[idx] = &exp{val: o.Value, smu: smu}
	}
	var notes []string
	logged := 0
	for _, line := range nonEmptyLines(out) {
		m := reSetLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		logged++
		lc, _ := strconv.Atoi(m[1])
		phys, _ := strconv.Atoi(m[2])
		val, _ := strconv.Atoi(m[3])
		e, ok := wantMap[lc]
		if !ok {
			notes = append(notes, fmt.Sprintf("tool logged a Set call for logical core %d which was not requested", lc))
			continue
		}
		if e.seen {
			notes = append(notes, fmt.Sprintf("duplicate Set call logged for logical core %d", lc))
		}
		e.seen = true
		if val != e.val {
			notes = append(notes, fmt.Sprintf("logged value %d for logical core %d differs from the requested %d", val, lc, e.val))
		}
		if e.smu >= 0 && phys != e.smu {
			notes = append(notes, fmt.Sprintf("logged physical core %d for logical core %d differs from our topology's SMU index %d", phys, lc, e.smu))
		}
	}
	for lc, e := range wantMap {
		if !e.seen {
			notes = append(notes, fmt.Sprintf("no Set call logged for logical core %d", lc))
		}
	}
	return logged, notes
}

// toolIDX parses our own tool-id shape ("coreN") strictly: anything
// else (garbage, negative, huge) is not an index, never a silent 0.
func toolIDX(id string) (int, bool) {
	if !strings.HasPrefix(id, "core") || len(id) <= 4 {
		return 0, false
	}
	n, err := strconv.Atoi(id[4:])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func nonEmptyLines(out string) []string {
	var res []string
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			res = append(res, strings.TrimSpace(line))
		}
	}
	return res
}
