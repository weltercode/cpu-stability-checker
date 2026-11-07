package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/co"
	"github.com/weltercode/cpu-stability-checker/internal/co/ryzensmucli"
	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/cotx"
)

// coCmd is the CO surface: read-only probe/topology/read, plus the
// bounded hardware-verification round-trip (verify-hardware) and its
// safety valve (recover). verify-hardware is NOT a general apply: it
// moves exactly one core by at most 2 counts from the value the
// machine holds right now, through the durable transaction engine, and
// restores that one core to its exact original - the P10d "separately
// permitted apply/restore," gated on the user's explicit action (the
// typed -core/-delta flags) and on a build validated against the
// pinned provenance. Arbitrary multi-core tuning is not exposed.
//
// The tool path is always explicit (-tool flag or config co_tool) -
// nothing is silently searched in PATH. Setup/refusal failures are
// exit 2 (no hardware change); a transaction that did not cleanly
// restore is exit 1 (the hardware may be modified - run `co recover`).
// The JSON output never guesses over an unknown and never carries ANSI.
func coCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: co probe|topology|read|verify-hardware|recover|receipt [-tool PATH] [-json] [-timeout DUR]")
		fmt.Fprintln(os.Stderr, "  co verify-hardware -core coreN -delta D   bounded single-core CO round-trip (needs elevation)")
		fmt.Fprintln(os.Stderr, "  co recover                                 reconcile an interrupted transaction (needs elevation)")
		fmt.Fprintln(os.Stderr, "  co receipt                                 show the verified-write-path receipt (read-only)")
		return 2
	}
	sub := args[0]
	switch sub {
	case "probe", "topology", "read", "verify-hardware", "recover", "receipt":
	default:
		fmt.Fprintf(os.Stderr, "co: unknown subcommand %q (probe|topology|read|verify-hardware|recover|receipt)\n", sub)
		return 2
	}
	fs := flag.NewFlagSet("co "+sub, flag.ContinueOnError)
	tool := fs.String("tool", "", "path to the CO tool executable (explicit; config co_tool also honored)")
	jsonOut := fs.Bool("json", false, "machine-readable output (never ANSI)")
	// The write path makes many elevated tool calls; give it a longer
	// default bound than the read-only diagnostics.
	timeoutDefault := 30 * time.Second
	if sub == "verify-hardware" || sub == "recover" {
		timeoutDefault = 120 * time.Second
	}
	timeout := fs.Duration("timeout", timeoutDefault, "bound for the whole command")
	var core string
	var delta int
	if sub == "verify-hardware" {
		fs.StringVar(&core, "core", "", "logical core to nudge then restore, e.g. core1 (required)")
		fs.IntVar(&delta, "delta", 0, "CO delta to apply then undo, one of -2,-1,1,2 (required)")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	path := *tool
	if path == "" {
		path = config.Load(config.DefaultPath).CoTool
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "co: no CO tool configured: pass -tool PATH or set co_tool in config.json")
		fmt.Fprintln(os.Stderr, "  release builds (source sp00n/ryzen-smu-cli): https://github.com/rawhide-kobayashi/ryzen-smu-cli/releases")
		fmt.Fprintln(os.Stderr, "  the tool needs elevation and the .NET 8 Desktop Runtime")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	backend := ryzensmucli.New(path, nil)
	switch sub {
	case "probe":
		return coProbe(ctx, backend, *jsonOut)
	case "topology":
		return coTopology(ctx, backend, *jsonOut)
	case "verify-hardware":
		return coVerifyHardware(ctx, backend, core, delta, *jsonOut)
	case "recover":
		return coRecover(ctx, backend, *jsonOut)
	case "receipt":
		return coReceipt(ctx, backend, *jsonOut)
	default:
		return coRead(ctx, backend, *jsonOut)
	}
}

