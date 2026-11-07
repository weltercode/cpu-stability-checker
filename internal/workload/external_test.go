package workload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHelperProcess is not a real test: it is the fake "external stress
// program" for the tests below. Its behavior is configured via environment
// variables, and it exits with the requested code.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	for i := 0; i < envInt("FAKE_LINES"); i++ {
		fmt.Printf("progress line %d\n", i)
	}
	// y-cruncher-style self-reports: "Running BKT: Passed"
	if s := os.Getenv("FAKE_STATUS"); s != "" {
		fmt.Println(s)
	}
	// real stress tools (mprime, y-cruncher) write into their working
	// directory; FAKE_TOUCH lets us check that Dir is honored
	if name := os.Getenv("FAKE_TOUCH"); name != "" {
		if err := os.WriteFile(name, []byte("touched"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
	}
	// GUI tools report findings by appending to a results file; the delay
	// lets us prove the tailer reacts during the run, not just at exit
	if name := os.Getenv("FAKE_LOG_FILE"); name != "" {
		if ms := envInt("FAKE_LOG_DELAY_MS"); ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		if os.Getenv("FAKE_LOG_TRUNC") == "1" {
			// rotation by truncation: discard the old content, then report
			f.Truncate(0)
			f.Seek(0, io.SeekStart)
		}
		switch {
		case os.Getenv("FAKE_LOG_SPLIT") == "1":
			// one fault split across two writes: the tailer must not
			// match the first half alone and must not miss the joined line
			fmt.Fprint(f, os.Getenv("FAKE_LOG_TEXT"))
			f.Sync()
			time.Sleep(time.Duration(envInt("FAKE_LOG_PART_DELAY_MS")) * time.Millisecond)
			fmt.Fprintf(f, "%s\n", os.Getenv("FAKE_LOG_TEXT2"))
		case os.Getenv("FAKE_LOG_NOTAIL") == "1":
			// last-gasp fault: the file ends mid-line, no trailing newline
			fmt.Fprint(f, os.Getenv("FAKE_LOG_TEXT"))
		default:
			fmt.Fprintf(f, "%s\n", os.Getenv("FAKE_LOG_TEXT"))
		}
		f.Close()
	}
	if os.Getenv("FAKE_LOG_REPLACE") == "1" {
		// rotation by renaming: move the old results file aside and
		// start a new one under the same name
		name := "results.json.txt"
		if err := os.Rename(name, name+".old"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		if err := os.WriteFile(name, []byte(os.Getenv("FAKE_LOG_TEXT2")+"\n"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
	}
	// y-cruncher v0.8 stress protocol, line-for-line as measured live
	// against the pinned build (banner from the launcher exe):
	if os.Getenv("FAKE_YC") == "1" {
		banner := os.Getenv("FAKE_YC_BANNER")
		if banner == "" {
			banner = "y-cruncher v0.8.7 Build 9547-vsc"
		}
		fmt.Println(banner)
		fmt.Println("Component Stress Tester")
		fmt.Println("Allocating Memory...")
		fmt.Println("Iteration: 0  Total Elapsed Time: 0.002 seconds  ( 0.000 minutes )")
		fmt.Println("Running BKT: Passed  Test Speed:  2.31 * 10^10  bits / sec")
		if os.Getenv("FAKE_YC_FAIL") == "1" {
			// y-cruncher stops itself on error and still exits 0
			fmt.Println("Running BKT: Failed  Test Speed:  2.3 * 10^10  bits / sec")
			fmt.Println("Error(s) encountered on logical core 5")
			fmt.Println("Errors encountered. Stopping test...")
			os.Exit(0)
		}
		if os.Getenv("FAKE_YC_EXC") == "1" {
			fmt.Println("Exception Encountered: access violation reading location 0x0")
			os.Exit(0)
		}
		if os.Getenv("FAKE_YC_HANG") == "1" {
			time.Sleep(30 * time.Second)
		}
		fmt.Println("Test Finished. Waiting for threads to terminate...")
		// Measured live 2026-10-24: the pinned mode binaries can sit in
		// their own thread-termination wait for minutes after printing
		// that banner - the protocol claim is complete, only the
		// teardown lingers. FAKE_YC_STALL_MS reproduces that state:
		// nothing left to verify, process refusing to die.
		if ms := envInt("FAKE_YC_STALL_MS"); ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
	}
	// Prime95 v30.19 torture protocol, line-for-line as measured live
	// against the pinned build (plain-text results.txt, no JSON).
	// Append mode matters: real Prime95 keeps results.txt across runs
	// and appends; a fake that truncates would hide the tailer's
	// fixed-position semantics from the second slot of a run.
	if os.Getenv("FAKE_P95") == "1" {
		f, err := os.OpenFile("results.txt", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		fmt.Fprintln(f, "[Fri Oct 16 03:43:21 2026]")
		fmt.Fprintln(f, "Self-test 8K (thread 1 of 2) passed!")
		if os.Getenv("FAKE_P95_FAULT") == "1" {
			// after a hardware failure Prime95 stops the test but
			// keeps its window open: the process stays alive
			fmt.Fprintln(f, "Hardware failure detected running 8K FFT size, consult stress.txt file.")
			time.Sleep(30 * time.Second)
		}
		if os.Getenv("FAKE_P95_TOOLERR") == "1" {
			fmt.Fprintln(f, "Error: unable to open scratch file")
			time.Sleep(30 * time.Second)
		}
		if os.Getenv("FAKE_P95_HANG") == "1" {
			time.Sleep(30 * time.Second)
		}
		// plain case: exits quickly and cleanly - but the torture
		// test does not self-complete, so that is an early clean exit
	}
	// single_instance tests count how many times the "tool" was launched
	if name := os.Getenv("FAKE_COUNT_FILE"); name != "" {
		if f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
		}
	}
	// launchers (y-cruncher's) spawn a child that inherits our stdout; the
	// child outlives the direct process unless the job kills the tree
	if os.Getenv("FAKE_CHILD_HOLD") == "1" {
		child := exec.Command("cmd", "/c", "ping -n 30 127.0.0.1 >nul")
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
	}
	if ms := envInt("FAKE_SLEEP_MS"); ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	os.Exit(envInt("FAKE_EXIT"))
}

func envInt(name string) int {
	v := 0
	if s := os.Getenv(name); s != "" {
		fmt.Sscanf(s, "%d", &v)
	}
	return v
}

// helperSpec builds a spec that re-runs this test binary as the "external"
// program.
func helperSpec(env map[string]string) ExternalSpec {
	if env == nil {
		env = map[string]string{}
	}
	env["GO_WANT_HELPER_PROCESS"] = "1"
	return ExternalSpec{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess"},
		Env:     env,
	}
}

type recordingReporter struct {
	mu       sync.Mutex
	adds     uint64
	beats    int
	statuses []string
	evidence []string
}

func (r *recordingReporter) Add(n uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adds += n
}

func (r *recordingReporter) Beat() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beats++
}

func (r *recordingReporter) Status(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = append(r.statuses, text)
}

// ReportEvidence implements workload.EvidenceReporter so tests can see
// exactly what the adapter proved.
func (r *recordingReporter) ReportEvidence(state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evidence = append(r.evidence, state)
}

func (r *recordingReporter) snapshot() (uint64, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.adds, r.beats
}

func (r *recordingReporter) statusSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.statuses...)
}

func (r *recordingReporter) evidenceSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.evidence...)
}

