package sysinfo

import (
	"bytes"
	"strings"
	"testing"
)

func TestNormalizeTopology(t *testing.T) {
	tests := []struct {
		name       string
		raw        []rawLogicalCPU
		wantCores  int
		wantFirst  []uint8
		wantCCX    []int
		wantErrSub string
	}{
		{
			name: "groups unsorted SMT siblings",
			raw: []rawLogicalCPU{
				{group: 0, coreIndex: 1, number: 3, cpuSetID: 13, cpuSetIDKnown: true},
				{group: 0, coreIndex: 0, number: 1, cpuSetID: 11, cpuSetIDKnown: true},
				{group: 0, coreIndex: 0, number: 0, cpuSetID: 10, cpuSetIDKnown: true},
				{group: 0, coreIndex: 1, number: 2, cpuSetID: 12, cpuSetIDKnown: true},
			},
			wantCores: 2,
			wantFirst: []uint8{0, 1},
		},
		{
			name: "cores sharing one L3 form one CCX",
			raw: []rawLogicalCPU{
				{group: 0, coreIndex: 0, number: 0, l3ID: 10, l3Known: true},
				{group: 0, coreIndex: 1, number: 1, l3ID: 10, l3Known: true},
				{group: 0, coreIndex: 2, number: 2, l3ID: 20, l3Known: true},
				{group: 0, coreIndex: 3, number: 3, l3ID: 20, l3Known: true},
			},
			wantCores: 4,
			wantFirst: []uint8{0},
			wantCCX:   []int{0, 0, 1, 1},
		},
		{
			name: "unknown L3 leaves CCX unset",
			raw: []rawLogicalCPU{
				{group: 0, coreIndex: 0, number: 0},
				{group: 0, coreIndex: 1, number: 1},
			},
			wantCores: 2,
			wantFirst: []uint8{0},
		},
		{
			name: "processor groups keep native core indexes separate",
			raw: []rawLogicalCPU{
				{group: 1, coreIndex: 0, number: 0},
				{group: 0, coreIndex: 0, number: 0},
			},
			wantCores: 2,
			wantFirst: []uint8{0},
		},
		{
			name: "duplicate logical CPU",
			raw: []rawLogicalCPU{
				{group: 0, coreIndex: 0, number: 0},
				{group: 0, coreIndex: 1, number: 0},
			},
			wantErrSub: "duplicate logical CPU",
		},
		{
			name: "conflicting sibling metadata",
			raw: []rawLogicalCPU{
				{group: 0, coreIndex: 0, number: 0, numaNode: 0},
				{group: 0, coreIndex: 0, number: 1, numaNode: 1},
			},
			wantErrSub: "conflicting metadata",
		},
		{
			name: "conflicting L3 between siblings",
			raw: []rawLogicalCPU{
				{group: 0, coreIndex: 0, number: 0, l3ID: 10, l3Known: true},
				{group: 0, coreIndex: 0, number: 1, l3ID: 20, l3Known: true},
			},
			wantErrSub: "conflicting L3 cache",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeTopology(tt.raw)
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("error = %v, want substring %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeTopology() error = %v", err)
			}
			if len(got) != tt.wantCores {
				t.Fatalf("core count = %d, want %d", len(got), tt.wantCores)
			}
			first := got[0].LogicalCPUs
			if len(first) != len(tt.wantFirst) {
				t.Fatalf("first core logical count = %d, want %d", len(first), len(tt.wantFirst))
			}
			for i, want := range tt.wantFirst {
				if first[i].Number != want {
					t.Errorf("first core logical[%d] = %d, want %d", i, first[i].Number, want)
				}
			}
			if tt.wantCCX != nil {
				for i, want := range tt.wantCCX {
					if !got[i].CCXKnown || got[i].CCXIndex != want {
						t.Errorf("core %d CCX = (%d, known=%v), want %d", i, got[i].CCXIndex, got[i].CCXKnown, want)
					}
				}
			}
		})
	}
}

func TestWriteTopology(t *testing.T) {
	topology := Topology{
		Source: "test",
		PhysicalCores: []PhysicalCore{
			{
				Index:       0,
				Group:       0,
				NativeIndex: 3,
				CCXIndex:    2,
				CCXKnown:    true,
				LogicalCPUs: []LogicalCPU{
					{Group: 0, Number: 6, CPUSetID: 20, CPUSetIDKnown: true},
					{Group: 0, Number: 7, CPUSetID: 21, CPUSetIDKnown: true, Parked: true},
				},
			},
		},
	}

	var out bytes.Buffer
	WriteTopology(&out, topology)
	text := out.String()
	for _, want := range []string{"Physical cores: 1, logical CPUs: 2", "NUMA  CCX  EFF", "g0:6,g0:7", "20,21", "g0:7"} {
		if !strings.Contains(text, want) {
			t.Errorf("output does not contain %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "0     3       0      0     2   ") {
		t.Errorf("output does not show CCX 2 for the core:\n%s", text)
	}
}