func coProbe(ctx context.Context, b co.Backend, jsonOut bool) int {
	caps, err := b.Probe(ctx)
	if err != nil {
		return coFail("probe", err, jsonOut)
	}
	if jsonOut {
		return coJSON(caps)
	}
	fmt.Printf("co probe: %s version %s\n", caps.Tool, orUnknown(caps.Version))
	if caps.SHA256 != "" {
		fmt.Printf("  exe sha256: %s\n", caps.SHA256)
	}
	if caps.PayloadSHA256 != "" {
		fmt.Printf("  payload sha256: %s\n", caps.PayloadSHA256)
	} else {
		fmt.Println("  payload sha256: unknown (the managed payload beside the launcher was not readable)")
	}
	fmt.Printf("  supported: %s\n", triLine(caps.Supported))
	fmt.Printf("  read:      %s\n", triLine(caps.Read))
	fmt.Printf("  apply:     %s\n", triLine(caps.Apply))
	fmt.Printf("  readback:  %s\n", triLine(caps.Readback))
	if caps.RangeKnown {
		fmt.Printf("  range:     [%d..%d]\n", caps.RangeMin, caps.RangeMax)
	} else {
		fmt.Println("  range:     unknown (hardware-validated span never measured)")
	}
	if caps.SyntaxKnown {
		fmt.Printf("  syntax:    [%d..%d] (tool parse limit, not hardware span)\n", caps.SyntaxMin, caps.SyntaxMax)
	}
	for _, n := range caps.Notes {
		fmt.Printf("  note: %s\n", n)
	}
	return 0
}

func coTopology(ctx context.Context, b co.Backend, jsonOut bool) int {
	m, err := b.Topology(ctx)
	if err != nil {
		return coFail("topology", err, jsonOut)
	}
	if jsonOut {
		return coJSON(m)
	}
	fmt.Printf("co topology: %d addressable cores (mapping quality: %s)\n", len(m.Slots), orUnknown(m.Quality))
	for _, s := range m.Slots {
		fmt.Printf("  %-8s smu=%s os=%s\n", s.ToolID, intTri(s.SMUIndex), intTri(s.OSLogical))
		for _, n := range s.Notes {
			fmt.Printf("    note: %s\n", n)
		}
	}
	for _, n := range m.Notes {
		fmt.Printf("  note: %s\n", n)
	}
	return 0
}

func coRead(ctx context.Context, b co.Backend, jsonOut bool) int {
	// Establish tool identity first: the snapshot's hardware
	// fingerprint needs the probe's tool name/version/exe digest, and
	// without them a full read stays "partial" (identity unconfirmed,
	// values not attributable to a confirmed machine). The
	// transaction engine runs probe->topology->read for exactly this
	// reason; a standalone read does the same so its snapshot is
	// exact and fingerprinted.
	if _, err := b.Probe(ctx); err != nil {
		return coFail("read", err, jsonOut)
	}
	s, err := b.Read(ctx)
	if err != nil {
		return coFail("read", err, jsonOut)
	}
	if jsonOut {
		return coJSON(s)
	}
	fmt.Printf("co read: %d offsets (quality: %s)\n", len(s.Profile.Offsets), orUnknown(s.Quality))
	for _, o := range s.Profile.Offsets {
		fmt.Printf("  %-8s %d\n", o.Core, o.Value)
	}
	for _, n := range s.Notes {
		fmt.Printf("  note: %s\n", n)
	}
	return 0
}

// coVerifyHardware runs the bounded single-core bootstrap round-trip
// through the durable transaction engine: read the exact original,
// move ONE core by delta (<=2 counts, and only on a build validated
// against the pinned provenance), confirm the change by independent
// readback, then restore that one core to its exact original and
// confirm again. It is the P10d separately-permitted apply/restore,
// not a general tuning surface.
func coVerifyHardware(ctx context.Context, b *ryzensmucli.Adapter, core string, delta int, jsonOut bool) int {
	if core == "" {
		fmt.Fprintln(os.Stderr, "co verify-hardware: -core is required (e.g. -core core1)")
		return 2
	}
	if delta < -2 || delta > 2 || delta == 0 {
		fmt.Fprintln(os.Stderr, "co verify-hardware: -delta must be one of -2,-1,1,2 (a bounded single-core nudge)")
		return 2
	}
	e := &cotx.Engine{Backend: b} // default absolute, cwd-independent lock/store paths
	res, err := e.BootstrapCore(ctx, core, delta)
	// On a confirmed, durable restore, record the identity-bound
	// evidence that the write path works on this machine+build. A
	// failure to record it does not change the transaction outcome
	// (the hardware is already restored) and is reported honestly.
	if res != nil && res.Status == cotx.StatusRestored {
		if from, to, ok := coreDelta(res, core); ok {
			if rerr := b.WriteVerifiedReceipt(res.TxID, core, from, to); rerr != nil {
				fmt.Fprintf(os.Stderr, "co verify-hardware: round-trip restored, but the verified-write receipt was not recorded: %v\n", rerr)
			}
		}
	}
	return coTxResult("verify-hardware", res, err, jsonOut)
}

