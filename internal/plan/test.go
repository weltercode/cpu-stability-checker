package plan

import (
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// Test is a pluggable stability test. A test only decides *which* workload
// runs on *which* core; the runner is responsible for actually scheduling it.
//
// The three categories match the tool's purpose:
//  1. general  — every core runs the same workload.
//  2. cyclic   — cores rotate through a set of workloads.
//  3. patterns — fixed workload patterns across the cores (corecycler style).
type Test struct {
	Category    string
	Name        string
	Description string
	NeedsTwo    bool // test uses both workload slots a and b
	Plan        func(cores int, a, b string) []string
}

// workloadSlots reports how many user-selected workloads a layout consumes.
// The cyclic layout chooses from the complete registry by itself.
func (t Test) workloadSlots() int {
	if t.Name == "cyclic" {
		return 0
	}
	if t.NeedsTwo {
		return 2
	}
	return 1
}

var tests = []*Test{
	{
		Category:    "1. General",
		Name:        "general",
		Description: "same workload on every selected logical CPU",
		NeedsTwo:    false,
		Plan: func(n int, a, _ string) []string {
			p := make([]string, n)
			for i := range p {
				p[i] = a
			}
			return p
		},
	},
	{
		Category:    "2. Cyclic",
		Name:        "cyclic",
		Description: "distribute all built-in workloads across the selected logical CPUs",
		NeedsTwo:    false,
		Plan: func(n int, _, _ string) []string {
			names := workload.Names()
			p := make([]string, n)
			for i := range p {
				p[i] = names[i%len(names)]
			}
			return p
		},
	},
	{
		Category:    "3. Patterns",
		Name:        "abab",
		Description: "alternate two workloads: A, B, A, B, ...",
		NeedsTwo:    true,
		Plan: func(n int, a, b string) []string {
			p := make([]string, n)
			for i := range p {
				if i%2 == 0 {
					p[i] = a
				} else {
					p[i] = b
				}
			}
			return p
		},
	},
	{
		Category:    "3. Patterns",
		Name:        "aabb",
		Description: "assign workloads in pairs: A, A, B, B, ...",
		NeedsTwo:    true,
		Plan: func(n int, a, b string) []string {
			p := make([]string, n)
			for i := range p {
				if i%4 < 2 {
					p[i] = a
				} else {
					p[i] = b
				}
			}
			return p
		},
	},
	{
		Category:    "3. Patterns",
		Name:        "half",
		Description: "workload A on the first half, workload B on the second half",
		NeedsTwo:    true,
		Plan: func(n int, a, b string) []string {
			p := make([]string, n)
			for i := range p {
				if i < n/2 {
					p[i] = a
				} else {
					p[i] = b
				}
			}
			return p
		},
	},
	{
		Category:    "3. Patterns",
		Name:        "isolate",
		Description: "one load worker; all other CPU slots remain idle",
		NeedsTwo:    false,
		Plan: func(n int, a, _ string) []string {
			p := make([]string, n)
			for i := range p {
				p[i] = workload.Idle
			}
			if n > 0 {
				p[0] = a
			}
			return p
		},
	},
}

// AllTests lists every registered test, grouped by category order.
func AllTests() []*Test {
	return append([]*Test(nil), tests...)
}

// GetTest looks up a test by name.
func GetTest(name string) (*Test, bool) {
	for _, t := range tests {
		if t.Name == name {
			return t, true
		}
	}
	return nil, false
}