// statusText maps a completion report to the test running now (sequence)
// or shows it verbatim.
func TestExternalStatusText(t *testing.T) {
	cases := []struct {
		name  string
		spec  ExternalSpec
		match []string
		want  string
	}{
		{"sequence names the next test", ExternalSpec{StatusSequence: []string{"BKT", "BBP", "SFT"}},
			[]string{"Running BKT: Passed", "BKT"}, "BBP"},
		{"sequence wraps for the next iteration", ExternalSpec{StatusSequence: []string{"BKT", "BBP", "SFT"}},
			[]string{"Running SFT: Passed", "SFT"}, "BKT"},
		{"unknown tag shown as reported", ExternalSpec{StatusSequence: []string{"BKT", "BBP"}},
			[]string{"Running XYZ: Passed", "XYZ"}, "XYZ"},
		{"no sequence shows the capture", ExternalSpec{},
			[]string{"Running BKT: Passed", "BKT"}, "BKT"},
		{"no capture group shows the line", ExternalSpec{},
			[]string{"Running BKT: Passed"}, "Running BKT: Passed"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.statusText(tt.match); got != tt.want {
				t.Errorf("statusText() = %q, want %q", got, tt.want)
			}
		})
	}
}

// End to end: a tool printing "Running BKT: Passed" must land "BBP" (the
// next configured test) in the reporter while it is still running.
func TestExternalStatusReachesReporter(t *testing.T) {
	spec := helperSpec(map[string]string{"FAKE_STATUS": "Running BKT: Passed", "FAKE_SLEEP_MS": "2000"})
	spec.StatusPattern = `Running (\w+): Passed`
	spec.StatusSequence = []string{"BKT", "BBP", "SFT"}
	rep := &recordingReporter{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := spec.run(ctx, rep); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := rep.statusSnapshot()
	if len(got) == 0 || got[0] != "BBP" {
		t.Errorf("statuses = %v, want first %q", got, "BBP")
	}
}

func TestExternalWriteFiles(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		spec    ExternalSpec
		want    map[string]string // path under dir -> content ("" = must not exist)
		wantErr bool
	}{
		{
			name: "prime95 preset lands in Dir",
			spec: ExternalSpec{Dir: dir, WriteFiles: map[string]string{
				"prime.txt": "minTortureFFT=36\nmaxTortureFFT=248\n",
			}},
			want: map[string]string{"prime.txt": "minTortureFFT=36\nmaxTortureFFT=248\n"},
		},
		{
			name: "nested paths get their directories",
			spec: ExternalSpec{Dir: dir, WriteFiles: map[string]string{
				"sub\\stress.json": "{}",
			}},
			want: map[string]string{filepath.Join("sub", "stress.json"): "{}"},
		},
		{
			name: "no dir writes next to the caller",
			spec: ExternalSpec{WriteFiles: nil},
			want: map[string]string{},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.spec.writeFiles(); (err != nil) != tt.wantErr {
				t.Fatalf("writeFiles() err = %v, wantErr %v", err, tt.wantErr)
			}
			for rel, want := range tt.want {
				got, err := os.ReadFile(filepath.Join(dir, rel))
				if want == "" {
					if err == nil {
						t.Errorf("%q was created but must not exist", rel)
					}
					continue
				}
				if err != nil {
					t.Fatalf("read %q: %v", rel, err)
				}
				if string(got) != want {
					t.Errorf("%q = %q, want %q", rel, got, want)
				}
			}
		})
	}
}