// coreDelta pulls the touched core's before -> applied values out of
// the durable document, for the receipt.
func coreDelta(res *cotx.Result, core string) (from, to int, ok bool) {
	before := map[string]int{}
	for _, o := range res.Doc.State.Before.Profile.Offsets {
		before[o.Core] = o.Value
	}
	for _, o := range res.Doc.State.Desired.Offsets {
		if o.Core == core {
			return before[core], o.Value, true
		}
	}
	return 0, 0, false
}

// coReceipt reports the durable verified-write-path receipt and whether
// it still applies to the current machine, build and topology. It is
// read-only: it establishes identity (probe + topology) to validate the
// receipt, but never writes or authorizes anything.
func coReceipt(ctx context.Context, b *ryzensmucli.Adapter, jsonOut bool) int {
	if _, err := b.Probe(ctx); err != nil {
		return coFail("receipt", err, jsonOut)
	}
	if _, err := b.Topology(ctx); err != nil {
		return coFail("receipt", err, jsonOut)
	}
	st, err := b.CheckVerifiedReceipt()
	if err != nil {
		return coFail("receipt", err, jsonOut)
	}
	if jsonOut {
		if !st.Found {
			return coJSON(struct {
				Found bool `json:"found"`
			}{false})
		}
		return coJSON(st)
	}
	if !st.Found {
		fmt.Println("co receipt: no verified-write receipt recorded on this machine")
		return 0
	}
	r := st.Receipt
	fmt.Printf("co receipt: core %s %d->%d via tx %s at %s\n", r.Core, r.FromValue, r.ToValue, orUnknown(r.TxID), r.VerifiedAt)
	fmt.Printf("  bound to: tool %s version %s exe %s payload %s host %s topo %s\n", r.Tool, orUnknown(r.Version), r.ExeSHA256, orUnknown(r.PayloadSHA256), r.Host, r.TopoID)
	if st.Applies {
		fmt.Println("  status: APPLIES to this machine/build/topology (write path verified here)")
		return 0
	}
	fmt.Println("  status: DOES NOT apply to the current machine/build/topology:")
	for _, why := range st.Reasons {
		fmt.Printf("    - %s\n", why)
	}
	return 0
}

// coRecover reconciles an interrupted transaction from a fresh read:
// if the machine still shows the bootstrapped value it restores the
// saved original; if it already shows the original it closes honestly;
// anything else stops without overwriting. It never re-applies.
func coRecover(ctx context.Context, b co.Backend, jsonOut bool) int {
	e := &cotx.Engine{Backend: b}
	res, err := e.Recover(ctx)
	return coTxResult("recover", res, err, jsonOut)
}

