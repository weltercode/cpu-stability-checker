package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/sanitize"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// bundleCmd (R09) writes a sanitized bug-report bundle: the evidence a
// maintainer needs, minus the machine's personal identity. The problem
// it solves is concrete — journals, saved requests, configs and CO
// receipts embed the hostname, the user name and the user's home
// paths (CO documents use hostname as an identity binding), so "here
// is my log file" historically meant "here is my machine's name".
//
// The honesty contract of this command:
//   - every artifact passes through internal/sanitize with the LIVE
//     OS identity (hostname, user, home), case-insensitively; no
//     curated blocklist that could silently be incomplete;
//   - the manifest names exactly which fields were replaced (by
//     length only, so writing the manifest cannot re-leak them) and
//     what was deliberately NOT claimed;
//   - after writing, every artifact is re-read and self-checked: if a
//     live identity string still occurs anywhere, the bundle is
//     reported UNSAFE (exit 1) rather than shipped — the promise is
//     enforced, not decorative;
//   - a missing optional artifact (no lastresult.json yet) is a named
//     manifest line, never a silently absent file.
func bundleCmd(args []string) int {
	fs := flag.NewFlagSet("bundle", flag.ExitOnError)
	out := fs.String("out", "", "bundle directory (default bug-report-<UTC timestamp>)")
	fs.Parse(args)
	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "bundle takes at most one journal path: bundle [journal] (-out dir)")
		return 2
	}
	journalPath := ""
	if fs.NArg() == 1 {
		journalPath = fs.Arg(0)
	}
	if journalPath == "" {
		journalPath = journal.Path(config.Load(config.DefaultPath).LogFile)
	}
	if *out == "" {
		*out = "bug-report-" + time.Now().UTC().Format("20060102-150405")
	}

	recs, iss := journal.ReadAll(journalPath)
	if len(recs) == 0 && iss.ReadErr != "" {
		fmt.Fprintf(os.Stderr, "bundle: cannot open journal %q: %v\n", journalPath, iss.ReadErr)
		return 2
	}
	// Setup passed: only now may anything be created on disk.
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "bundle: cannot create %q: %v\n", *out, err)
		return 2
	}
	reports := journal.RecoverRuns(recs)
	id := sanitize.FromOS()
	rules := sanitize.Rules(id)

	b := &bundleBuilder{dir: *out, rules: rules}

	// journal: the raw crash-recovery story, identity-scrubbed.
	b.lines("journal.jsonl", scrubLines(readAllBytes(journalPath), rules))

	// report: the same aggregation `report` prints, so the maintainer
	// sees exactly what the operator sees.
	var repText strings.Builder
	writeReportText(&repText, journalPath, reports, iss, journal.DetectClockAnomalies(reports))
	b.str("report.txt", sanitize.Scrub(repText.String(), rules))

	js := summaryToJSON(journalPath, journal.SummarizeCores(reports), journal.DetectClockAnomalies(reports), iss, wheaSummary(reports))
	js.Provenance = provenanceList(reports)
	js.RunDetails = runDetails(reports)
	if s, code := marshaled(js, "report"); code != 0 {
		return code
	} else {
		b.str("report.json", sanitize.Scrub(s, rules))
	}

	// config: the settings that produced the run. The config file
	// holds machine paths (tool locations); identity inside them is
	// scrubbed like anywhere else, and run_meta already carries the
	// config hash for integrity.
	if raw, err := os.ReadFile(config.DefaultPath); err == nil {
		b.bytes("config.json", scrubBytes(raw, rules))
	} else {
		b.missing = append(b.missing, fmt.Sprintf("config.json: not present (%v): the run's config_sha256 in run_meta still fingerprints it", err))
	}

	// lastresult: the durable outcome of the most recent run.
	if raw, err := os.ReadFile(lastResultPath); err == nil {
		b.bytes("lastresult.json", scrubBytes(raw, rules))
	} else {
		b.missing = append(b.missing, fmt.Sprintf("lastresult.json: not present (%v): the terminal verdict line from the run itself is not in this bundle", err))
	}

	// topology + caps: what the machine is and which telemetry is
	// alive HERE, now (a refusal or a degraded-monitoring report is
	// undebuggable without them).
	if top, err := sysinfo.DiscoverTopology(); err == nil {
		if s, code := marshaled(topologyJSON(top), "topology"); code != 0 {
			return code
		} else {
			b.str("topology.json", sanitize.Scrub(s, rules))
		}
	} else {
		b.missing = append(b.missing, fmt.Sprintf("topology.json: not collected (%v)", err))
	}
	if s, code := marshaled(capsJSON(), "capabilities"); code != 0 {
		_ = s
		return code
	} else {
		b.str("caps.json", sanitize.Scrub(s, rules))
	}
	var capsText strings.Builder
	writeCaps(&capsText)
	b.str("caps.txt", sanitize.Scrub(capsText.String(), rules))

	// Self-check before the manifest: re-read every written artifact
	// and demand that no live identity string survives. This is the
	// difference between "we tried to sanitize" and "this bundle does
	// not contain the machine's identity".
	var checkLines []string
	names := append([]string(nil), b.names...)
	sort.Strings(names)
	unsafe := false
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(b.dir, name))
		if err != nil {
			checkLines = append(checkLines, fmt.Sprintf("self_check %s: UNREADABLE (%v) — NOT PROVEN CLEAN", name, err))
			unsafe = true
			continue
		}
		if sanitize.Contains(string(raw), rules) {
			checkLines = append(checkLines, fmt.Sprintf("self_check %s: STILL CONTAINS LIVE IDENTITY", name))
			unsafe = true
		} else {
			checkLines = append(checkLines, fmt.Sprintf("self_check %s: clean", name))
		}
	}

	// Manifest last: it describes the final state of every artifact.
	var man strings.Builder
	fmt.Fprintf(&man, "cpu-stability-checker bug-report bundle — generated %s UTC\n", time.Now().UTC().Format(time.RFC3339))
	man.WriteString("Everything in this bundle has been through the identity scrubber; read verdicts from report.txt / report.json / lastresult.json.\n\n")
	man.WriteString("identity replacement (case-insensitive; raw, JSON-escaped and slashed path forms; longest first):\n")
	if len(rules) == 0 {
		man.WriteString("  no identity fields were observable from the OS — nothing was replaced\n")
	}
	tokenDesc := map[string]string{sanitize.HostToken: "machine hostname", sanitize.UserToken: "current user name", sanitize.HomeToken: "user home directory path"}
	for _, r := range rules {
		fmt.Fprintf(&man, "  %s <- %s, %d chars (value intentionally not echoed here; count it on your own machine)\n",
			r.Token, tokenDesc[r.Token], len(r.Needle))
	}
	man.WriteString("\nnot claimed by this bundle: any identity beyond the three fields above is NOT scrubbed by rule\n")
	man.WriteString("(share names, volume labels, tool paths that contain no user segment, WHEA event-log record ids travel as-is);\n")
	man.WriteString("if an artifact holds an identifier you personally recognize, edit it out before posting.\n\n")
	man.WriteString("artifacts (sha256 after scrubbing):\n")
	for _, e := range b.manifest {
		fmt.Fprintf(&man, "  %s  %d B  sha256 %s\n", e.name, e.size, e.sum)
	}
	if len(b.missing) > 0 {
		man.WriteString("\nnot included, by name:\n")
		for _, m := range b.missing {
			fmt.Fprintf(&man, "  %s\n", m)
		}
	}
	man.WriteString("\nthe bundle never pads or repairs evidence: journal parse problems appear as `journal issues` in report.txt exactly as the reader found them.\n\n")
	for _, l := range checkLines {
		man.WriteString(l + "\n")
	}
	if err := b.write("manifest.txt", []byte(man.String())); err != nil {
		fmt.Fprintf(os.Stderr, "bundle: cannot write manifest: %v\n", err)
		return 2
	}

	fmt.Printf("bundle written to %s (%d artifacts)\n", b.dir, len(b.names))
	for _, m := range b.missing {
		fmt.Printf("  not included: %s\n", m)
	}
	if unsafe {
		fmt.Fprintln(os.Stderr, "bundle is NOT safe to post: a live identity string survived the scrub — do not publish this directory; report this bundle+manifest to the maintainer through a private channel instead")
		return 1
	}
	fmt.Println("self-check: no live hostname/user/home string occurs in any artifact (verify by searching your own values before posting)")
	return 0
}