func TestExternalSingleInstance(t *testing.T) {
	dir := t.TempDir()
	countFile := filepath.Join(dir, "launches.txt")

	t.Run("one process serves every slot", func(t *testing.T) {
		spec := helperSpec(map[string]string{"FAKE_SLEEP_MS": "2500", "FAKE_COUNT_FILE": countFile})
		spec.SingleInstance = true

		const slots = 4
		var wg sync.WaitGroup
		errs := make([]error, slots)
		reps := make([]*recordingReporter, slots)
		for i := range errs {
			reps[i] = &recordingReporter{}
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, errs[i] = spec.run(ctx, reps[i])
			}(i)
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Errorf("slot %d error = %v, want nil", i, err)
			}
		}
		data, err := os.ReadFile(countFile)
		if err != nil {
			t.Fatalf("read launch count: %v", err)
		}
		if n := len(strings.Fields(string(data))); n != 1 {
			t.Errorf("launches = %d, want exactly 1 process for %d slots", n, slots)
		}
		if _, beats := reps[slots-1].snapshot(); beats == 0 {
			t.Error("follower never beat, the watchdog would abort it")
		}
	})

	t.Run("owner fault faults the followers", func(t *testing.T) {
		spec := helperSpec(map[string]string{"FAKE_EXIT": "1", "FAKE_SLEEP_MS": "500"})
		spec.FaultExit = []int{1}
		spec.SingleInstance = true

		const slots = 3
		var wg sync.WaitGroup
		errs := make([]error, slots)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, errs[i] = spec.run(ctx, &recordingReporter{})
			}(i)
		}
		wg.Wait()

		var id string
		for i, err := range errs {
			if err == nil || !strings.Contains(err.Error(), "fault") {
				t.Errorf("slot %d error = %v, want the shared process fault", i, err)
				continue
			}
			// The new contract: every slot reports the SAME shared
			// run-level fact, and the inner fault classification
			// (external tool-identified) survives the wrapper.
			sh, ok := SharedRunOf(err)
			if !ok {
				t.Errorf("slot %d: error is not a shared-run outcome: %v", i, err)
				continue
			}
			if i == 0 {
				id = sh.ID
			} else if sh.ID != id {
				t.Errorf("slot %d: shared ID %q differs from %q", i, sh.ID, id)
			}
			if !IsExternalFault(err) {
				t.Errorf("slot %d: fault classification lost through the wrapper", i)
			}
		}
	})
}

