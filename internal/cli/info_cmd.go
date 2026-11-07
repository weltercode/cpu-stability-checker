package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// G01d (menu plan section 9): the read-only info surface a GUI uses
// to paint its pickers - catalog, topology, capabilities, history -
// plus the one-action repeat. Every command answers in two shapes:
// human text on stdout, or -json with one structured document from
// the same data. The JSON never carries ANSI, never invents a
// value: unavailable says why, unknown stays unknown.

// infoSchemaVersion versions the answer envelopes of this build.
const infoSchemaVersion = 1

// writeJSONAnswer is the single serializer for info answers: one
// indented document on stdout, a failed encode reported loudly (an
// answer that cannot be encoded must never exit as if it answered).
func writeJSONAnswer(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// refusePositionals keeps the info commands honest about their
// arguments: a stray positional is a refusal, not a shrug.
func refusePositionals(fs *flag.FlagSet, cmd string) bool {
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "%s takes no positional arguments\n", cmd)
		return true
	}
	return false
}

// --- catalog ---

type catalogAnswer struct {
	SchemaVersion int               `json:"schema_version"`
	Profiles      []catalog.Profile `json:"profiles"`
}

// catalogCmd prints the profile catalog: the same frozen cards the
// menu walks, with stable ids, explicit availability and the card's
// own reason when it is not available.
func catalogCmd(args []string) int {
	fs := flag.NewFlagSet("catalog", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "structured answer on stdout")
	fs.Parse(args)
	if refusePositionals(fs, "catalog") {
		return 2
	}
	if *asJSON {
		if err := writeJSONAnswer(catalogAnswer{SchemaVersion: infoSchemaVersion, Profiles: catalog.All()}); err != nil {
			fmt.Fprintf(os.Stderr, "cannot encode catalog: %v\n", err)
			return 2
		}
		return 0
	}
	for _, p := range catalog.All() {
		line := fmt.Sprintf("%-22s %-4s %-11s %s", p.ID, p.Version, p.Availability, p.Title)
		if p.Availability != catalog.Available && p.Reason != "" {
			line += " — " + p.Reason
		}
		fmt.Println(line)
	}
	return 0
}

// --- topology ---

// topologyAnswer is the protocol view of the topology: the cli
// shapes the wire format itself (fixtures pin it), so sysinfo may
// rename Go fields without breaking a GUI.
type topologyAnswer struct {
	SchemaVersion int            `json:"schema_version"`
	Source        string         `json:"source"`
	Estimated     bool           `json:"estimated"` // true = the platform hid cache topology: grouping is inferred
	PhysicalCores []topologyCore `json:"physical_cores"`
}

type topologyCore struct {
	Index           int           `json:"index"`
	Group           uint16        `json:"group"`
	NativeIndex     uint8         `json:"native_index"`
	NUMANode        uint8         `json:"numa_node"`
	EfficiencyClass uint8         `json:"efficiency_class"`
	CCXIndex        int           `json:"ccx_index"` // -1 when ccx_known is false
	CCXKnown        bool          `json:"ccx_known"`
	LogicalCPUs     []topologyCPU `json:"logical_cpus"`
}

type topologyCPU struct {
	Group         uint16 `json:"group"`
	Number        uint8  `json:"number"`
	CPUSetID      uint32 `json:"cpuset_id"`
	CPUSetIDKnown bool   `json:"cpuset_id_known"`
	Parked        bool   `json:"parked"`
}

func topologyJSON(topology sysinfo.Topology) topologyAnswer {
	ans := topologyAnswer{
		SchemaVersion: infoSchemaVersion,
		Source:        topology.Source,
		Estimated:     topology.Estimated,
	}
	for _, c := range topology.PhysicalCores {
		tc := topologyCore{
			Index:           c.Index,
			Group:           c.Group,
			NativeIndex:     c.NativeIndex,
			NUMANode:        c.NUMANode,
			EfficiencyClass: c.EfficiencyClass,
			CCXIndex:        c.CCXIndex,
			CCXKnown:        c.CCXKnown,
		}
		for _, l := range c.LogicalCPUs {
			tc.LogicalCPUs = append(tc.LogicalCPUs, topologyCPU{
				Group:         l.Group,
				Number:        l.Number,
				CPUSetID:      l.CPUSetID,
				CPUSetIDKnown: l.CPUSetIDKnown,
				Parked:        l.Parked,
			})
		}
		ans.PhysicalCores = append(ans.PhysicalCores, tc)
	}
	return ans
}

