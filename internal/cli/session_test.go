package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

func TestParseNumberSet(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		limit   int
		want    []int
		wantErr bool
	}{
		{name: "all", input: "all", limit: 4, want: []int{0, 1, 2, 3}},
		{name: "numbers and ranges", input: "1, 2, 5-8, 12", limit: 16, want: []int{1, 2, 5, 6, 7, 8, 12}},
		{name: "duplicates collapse", input: "1,1-3,2", limit: 4, want: []int{1, 2, 3}},
		{name: "descending range", input: "5-2", limit: 8, wantErr: true},
		{name: "outside range", input: "8", limit: 8, wantErr: true},
		{name: "bad token", input: "1,two", limit: 8, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNumberSet(tt.input, tt.limit)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseNumberSet() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseNumberSet() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildPerCoreSessionRuns(t *testing.T) {
	topology := sysinfo.Topology{PhysicalCores: []sysinfo.PhysicalCore{
		{Index: 0, LogicalCPUs: []sysinfo.LogicalCPU{{Number: 0}, {Number: 1}}},
		{Index: 1, LogicalCPUs: []sysinfo.LogicalCPU{{Number: 2}, {Number: 3}}},
		{Index: 2, LogicalCPUs: []sysinfo.LogicalCPU{{Number: 4}, {Number: 5}}},
	}}
	session := SessionOptions{
		Mode:           sessionPerCore,
		PhysicalCores:  []int{0, 2},
		Workload:       "int",
		ThreadsPerCore: 2,
		Duration:       time.Second,
		Passes:         2,
	}

	runs, err := buildSessionRuns(session, topology)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 4 {
		t.Fatalf("run count = %d, want 4", len(runs))
	}
	if !reflect.DeepEqual(runs[0].logicalCPUs, []int{0, 1}) || !reflect.DeepEqual(runs[1].logicalCPUs, []int{4, 5}) {
		t.Fatalf("unexpected first pass mapping: %v, %v", runs[0].logicalCPUs, runs[1].logicalCPUs)
	}
	if runs[2].pass != 2 || runs[3].pass != 2 {
		t.Fatalf("second pass not generated correctly: %+v", runs)
	}
}

func TestWriteSessionPlanExecutionLine(t *testing.T) {
	topology := sysinfo.Topology{PhysicalCores: []sysinfo.PhysicalCore{
		{Index: 0, LogicalCPUs: []sysinfo.LogicalCPU{{Number: 0}, {Number: 1}}},
	}}
	tests := []struct {
		name    string
		session SessionOptions
		want    string
	}{
		{
			name:    "in-process by default",
			session: SessionOptions{Mode: sessionAllCore, PhysicalCores: []int{0}, Workload: "int", ThreadsPerCore: 1, Duration: time.Second, Passes: 1},
			want:    "in-process worker threads",
		},
		{
			name:    "per-process subprocess",
			session: SessionOptions{Mode: sessionAllCore, PhysicalCores: []int{0}, Workload: "int", ThreadsPerCore: 1, Duration: time.Second, Passes: 1, PerProcess: true},
			want:    "one pinned subprocess per logical CPU",
		},
		{
			name:    "rotating subprocesses",
			session: SessionOptions{Mode: sessionAllCore, PhysicalCores: []int{0}, Workload: "int", ThreadsPerCore: 1, Duration: time.Second, Passes: 1, PerProcess: true, Rotate: true},
			want:    "rotating subprocesses: exclusive window per core",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			writeSessionPlan(&out, tt.session, topology)
			if !strings.Contains(out.String(), tt.want) {
				t.Errorf("plan output does not contain %q:\n%s", tt.want, out.String())
			}
		})
	}
}

func TestBuildAllCoreSessionRuns(t *testing.T) {
	topology := sysinfo.Topology{PhysicalCores: []sysinfo.PhysicalCore{
		{Index: 0, LogicalCPUs: []sysinfo.LogicalCPU{{Number: 0}, {Number: 1}}},
		{Index: 1, LogicalCPUs: []sysinfo.LogicalCPU{{Number: 2}, {Number: 3}}},
		{Index: 2, LogicalCPUs: []sysinfo.LogicalCPU{{Number: 4}, {Number: 5}}},
	}}
	session := SessionOptions{
		Mode:           sessionAllCore,
		PhysicalCores:  []int{1, 2},
		Workload:       "float",
		ThreadsPerCore: 1,
		Duration:       time.Second,
		Passes:         3,
	}

	runs, err := buildSessionRuns(session, topology)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("run count = %d, want 3", len(runs))
	}
	for i, run := range runs {
		if run.physicalCore != nil {
			t.Fatalf("all-core run %d unexpectedly has one physical core", i)
		}
		if !reflect.DeepEqual(run.logicalCPUs, []int{2, 4}) {
			t.Fatalf("all-core run %d logical CPUs = %v, want [2 4]", i, run.logicalCPUs)
		}
	}
}

func TestSessionLogPattern(t *testing.T) {
	got := sessionLogPattern("logs/cpu-stability.log", sessionPerCore)
	if !strings.Contains(got, "passNN-coreNN.log") {
		t.Fatalf("pattern = %q", got)
	}
}