// bundleBuilder writes scrubbed artifacts into the bundle directory
// and accumulates the manifest rows (name, size, sha256) in order.
type bundleBuilder struct {
	dir      string
	rules    []sanitize.Rule
	names    []string
	manifest []bundleEntry
	missing  []string
}

type bundleEntry struct {
	name string
	size int64
	sum  string
}

func (b *bundleBuilder) lines(name string, ls []string) { b.str(name, strings.Join(ls, "\n")+"\n") }
func (b *bundleBuilder) str(name, s string)             { b.bytes(name, []byte(s)) }

func (b *bundleBuilder) bytes(name string, raw []byte) {
	if err := b.write(name, raw); err != nil {
		fmt.Fprintf(os.Stderr, "bundle: cannot write %s: %v\n", name, err)
		return
	}
	sum := sha256.Sum256(raw)
	b.names = append(b.names, name)
	b.manifest = append(b.manifest, bundleEntry{name: name, size: int64(len(raw)), sum: hex.EncodeToString(sum[:])})
}

func (b *bundleBuilder) write(name string, raw []byte) error {
	return os.WriteFile(filepath.Join(b.dir, name), raw, 0o644)
}

// readAllBytes returns nil on read failure: the journal's own state is
// already reported through journal.ReadAll (ReadErr), so the raw copy
// simply stays empty and the manifest's self-check says so.
func readAllBytes(path string) []byte {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return raw
}

func scrubLines(raw []byte, rules []sanitize.Rule) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		out = append(out, sanitize.Scrub(strings.TrimSuffix(l, "\r"), rules))
	}
	return out
}

func scrubBytes(raw []byte, rules []sanitize.Rule) []byte {
	return []byte(sanitize.Scrub(string(raw), rules))
}

// marshaled renders an answer exactly as writeJSONAnswer would print
// it, but returns the text; the exit code is 0 on success, 2 on an
// encoder failure (same vocabulary as every -json entry point).
func marshaled(v any, what string) (string, int) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "bundle: cannot encode %s: %v\n", what, err)
		return "", 2
	}
	return string(out) + "\n", 0
}

// capsJSON is the -json answer of capsCmd, shared so the bundle and
// the terminal can never disagree.
func capsJSON() capsAnswer {
	ans := capsAnswer{SchemaVersion: infoSchemaVersion}
	for _, c := range capProbes() {
		detail, ok := c.probe()
		ans.Capabilities = append(ans.Capabilities, capEntry{Name: c.name, Available: ok, Detail: detail})
	}
	return ans
}