// --- history ---

type historyAnswer struct {
	SchemaVersion int            `json:"schema_version"`
	Total         int            `json:"total"` // sessions found, before any -limit cut
	Sessions      []historyEntry `json:"sessions"`
	// Warnings name manifest files that could not be read: the list
	// shows what is readable and says what is not, never rewrites
	// or deletes the unreadable ones.
	Warnings []string `json:"warnings"`
}

type historyEntry struct {
	SessionID  string `json:"session_id"`
	Kind       string `json:"kind"`
	State      string `json:"state"` // "running" after a crash = unfinished, never rewritten
	Started    string `json:"started"`
	Finished   string `json:"finished"` // "" = never finalized: unknown
	Exit       *int   `json:"exit"`     // null = never ended observably
	Validation string `json:"validation"`
	RecipeID   string `json:"recipe_id"`
	ProfileID  string `json:"profile_id"`
	Runs       int    `json:"runs"`
}

// historyCmd lists session manifests newest first. The full record
// of one session (its replay request, its run files) stays in
// history/<session-id>.json; this list is the index view, and the
// scan itself is rebuildable - deleting the list changes nothing
// about what history can be read back.
func historyCmd(args []string) int {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "structured answer on stdout")
	limit := fs.Int("limit", 0, "show at most this many sessions (0 = all)")
	fs.Parse(args)
	if refusePositionals(fs, "history") {
		return 2
	}
	if *limit < 0 {
		fmt.Fprintln(os.Stderr, "history -limit cannot be negative")
		return 2
	}
	list, warns := listSessionManifests()
	if *asJSON {
		ans := historyAnswer{SchemaVersion: infoSchemaVersion, Total: len(list), Warnings: warns}
		shown := list
		if *limit > 0 && len(shown) > *limit {
			shown = shown[:*limit]
		}
		for _, m := range shown {
			ans.Sessions = append(ans.Sessions, historyEntry{
				SessionID:  m.SessionID,
				Kind:       m.Kind,
				State:      m.State,
				Started:    m.Started,
				Finished:   m.Finished,
				Exit:       m.Exit,
				Validation: m.Validation,
				RecipeID:   m.RecipeID,
				ProfileID:  m.ProfileID,
				Runs:       len(m.Runs),
			})
		}
		if err := writeJSONAnswer(ans); err != nil {
			fmt.Fprintf(os.Stderr, "cannot encode history: %v\n", err)
			return 2
		}
		return 0
	}
	if len(list) == 0 {
		fmt.Println("no sessions recorded yet.")
	}
	shown := list
	if *limit > 0 && len(shown) > *limit {
		shown = shown[:*limit]
	}
	for _, m := range shown {
		exit := "unknown"
		if m.Exit != nil {
			exit = fmt.Sprintf("%d", *m.Exit)
		}
		fmt.Printf("%s  %-8s %-13s started %s  exit %s  runs %d\n",
			m.SessionID, m.Kind, m.State, m.Started, exit, len(m.Runs))
	}
	if len(list) > len(shown) {
		fmt.Printf("shown %d of %d sessions.\n", len(shown), len(list))
	}
	for _, w := range warns {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	return 0
}

// --- repeat ---

// repeatCmd re-runs the saved request with one action (section 5):
// exactly the code the menu's repeat item uses, so the CLI word and
// the menu button can never drift. A GUI may instead replay any
// history request through run -request; this is the human one-word
// form over the same pipeline.
func repeatCmd() int {
	if _, err := loadLastRun(); err != nil {
		fmt.Fprintf(os.Stderr, "repeat refused: %v\n", err)
		return 2
	}
	return repeatLastRunWithUI(nil)
}