func TestClassifyExit(t *testing.T) {
	tests := []struct {
		name    string
		code    int
		spec    ExternalSpec
		wantErr string
	}{
		{name: "zero is ok by default", code: 0},
		{name: "non-zero is unexpected by default", code: 1, wantErr: "unexpected code 1"},
		{name: "fault code", code: 1, spec: ExternalSpec{FaultExit: []int{1}}, wantErr: "reported a fault (exit code 1)"},
		{name: "custom ok codes", code: 99, spec: ExternalSpec{OKExit: []int{0, 99}}},
		{name: "ok wins over fault", code: 0, spec: ExternalSpec{FaultExit: []int{0}}},
		{name: "fault list misses", code: 2, spec: ExternalSpec{FaultExit: []int{1}}, wantErr: "unexpected code 2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyExit(tt.code, tt.spec)
			if (err != nil) != (tt.wantErr != "") {
				t.Fatalf("classifyExit(%d) error = %v, wantErr %q", tt.code, err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("classifyExit(%d) error = %v, want it to contain %q", tt.code, err, tt.wantErr)
			}
		})
	}
}

func TestExternalOutcome(t *testing.T) {
	t.Run("cancelled context is never a fault", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := externalOutcome(ctx, fmt.Errorf("signal: killed"), ExternalSpec{Command: "x"})
		if err != nil {
			t.Fatalf("externalOutcome() error = %v, want nil for our own cancellation", err)
		}
	})

	t.Run("missing command is an error", func(t *testing.T) {
		err := externalOutcome(context.Background(),
			errors.New(`exec: "no-such-program": executable file not found in %PATH%`),
			ExternalSpec{Command: "no-such-program"})
		if err == nil || !strings.Contains(err.Error(), "no-such-program") {
			t.Fatalf("externalOutcome() error = %v, want it to name the command", err)
		}
	})
}