// coTxResult reports a transaction outcome and maps it to an exit
// code. No result at all means the transaction never reached the
// hardware (setup failure, refusal before any write, lock held): exit
// 2 by kind. A result whose durable status is terminal-clean (restored
// / closed_no_change) with no trailing error is exit 0. Any other
// status means the hardware may be left modified: exit 1 and tell the
// operator to run `co recover` before starting anything new.
//
// Audit C03: the exit code is computed ONCE from the outcome, and the
// output format only chooses the presentation - a JSON consumer
// (script, future GUI) must never see success where the text operator
// sees a fault. A JSON write failure is non-zero regardless of status:
// a report the consumer never received is not a success.
func coTxResult(op string, res *cotx.Result, err error, jsonOut bool) int {
	if res == nil {
		return coFail(op, err, jsonOut) // nothing was written; report by kind
	}
	kind := co.KindOf(err)
	clean := res.Status == cotx.StatusRestored || res.Status == cotx.StatusClosedNoChange
	code := 1
	if clean && err == nil {
		code = 0
	}
	trailing := ""
	if err != nil {
		trailing = fmt.Sprintf("durable status %s, but a step reported: %v", res.Status, err)
	}
	if jsonOut {
		rep := struct {
			TxID   string      `json:"txid"`
			Status string      `json:"status"`
			Kind   string      `json:"kind,omitempty"`
			Error  string      `json:"error,omitempty"` // trailing step error, same fact the text branch prints
			Notes  []string    `json:"notes,omitempty"`
			Doc    *cotx.TxDoc `json:"doc,omitempty"`
		}{TxID: res.TxID, Status: res.Status, Kind: kind, Error: trailing, Notes: res.Notes, Doc: &res.Doc}
		// The operator warning rides stderr in BOTH formats - it
		// never pollutes the JSON contract on stdout.
		if !clean {
			fmt.Fprintf(os.Stderr, "co %s: hardware may be left modified (status %s); run `co recover` before any new apply\n", op, res.Status)
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "co %s: %s\n", op, trailing)
		}
		if c := coJSON(rep); c != 0 {
			return c
		}
		return code
	}
	fmt.Printf("co %s: tx %s status %s\n", op, orUnknown(res.TxID), res.Status)
	if kind != "" {
		fmt.Printf("  kind: %s\n", kind)
	}
	for _, n := range res.Notes {
		fmt.Printf("  note: %s\n", n)
	}
	switch {
	case clean && err == nil:
		fmt.Printf("  result: %s\n", txResultLine(res))
		return 0
	case clean:
		// Durable status is clean but a trailing step reported an
		// error: surface it, do not hide it behind exit 0.
		fmt.Fprintf(os.Stderr, "co %s: %s\n", op, trailing)
		return 1
	default:
		fmt.Fprintf(os.Stderr, "co %s: hardware may be left modified (status %s); run `co recover` before any new apply\n", op, res.Status)
		return 1
	}
}

// txResultLine summarizes the round-trip from the durable document:
// the touched core's before -> applied -> (restored to before). Only
// cores whose value actually differs are shown, so a single-core
// bootstrap prints exactly one line.
func txResultLine(res *cotx.Result) string {
	before := map[string]int{}
	for _, o := range res.Doc.State.Before.Profile.Offsets {
		before[o.Core] = o.Value
	}
	var parts []string
	for _, o := range res.Doc.State.Desired.Offsets {
		if b, ok := before[o.Core]; ok && b != o.Value {
			verb := "restored to"
			if res.Status == cotx.StatusClosedNoChange {
				verb = "unchanged at"
			}
			parts = append(parts, fmt.Sprintf("%s %d->%d %s %d", o.Core, b, o.Value, verb, b))
		}
	}
	if len(parts) == 0 {
		return "no core value changed"
	}
	return strings.Join(parts, "; ")
}

// coFail reports the typed kind, never a bare message: the reader
// learns WHICH branch failed (no privilege, unsupported CPU, malformed
// output...) without the tool guessing between them. Setup-stage
// failure by kind: exit 2 in BOTH formats (audit C03) - the JSON form
// carries the same verdict as an object instead of a text line, and
// still stays non-zero if writing that object fails. No result here
// ever means a hardware change happened.
func coFail(op string, err error, jsonOut bool) int {
	if jsonOut {
		coJSON(struct {
			Op    string `json:"op"`
			Kind  string `json:"kind,omitempty"`
			Error string `json:"error"`
		}{Op: op, Kind: co.KindOf(err), Error: fmt.Sprint(err)})
		return 2
	}
	if k := co.KindOf(err); k != "" {
		fmt.Fprintf(os.Stderr, "co %s: %s: %v\n", op, k, err)
		return 2
	}
	fmt.Fprintf(os.Stderr, "co %s: %v\n", op, err)
	return 2
}

func coJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "co: json: %v\n", err)
		return 2
	}
	return 0
}

// orUnknown keeps empty optional strings honest in human output.
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func triLine(t co.TriState) string {
	if !t.Known {
		return "unknown"
	}
	if t.Yes {
		return "yes"
	}
	return "no"
}

func intTri(p *int) string {
	if p == nil {
		return "unknown"
	}
	return fmt.Sprintf("%d", *p)
}
