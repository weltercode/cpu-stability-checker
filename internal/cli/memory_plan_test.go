package cli

import (
	"bytes"
	"strings"
	"testing"
)

// R28 (owner complaint 2026-11-12: the plan screen printed only the
// raw word "auto" - nobody could see how many bytes the engine would
// actually take). A resolved request must show the computed bytes and
// the rule that produced them, labeled as the plan-time estimate it
// is, and say what stays authoritative (recompute at phase start,
// exact grants, no silent shrink). The assertions hold regardless of
// whether THIS machine admits or refuses at this instant - both
// outcomes are honest and must be visible.
func TestMemoryPlanLinesAutoShowsComputedEstimate(t *testing.T) {
	var buf bytes.Buffer
	memoryPlanLines(&buf, "auto", "try", 7, "")
	out := buf.String()
	if !strings.Contains(out, `budget "auto", lock "try", seed 7`) {
		t.Errorf("request line lost what was asked:\n%s", out)
	}
	// Either the computed number with its rule, or an explicit
	// not-computed refusal - never silence, never a bare "auto".
	estimate := strings.Contains(out, "budget estimate at plan time:") ||
		strings.Contains(out, "budget estimate: not computed")
	if !estimate {
		t.Errorf("no honest estimate line for auto:\n%s", out)
	}
	if strings.Contains(out, "budget estimate at plan time:") {
		if !strings.Contains(out, "(auto:") {
			t.Errorf("estimate line does not state the binding rule:\n%s", out)
		}
		if !strings.Contains(out, "estimate only") {
			t.Errorf("the number is shown without its estimate label:\n%s", out)
		}
	}
}

// "max" (R28) must be shown as its own mode with its own rule, so it
// can never masquerade as auto's bounded share.
func TestMemoryPlanLinesMaxStatesMaxRule(t *testing.T) {
	var buf bytes.Buffer
	memoryPlanLines(&buf, "max", "", 0, "")
	out := buf.String()
	if !strings.Contains(out, `budget "max", lock "off"`) {
		t.Errorf("max request line:\n%s", out)
	}
	if strings.Contains(out, "budget estimate at plan time:") {
		if !strings.Contains(out, "(max:") {
			t.Errorf("max estimate does not carry the max rule:\n%s", out)
		}
	} else if !strings.Contains(out, "budget estimate: not computed") {
		t.Errorf("no honest estimate line for max:\n%s", out)
	}
}

// A fixed request promises exactness: the screen shows the exact
// byte count and the no-silent-shrink rule, and never dresses a
// fixed size up as an "estimate".
func TestMemoryPlanLinesFixedIsExact(t *testing.T) {
	var buf bytes.Buffer
	memoryPlanLines(&buf, "512MiB", "off", 3, "")
	out := buf.String()
	if !strings.Contains(out, `budget "512MiB", lock "off"`) {
		t.Errorf("fixed request line:\n%s", out)
	}
	if !strings.Contains(out, "exactly 512.0 MiB") {
		t.Errorf("fixed request lost its exact byte count:\n%s", out)
	}
	if !strings.Contains(out, "never quietly shrunk") {
		t.Errorf("fixed request lost the no-silent-shrink promise:\n%s", out)
	}
	if strings.Contains(out, "estimate") {
		t.Errorf("an exact request was labeled an estimate:\n%s", out)
	}
}

// Engine off stays loud (audit A05): no budget configured is said in
// words, and a malformed value that somehow reached a plan screen is
// shown as written with its error, never guessed around.
func TestMemoryPlanLinesOffAndMalformed(t *testing.T) {
	var buf bytes.Buffer
	memoryPlanLines(&buf, "", "", 0, "")
	if !strings.Contains(buf.String(), "memory engine: off") {
		t.Errorf("off state not stated:\n%s", buf.String())
	}

	buf.Reset()
	memoryPlanLines(&buf, "5mib", "off", 0, "  ")
	out := buf.String()
	if !strings.Contains(out, `"5mib"`) {
		t.Errorf("the malformed value was not shown as written:\n%s", out)
	}
	if !strings.Contains(out, "not computed") {
		t.Errorf("a malformed budget was smoothed over:\n%s", out)
	}
	if strings.Contains(out, "estimate at plan time") {
		t.Errorf("a malformed budget produced a number anyway:\n%s", out)
	}
}