func TestExternalWorkloadRun(t *testing.T) {
	t.Run("successful process reports lines and no fault", func(t *testing.T) {
		spec := helperSpec(map[string]string{"FAKE_LINES": "3", "FAKE_EXIT": "0"})
		spec.Beat = externalBeatStdout
		rep := &recordingReporter{}

		got, err := spec.run(context.Background(), rep)
		if err != nil {
			t.Fatalf("run() error = %v, want nil", err)
		}
		if got != 3 {
			t.Fatalf("run() output lines = %d, want 3 observed lines", got)
		}
		// P03 honesty: output lines keep the watchdog alive but are
		// never counted as iterations - a line is not a verified
		// check, and beats must not inflate coverage.
		if adds, beats := rep.snapshot(); adds != 0 || beats < 3 {
			t.Fatalf("reporter adds=%d beats=%d, want 0 adds and live beats", adds, beats)
		}
		// A clean legacy run proves nothing about verification: it
		// must report no evidence at all (the engine then keeps the
		// slot explicitly unknown).
		if ev := rep.evidenceSnapshot(); len(ev) != 0 {
			t.Errorf("clean legacy run reported evidence %v, want none", ev)
		}
	})

	t.Run("fault exit code becomes a workload fault", func(t *testing.T) {
		spec := helperSpec(map[string]string{"FAKE_EXIT": "1"})
		spec.FaultExit = []int{1}

		_, err := spec.run(context.Background(), &recordingReporter{})
		if err == nil || !strings.Contains(err.Error(), "reported a fault (exit code 1)") {
			t.Fatalf("run() error = %v, want a fault classification", err)
		}
	})

	t.Run("unexpected exit code becomes an error", func(t *testing.T) {
		spec := helperSpec(map[string]string{"FAKE_EXIT": "7"})

		_, err := spec.run(context.Background(), &recordingReporter{})
		if err == nil || !strings.Contains(err.Error(), "unexpected code 7") {
			t.Fatalf("run() error = %v, want an unexpected-exit classification", err)
		}
	})

	t.Run("cancelling the run stops the process without a fault", func(t *testing.T) {
		spec := helperSpec(map[string]string{"FAKE_SLEEP_MS": "10000"})

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()

		start := time.Now()
		_, err := spec.run(ctx, &recordingReporter{})
		if err != nil {
			t.Fatalf("run() error = %v, want nil when we cancelled the run", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("run() took %v, want prompt termination after cancel", elapsed)
		}
	})

	t.Run("timer mode beats while the process lives", func(t *testing.T) {
		spec := helperSpec(map[string]string{"FAKE_SLEEP_MS": "1500"})
		rep := &recordingReporter{}

		_, err := spec.run(context.Background(), rep)
		if err != nil {
			t.Fatalf("run() error = %v, want nil", err)
		}
		if _, beats := rep.snapshot(); beats == 0 {
			t.Fatal("reporter received no Beat calls, want periodic beats while running")
		}
	})

	t.Run("empty command is rejected", func(t *testing.T) {
		_, err := ExternalSpec{}.run(context.Background(), &recordingReporter{})
		if err == nil || !strings.Contains(err.Error(), "command is empty") {
			t.Fatalf("run() error = %v, want an empty-command error", err)
		}
	})

	t.Run("dir sets the working directory of the process", func(t *testing.T) {
		dir := t.TempDir()
		spec := helperSpec(map[string]string{"FAKE_TOUCH": "probe.txt"})
		spec.Dir = dir

		if _, err := spec.run(context.Background(), &recordingReporter{}); err != nil {
			t.Fatalf("run() error = %v, want nil", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "probe.txt")); err != nil {
			t.Fatalf("process did not write into Dir: %v", err)
		}
	})

	t.Run("fault pattern in stdout output fails the workload", func(t *testing.T) {
		spec := helperSpec(map[string]string{"FAKE_LINES": "5", "FAKE_EXIT": "0"})
		spec.Beat = externalBeatStdout
		spec.FaultPattern = "progress line 2"

		_, err := spec.run(context.Background(), &recordingReporter{})
		if err == nil || !strings.Contains(err.Error(), `output reported a fault: "progress line 2"`) {
			t.Fatalf("run() error = %v, want the matched output line reported as fault", err)
		}
	})

	t.Run("fault pattern stops a long silent process early", func(t *testing.T) {
		// timer beat mode: output is normally discarded, but a fault pattern
		// must still be seen and must kill the process right away
		spec := helperSpec(map[string]string{"FAKE_LINES": "2", "FAKE_SLEEP_MS": "10000", "FAKE_EXIT": "0"})
		spec.FaultPattern = "progress line 1"

		start := time.Now()
		_, err := spec.run(context.Background(), &recordingReporter{})
		if err == nil || !strings.Contains(err.Error(), "output reported a fault") {
			t.Fatalf("run() error = %v, want a fault from the output pattern", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("run() took %v, want the process killed right after the match", elapsed)
		}
	})

	t.Run("bad fault pattern is an error, not a silent pass", func(t *testing.T) {
		spec := helperSpec(map[string]string{"FAKE_EXIT": "0"})
		spec.FaultPattern = "([unclosed"

		if _, err := spec.run(context.Background(), &recordingReporter{}); err == nil ||
			!strings.Contains(err.Error(), "bad fault_pattern") {
			t.Fatalf("run() error = %v, want a fault_pattern compile error", err)
		}
	})

	t.Run("fault appended to the watched file fails the workload", func(t *testing.T) {
		// GUI-style tool: no console output, findings go to a results file
		dir := t.TempDir()
		spec := helperSpec(map[string]string{
			"FAKE_LOG_FILE":     "results.json.txt",
			"FAKE_LOG_DELAY_MS": "500",
			"FAKE_LOG_TEXT":     `{"v":17,"st":"F"} Worker #1: Hardware failure detected`,
			"FAKE_SLEEP_MS":     "10000",
			"FAKE_EXIT":         "0",
		})
		spec.Dir = dir
		spec.FaultFile = "results.json.txt"
		spec.FaultPattern = `(?i)hardware failure`

		start := time.Now()
		_, err := spec.run(context.Background(), &recordingReporter{})
		if err == nil || !strings.Contains(err.Error(), "reported a fault in") {
			t.Fatalf("run() error = %v, want a fault from the watched file", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("run() took %v, want the process killed right after the file match", elapsed)
		}
	})

	t.Run("old content of the watched file is ignored", func(t *testing.T) {
		dir := t.TempDir()
		// a previous run already recorded a failure here; this run must not
		// be poisoned by it
		if err := os.WriteFile(filepath.Join(dir, "results.json.txt"),
			[]byte("Worker #1: Hardware failure detected\n"), 0o644); err != nil {
			t.Fatalf("write results: %v", err)
		}
		spec := helperSpec(map[string]string{"FAKE_SLEEP_MS": "1500", "FAKE_EXIT": "0"})
		spec.Dir = dir
		spec.FaultFile = "results.json.txt"
		spec.FaultPattern = `(?i)hardware failure`

		if _, err := spec.run(context.Background(), &recordingReporter{}); err != nil {
			t.Fatalf("run() error = %v, want nil: pre-existing content must not re-trigger", err)
		}
	})

	t.Run("fault in the final line without a newline is still caught", func(t *testing.T) {
		// The tool writes its last-gasp fault and exits at once: the
		// tailer's final drain must read the tail (no trailing newline)
		// before the verdict is decided, or the fault would vanish.
		dir := t.TempDir()
		spec := helperSpec(map[string]string{
			"FAKE_LOG_FILE":   "results.json.txt",
			"FAKE_LOG_NOTAIL": "1",
			"FAKE_LOG_TEXT":   "Worker #1: Hardware failure detected",
			"FAKE_EXIT":       "0",
		})
		spec.Dir = dir
		spec.FaultFile = "results.json.txt"
		spec.FaultPattern = `(?i)hardware failure`

		if _, err := spec.run(context.Background(), &recordingReporter{}); err == nil ||
			!strings.Contains(err.Error(), "reported a fault in") {
			t.Fatalf("run() error = %v, want the fault from the unterminated tail", err)
		}
	})

	t.Run("fault split across two writes is caught once complete", func(t *testing.T) {
		// The first half alone must not match (and does not: the pattern
		// needs the joined text), and the joined line must be matched
		// even though it arrived in two chunks across tail ticks.
		dir := t.TempDir()
		spec := helperSpec(map[string]string{
			"FAKE_LOG_FILE":          "results.json.txt",
			"FAKE_LOG_SPLIT":         "1",
			"FAKE_LOG_TEXT":          "Worker #1: Hardware ",
			"FAKE_LOG_TEXT2":         "failure detected",
			"FAKE_LOG_PART_DELAY_MS": "1500",
			"FAKE_SLEEP_MS":          "10000",
			"FAKE_EXIT":              "0",
		})
		spec.Dir = dir
		spec.FaultFile = "results.json.txt"
		spec.FaultPattern = `(?i)hardware failure`

		start := time.Now()
		_, err := spec.run(context.Background(), &recordingReporter{})
		if err == nil || !strings.Contains(err.Error(), "reported a fault in") {
			t.Fatalf("run() error = %v, want the fault from the joined partial line", err)
		}
		if elapsed := time.Since(start); elapsed > 8*time.Second {
			t.Fatalf("run() took %v, want the kill right after the line completed", elapsed)
		}
	})

	t.Run("rotation by truncation is caught", func(t *testing.T) {
		// Tools rotate results by truncating the file in place: the
		// shrink must reset the read position (loudly), and the fresh
		// fault after the truncation must still be seen.
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "results.json.txt"),
			[]byte("old run: all fine\nold run: still fine\n"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		spec := helperSpec(map[string]string{
			"FAKE_LOG_FILE":  "results.json.txt",
			"FAKE_LOG_TRUNC": "1",
			"FAKE_LOG_TEXT":  "Worker #3: Hardware failure detected",
			"FAKE_SLEEP_MS":  "10000",
			"FAKE_EXIT":      "0",
		})
		spec.Dir = dir
		spec.FaultFile = "results.json.txt"
		spec.FaultPattern = `(?i)hardware failure`

		if _, err := spec.run(context.Background(), &recordingReporter{}); err == nil ||
			!strings.Contains(err.Error(), "reported a fault in") {
			t.Fatalf("run() error = %v, want the fault after the truncation", err)
		}
	})

	t.Run("rotation by renaming is caught on the new file", func(t *testing.T) {
		// Prime95-style rotation: rename the old results file away and
		// start a new one under the same name. The tailer's identity
		// checkpoint must notice the path no longer continues the file
		// it holds, adopt the new file, and catch the fault in it.
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "results.json.txt"),
			[]byte(strings.Repeat("old-result-line\n", 8)), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		spec := helperSpec(map[string]string{
			"FAKE_LOG_REPLACE": "1",
			"FAKE_LOG_TEXT2":   "Worker #7: Hardware failure detected",
			"FAKE_SLEEP_MS":    "10000",
			"FAKE_EXIT":        "0",
		})
		spec.Dir = dir
		spec.FaultFile = "results.json.txt"
		spec.FaultPattern = `(?i)hardware failure`

		if _, err := spec.run(context.Background(), &recordingReporter{}); err == nil ||
			!strings.Contains(err.Error(), "reported a fault in") {
			t.Fatalf("run() error = %v, want the fault from the replacement file", err)
		}
	})

	t.Run("a grandchild holding the pipe does not hang the run", func(t *testing.T) {
		// the direct process exits at once, but its child keeps the
		// inherited stdout pipe open for ~30s; only closing the job when
		// the direct process dies lets the scanner see EOF quickly
		//
		// Level 2 (Windows integration, audit A07): the holder really is
		// a console ping so the inherited-stdout hold cannot be faked by
		// our own helper; without the tools the subtest skips naming the
		// missing capability instead of failing for environment reasons.
		for _, tool := range []string{"cmd", "ping"} {
			if _, err := exec.LookPath(tool); err != nil {
				t.Skipf("test level 2 (Windows integration): %s is not available: %v", tool, err)
			}
		}
		spec := helperSpec(map[string]string{"FAKE_CHILD_HOLD": "1", "FAKE_EXIT": "0"})
		spec.FaultPattern = `never-matches-anything` // force piped mode

		start := time.Now()
		if _, err := spec.run(context.Background(), &recordingReporter{}); err != nil {
			t.Fatalf("run() error = %v, want nil", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("run() took %v, want the tree killed when the direct process exits", elapsed)
		}
	})
}

func TestRegisterExternal(t *testing.T) {
	specs := map[string]ExternalSpec{
		"fake-ok":    {Command: "fake", Description: "fake passing tool"},
		"no-cmd":     {Command: ""},
		"ycruncher":  {Command: "y-cruncher.exe"},
		"bad-re":     {Command: "fake", FaultPattern: "([unclosed"},
		"file-no-re": {Command: "fake", FaultFile: "results.txt"},
	}
	RegisterExternal(specs)
	t.Cleanup(func() {
		Delete("fake-ok")
		Delete("ycruncher")
	})

	w, ok := Get("fake-ok")
	if !ok {
		t.Fatal(`Get("fake-ok") not found after registration`)
	}
	if w.Description != "fake passing tool" {
		t.Fatalf("description = %q, want the configured one", w.Description)
	}
	if _, ok := Get("no-cmd"); ok {
		t.Fatal(`Get("no-cmd") found, want entries without a command skipped`)
	}
	if _, ok := Get("bad-re"); ok {
		t.Fatal(`Get("bad-re") found, want entries with an uncompilable fault_pattern skipped`)
	}
	if _, ok := Get("file-no-re"); ok {
		t.Fatal(`Get("file-no-re") found, want fault_file without fault_pattern skipped`)
	}
	w, ok = Get("ycruncher")
	if !ok || !strings.Contains(w.Description, "y-cruncher.exe") {
		t.Fatalf(`Get("ycruncher") = %+v, %v, want a generated description`, w, ok)
	}
}

// Adapter selection is explicit: an unknown adapter name is refused
// (never a silent fallback to something that might misread the tool),
// empty/"legacy" keeps the universal adapter, and a bad regex is
// caught when the adapter is built.
func TestExternalAdapterSelection(t *testing.T) {
	if _, err := (ExternalSpec{Command: "x", Adapter: "no-such-adapter"}).adapterFor(); err == nil ||
		!strings.Contains(err.Error(), "unknown adapter") {
		t.Errorf("adapterFor(unknown) error = %v, want a refusal to guess", err)
	}
	for _, name := range []string{"", "legacy", "LEGACY"} {
		a, err := (ExternalSpec{Command: "x", Adapter: name}).adapterFor()
		if err != nil || a == nil {
			t.Errorf("adapterFor(%q) = %v, %v, want the legacy adapter", name, a, err)
		}
	}
	if _, err := (ExternalSpec{Command: "x", FaultPattern: "("}).adapterFor(); err == nil {
		t.Error("adapterFor(bad fault_pattern) = nil error, want a compile error")
	}
	if _, err := (ExternalSpec{Command: "x", StatusPattern: "("}).adapterFor(); err == nil {
		t.Error("adapterFor(bad status_pattern) = nil error, want a compile error")
	}
}

// When the legacy adapter sees its fault pattern in the output, the
// failure must also surface as evidence: a finding alone would leave
// the slot's evidence unknown, and "unknown" hides that we did see a
// fault.
func TestExternalFaultLineReportsFailedEvidence(t *testing.T) {
	spec := helperSpec(map[string]string{
		"FAKE_STATUS": "A hardware failure detected",
		"FAKE_EXIT":   "0",
	})
	spec.Beat = externalBeatStdout
	spec.FaultPattern = "hardware failure"
	rep := &recordingReporter{}

	_, err := spec.run(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "reported a fault") {
		t.Fatalf("run() error = %v, want the fault classification", err)
	}
	ev := rep.evidenceSnapshot()
	if len(ev) != 1 || ev[0] != EvFailed {
		t.Errorf("evidence = %v, want [%s]", ev, EvFailed)
	}
}

// RegisterExternal must skip an entry whose adapter cannot be built
// (unknown name, uncompilable pattern) instead of registering a
// workload that would fail at run time - or worse, silently misread
// the tool's output.
func TestRegisterExternalSkipsUnusableAdapters(t *testing.T) {
	RegisterExternal(map[string]ExternalSpec{
		"bad-adapter": {Command: "x", Adapter: "no-such"},
		"bad-regex":   {Command: "x", FaultPattern: "("},
		"good-legacy": {Command: "x"},
	})
	t.Cleanup(func() { Delete("bad-adapter"); Delete("bad-regex"); Delete("good-legacy") })
	if VerificationOf("bad-adapter") != "" {
		t.Error("entry with unknown adapter was registered anyway")
	}
	if VerificationOf("bad-regex") != "" {
		t.Error("entry with an uncompilable pattern was registered anyway")
	}
	if VerificationOf("good-legacy") != VerifyExternal {
		t.Error("legacy entry must keep registering unchanged")
	}
}

// The affinity honesty rules are pure and testable: what the mask read
// back from the OS does - and does not - let us claim. An allowed
// mask is never proof that a CPU was used; every gap is admitted.
func TestAffinityObservations(t *testing.T) {
	own := ExternalSpec{Command: "tool"}
	shared := ExternalSpec{Command: "mprime", SingleInstance: true}
	all := ^uint64(0)
	cases := []struct {
		name       string
		spec       ExternalSpec
		parentMask uint64
		parentOK   bool
		childMask  uint64
		readErr    error
		groupsOK   bool
		maxGroups  int
		activeCPUs int
		wantSubs   []string // empty means: no notes expected at all
	}{
		{"read failure admits unconfirmed coverage", own, 1, true, 0, errors.New("OpenProcess: denied"), true, 1, 32, []string{"cannot read back"}},
		{"child with no group-0 mask may be another group", own, 1, true, 0, nil, true, 1, 32, []string{"no group-0 CPU affinity"}},
		{"multi-group machine is stated, not truncated", own, 1, true, 1, nil, true, 2, 128, []string{"2 processor groups / 128 active CPUs", "outside what pinning can express"}},
		{"shared process discloses the allowed set only", shared, all, true, all, nil, true, 1, 32, []string{"worker count and per-CPU coverage are not confirmed"}},
		{"tool reset its mask", own, 1, true, 3, nil, true, 1, 32, []string{"reset its own affinity"}},
		{"broad inherited mask is not confinement", own, all, true, all, nil, true, 1, 32, []string{"not confined to one core"}},
		{"exact single-core confinement needs no note", own, 8, true, 8, nil, true, 1, 32, nil},
		{"own mask unreadable", own, 0, false, 8, nil, true, 1, 32, []string{"cannot read our own affinity"}},
		{"groups unreadable stays silent when coverage is exact", own, 1, true, 1, nil, false, 0, 0, nil},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := affinityObservations(tt.spec, tt.parentMask, tt.parentOK, tt.childMask, tt.readErr, tt.groupsOK, tt.maxGroups, tt.activeCPUs)
			joined := strings.Join(got, "\n")
			if len(tt.wantSubs) == 0 {
				if len(got) != 0 {
					t.Errorf("affinityObservations() = %q, want no notes", joined)
				}
				return
			}
			for _, want := range tt.wantSubs {
				if !strings.Contains(joined, want) {
					t.Errorf("affinityObservations() = %q, want a note saying %q", joined, want)
				}
			}
		})
	}
}
