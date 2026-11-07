package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// runCapture runs one command with stdout/stderr on pipes and hands
// back the verdict plus both streams: the JSON answers must parse
// from stdout with nothing else in the way. Both pipes are drained
// CONCURRENTLY with the command: a Windows anonymous pipe holds only
// ~8KB, and a catalog answer is bigger - reading afterwards would
// deadlock the writer (the same trap task_menu_test.go already pins).
func runCapture(t *testing.T, fn func() int) (int, string, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	outr, outw, _ := os.Pipe()
	errr, errw, _ := os.Pipe()
	os.Stdout, os.Stderr = outw, errw
	outCh := make(chan []byte, 1)
	errCh := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(outr); outCh <- b }()
	go func() { b, _ := io.ReadAll(errr); errCh <- b }()
	code := fn()
	os.Stdout, os.Stderr = oldOut, oldErr
	outw.Close()
	errw.Close()
	return code, string(<-outCh), string(<-errCh)
}

// The catalog answer is the GUI's picker data: every card, stable
// ids, explicit availability, and the card's own reason whenever it
// is not available - "why not" is data, never a guess.
func TestCatalogJSON(t *testing.T) {
	code, out, _ := runCapture(t, func() int { return catalogCmd([]string{"-json"}) })
	if code != 0 {
		t.Fatalf("catalog -json exit = %d", code)
	}
	var ans struct {
		SchemaVersion int `json:"schema_version"`
		Profiles      []struct {
			ID           string `json:"id"`
			Version      string `json:"version"`
			Category     string `json:"category"`
			Availability string `json:"availability"`
			Reason       string `json:"reason"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		t.Fatalf("catalog answer is not pure JSON: %v\n%s", err, out)
	}
	if ans.SchemaVersion != infoSchemaVersion || len(ans.Profiles) != len(catalog.All()) || len(ans.Profiles) == 0 {
		t.Fatalf("catalog answer wrong: %+v", ans)
	}
	for _, p := range ans.Profiles {
		if p.ID == "" || p.Version == "" || p.Category == "" {
			t.Errorf("card %q lacks identity: %+v", p.ID, p)
		}
		switch p.Availability {
		case "available":
		case "unavailable", "unknown":
			if p.Reason == "" {
				t.Errorf("card %q is %s without a reason", p.ID, p.Availability)
			}
		default:
			t.Errorf("card %q has availability %q, want available/unavailable/unknown", p.ID, p.Availability)
		}
	}

	code, out, _ = runCapture(t, func() int { return catalogCmd(nil) })
	if code != 0 || !strings.Contains(out, "cpu-all") {
		t.Errorf("human catalog exit %d or lacks the first card:\n%s", code, out)
	}
}

// The topology answer names every physical core with its SMT
// siblings; the source says how the machine was read, and estimated
// stays visible instead of smoothing over a hidden cache topology.
func TestTopologyJSON(t *testing.T) {
	code, out, stderr := runCapture(t, func() int { return topologyCmd([]string{"-json"}) })
	if code != 0 {
		t.Fatalf("topology -json exit = %d\n%s", code, stderr)
	}
	var ans struct {
		SchemaVersion int    `json:"schema_version"`
		Source        string `json:"source"`
		Estimated     bool   `json:"estimated"`
		PhysicalCores []struct {
			Index       int  `json:"index"`
			CCXKnown    bool `json:"ccx_known"`
			CCXIndex    int  `json:"ccx_index"`
			LogicalCPUs []struct {
				Number uint8 `json:"number"`
				Parked bool  `json:"parked"`
			} `json:"logical_cpus"`
		} `json:"physical_cores"`
	}
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		t.Fatalf("topology answer is not pure JSON: %v\n%s", err, out)
	}
	if ans.SchemaVersion != infoSchemaVersion || ans.Source == "" || len(ans.PhysicalCores) == 0 {
		t.Fatalf("topology answer wrong: %+v", ans)
	}
	logical := 0
	for i, c := range ans.PhysicalCores {
		if c.Index != i {
			t.Errorf("core %d reports index %d", i, c.Index)
		}
		if len(c.LogicalCPUs) == 0 {
			t.Errorf("core %d has no logical CPUs", i)
		}
		logical += len(c.LogicalCPUs)
	}
	if hw := runtime.NumCPU(); logical < hw {
		t.Errorf("answer lists %d logical CPUs, fewer than the %d this process sees", logical, hw)
	}
}

// The capabilities answer asks the same probes the human form asks:
// every source named once, available or named-why-not.
func TestCapsJSON(t *testing.T) {
	code, out, stderr := runCapture(t, func() int { return capsCmd([]string{"-json"}) })
	if code != 0 {
		t.Fatalf("caps -json exit = %d\n%s", code, stderr)
	}
	var ans struct {
		SchemaVersion int `json:"schema_version"`
		Capabilities  []struct {
			Name      string `json:"name"`
			Available bool   `json:"available"`
			Detail    string `json:"detail"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		t.Fatalf("caps answer is not pure JSON: %v\n%s", err, out)
	}
	if ans.SchemaVersion != infoSchemaVersion || len(ans.Capabilities) != len(capProbes()) {
		t.Fatalf("caps answer wrong: %+v", ans)
	}
	seen := map[string]bool{}
	for _, c := range ans.Capabilities {
		if c.Name == "" || seen[c.Name] {
			t.Errorf("capability %q missing or duplicated", c.Name)
		}
		seen[c.Name] = true
		if !c.Available && c.Detail == "" {
			t.Errorf("capability %q is unavailable without a reason", c.Name)
		}
	}
}

// The history answer is the index view: newest first, total counts
// everything found, unreadable manifests surface as warnings without
// dropping the readable ones, and -limit cuts the list but never
// the total.
func TestHistoryJSON(t *testing.T) {
	chdirTemp(t)
	old := newTestManifest("20260101-000000-0000000000000001")
	exit := 0
	old.State = "completed"
	old.Exit = &exit
	old.Finished = time.Now().UTC().Format(time.RFC3339)
	mid := newTestManifest("20260601-000000-0000000000000002") // stays "running": unfinished, honest
	for _, m := range []*sessionManifest{old, mid} {
		if err := saveSessionManifest(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(manifestPath("broken"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, _ := runCapture(t, func() int { return historyCmd([]string{"-json"}) })
	if code != 0 {
		t.Fatalf("history -json exit = %d", code)
	}
	var ans struct {
		SchemaVersion int `json:"schema_version"`
		Total         int `json:"total"`
		Sessions      []struct {
			SessionID string `json:"session_id"`
			State     string `json:"state"`
			Exit      *int   `json:"exit"`
			Finished  string `json:"finished"`
		} `json:"sessions"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		t.Fatalf("history answer is not pure JSON: %v\n%s", err, out)
	}
	if ans.SchemaVersion != infoSchemaVersion || ans.Total != 2 || len(ans.Sessions) != 2 {
		t.Fatalf("history answer wrong: %s", out)
	}
	if ans.Sessions[0].SessionID != mid.SessionID {
		t.Errorf("sessions not newest first: %v", ans.Sessions)
	}
	if ans.Sessions[0].State != "running" || ans.Sessions[0].Exit != nil || ans.Sessions[0].Finished != "" {
		t.Errorf("the unfinished session was dressed up: %+v", ans.Sessions[0])
	}
	if ans.Sessions[1].State != "completed" || ans.Sessions[1].Exit == nil {
		t.Errorf("the finished session lost its truth: %+v", ans.Sessions[1])
	}
	if len(ans.Warnings) != 1 || !strings.Contains(ans.Warnings[0], "broken") {
		t.Errorf("the corrupt manifest went silent: %v", ans.Warnings)
	}

	code, out, _ = runCapture(t, func() int { return historyCmd([]string{"-json", "-limit", "1"}) })
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		t.Fatalf("-limit answer is not pure JSON: %v\n%s", err, out)
	}
	if code != 0 || ans.Total != 2 || len(ans.Sessions) != 1 {
		t.Errorf("-limit cut the total too: code %d ans %+v", code, ans)
	}

	code, out, stderr := runCapture(t, func() int { return historyCmd([]string{"-limit", "1"}) })
	if code != 0 || !strings.Contains(out, "shown 1 of 2") || !strings.Contains(stderr, "warning:") {
		t.Errorf("human history wrong (code %d):\n%s\n--- stderr:\n%s", code, out, stderr)
	}
}

// repeat is the one-action entry over the same pipeline the menu
// uses: no slot, honest refusal; a slot, a real session with its
// manifest.
func TestRepeatCmd(t *testing.T) {
	chdirTemp(t)
	if code := repeatCmd(); code != 2 {
		t.Errorf("repeat without a slot exit = %d, want 2", code)
	}

	useScratchReceipt(t)
	g01AffinityGuard(t)
	workload.Register(&workload.Workload{Name: "g01d-fake",
		Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
			rep.Add(1)
			rep.Beat()
			<-ctx.Done()
			return 1, nil
		}})
	defer workload.Delete("g01d-fake")

	req := sessionRequest()
	req.Session.Workload = "g01d-fake"
	req.RecipeID = "recipe-g01d"
	if err := saveLastRun(req); err != nil {
		t.Fatal(err)
	}
	if code := repeatCmd(); code != 0 {
		t.Fatalf("repeat exit = %d, want 0", code)
	}
	m := onlyManifest(t)
	if m.Kind != "session" || m.State != "completed" || m.RecipeID != "recipe-g01d" {
		t.Errorf("repeat did not record its session: %+v", m)
	}
}

// Info commands refuse stray positionals and nonsense limits before
// answering anything.
func TestInfoCommandRefusals(t *testing.T) {
	chdirTemp(t)
	if code := catalogCmd([]string{"extra"}); code != 2 {
		t.Errorf("catalog positional exit = %d, want 2", code)
	}
	if code := historyCmd([]string{"-limit", "-1"}); code != 2 {
		t.Errorf("negative limit exit = %d, want 2", code)
	}
	if code := historyCmd([]string{"positional"}); code != 2 {
		t.Errorf("history positional exit = %d, want 2", code)
	}
	if code := topologyCmd([]string{"positional"}); code != 2 {
		t.Errorf("topology positional exit = %d, want 2", code)
	}
	if code := capsCmd([]string{"positional"}); code != 2 {
		t.Errorf("caps positional exit = %d, want 2", code)
	}
}
