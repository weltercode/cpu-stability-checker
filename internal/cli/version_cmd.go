package cli

import (
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// versionCmd is the read-only build-and-machine identity surface
// (queue R16). Evidence is only as good as its provenance: a journal,
// a screenshot or a bug report that cannot name the exact build and
// the exact machine it came from cannot be honestly compared with
// another one, and a user who is being asked "which version?" should
// not have to guess from a download page.
//
// The command never starts a load, never touches firmware settings and
// never blocks: it reads the VCS stamp Go embedded at build time, the
// runtime's own facts, and one read-only firmware query for the memory
// profile. Everything it cannot observe prints as "unknown" - never a
// blank, never a zero, never a plausible guess.
//
// What it says about memory is what firmware WROTE, under the names
// SMBIOS attaches to those offsets. The three speed fields are kept
// separate and unreconciled on purpose: this project's own dev-board
// firmware (verified live 2026-11-09) writes SPD speed 4800, max
// capability 5600 and a voltage-like 1100 in the slot the spec calls
// "configured memory clock speed". Reconciling them would be inventing
// a hardware fact, and calling one of them "your running speed" would
// be a diagnosis this tool does not make. Serial numbers and asset
// tags are never collected upstream, so they cannot appear here.

// versionReading is the constant disclaimer: the machine-readable
// answer carries it too, so the observation-versus-diagnosis line does
// not vanish exactly where an automated reader consumes it.
const versionReading = "firmware observations recorded verbatim; not a stability or clock diagnosis; serial numbers and asset tags are not collected"

// versionFacts is everything the answer is made of, gathered in one
// place so the human text and the -json document are rendered from the
// same values (two collectors would be two chances to disagree).
type versionFacts struct {
	Meta    journal.RunMetaFields // module version, VCS rev/modified/time (empty = not stamped)
	GoVer   string
	GOOS    string
	GOARCH  string
	Machine sysinfo.MachineIdentity
	Memory  *journal.RunMemoryFields // never nil: a refusal is recorded as UnknownReason
}

func collectVersionFacts() versionFacts {
	return versionFacts{
		Meta:    journal.BuildProvenance(),
		GoVer:   runtime.Version(),
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
		Machine: sysinfo.DiscoverMachineIdentity(),
		Memory:  plan.CollectMemoryProvenance(),
	}
}

// versionAnswer is the protocol shape of this answer. Empty optional
// strings are omitted (absent = not observed), pointer fields keep the
// difference between "unknown" and "reported zero", and the memory
// block is exactly the block a journal records - same mapper, so the
// printed answer and the journaled evidence can never drift.
type versionAnswer struct {
	SchemaVersion int                      `json:"schema_version"`
	ModuleVersion string                   `json:"module_version,omitempty"`
	BuildRev      string                   `json:"build_rev,omitempty"`
	BuildModified *bool                    `json:"build_modified,omitempty"` // nil = not stamped: unknown
	BuildTime     string                   `json:"build_time,omitempty"`
	GoVersion     string                   `json:"go_version"`
	GOOS          string                   `json:"goos"`
	GOARCH        string                   `json:"goarch"`
	CPUName       string                   `json:"cpu_name,omitempty"`
	OSVersion     string                   `json:"os_version,omitempty"`
	BIOSVersion   string                   `json:"bios_version,omitempty"` // reserved: never populated (unknown, not "no BIOS")
	Memory        *journal.RunMemoryFields `json:"memory,omitempty"`
	Reading       string                   `json:"reading"`
}

// versionOrUnknown renders an unobserved identity string as "unknown"
// (the journal package has the same rule for its own output).
func versionOrUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func versionJSON(f versionFacts) versionAnswer {
	return versionAnswer{
		SchemaVersion: infoSchemaVersion,
		ModuleVersion: f.Meta.ModuleVer,
		BuildRev:      f.Meta.BuildRev,
		BuildModified: f.Meta.BuildModified,
		BuildTime:     f.Meta.BuildTime,
		GoVersion:     f.GoVer,
		GOOS:          f.GOOS,
		GOARCH:        f.GOARCH,
		CPUName:       f.Machine.CPUName,
		OSVersion:     f.Machine.OSVersion,
		BIOSVersion:   f.Machine.BIOSVersion,
		Memory:        f.Memory,
		Reading:       versionReading,
	}
}

// printVersionHuman writes the human answer. The memory lines reuse
// the journal renderers, so the same words mean the same thing in a
// report, a journal print and this command.
func printVersionHuman(f versionFacts) {
	mod := "unknown"
	if f.Meta.BuildModified != nil {
		mod = map[bool]string{true: "yes", false: "no"}[*f.Meta.BuildModified]
	}
	fmt.Println("cpu-stability-checker")
	fmt.Printf("  build:    %s rev %s (modified: %s) built %s\n",
		versionOrUnknown(f.Meta.ModuleVer), versionOrUnknown(f.Meta.BuildRev),
		mod, versionOrUnknown(f.Meta.BuildTime))
	fmt.Printf("  runtime:  %s %s/%s\n", f.GoVer, f.GOOS, f.GOARCH)
	fmt.Printf("  cpu:      %q\n", versionOrUnknown(f.Machine.CPUName))
	fmt.Printf("  os:       %q\n", versionOrUnknown(f.Machine.OSVersion))
	fmt.Printf("  bios:     unknown (not collected)\n")
	fmt.Println("  " + journal.FormatMemoryObservations(f.Memory))
	for _, d := range f.Memory.Devices {
		fmt.Printf("    slot %q bank %q %s: %s\n",
			versionOrUnknown(d.Locator), versionOrUnknown(d.BankLocator),
			versionOrUnknown(d.Manufacturer), journal.FormatMemoryDevice(d))
	}
	fmt.Println()
	fmt.Println("Reading: " + versionReading + ".")
	if f.Meta.ModuleVer == "" && f.Meta.BuildRev == "" {
		fmt.Println("This binary carries no VCS stamp (a local or vendored build): " +
			"its identity cannot be recovered from itself, so journals it writes " +
			"say so instead of inventing one.")
	}
}

// versionCmd answers the identity question in both shapes the info
// surface uses: human text on stdout, or one structured document with
// -json. Informational only: it answers on every machine and never
// fails the caller (a machine it knows nothing about still gets an
// honest "unknown" answer, exit 0).
func versionCmd(args []string) int {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "structured answer on stdout")
	fs.Parse(args)
	if refusePositionals(fs, "version") {
		return 2
	}
	f := collectVersionFacts()
	if *asJSON {
		if err := writeJSONAnswer(versionJSON(f)); err != nil {
			fmt.Fprintf(os.Stderr, "cannot encode version: %v\n", err)
			return 2
		}
		return 0
	}
	printVersionHuman(f)
	return 0
}
