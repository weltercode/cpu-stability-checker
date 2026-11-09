package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/hwinfo"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/temperature"
	"github.com/weltercode/cpu-stability-checker/internal/whea"
	"github.com/weltercode/cpu-stability-checker/internal/wmisensor"
)

// capsCmd reports which telemetry sources are alive on this machine.
// It is the honest answer before trusting a long run's safety policy:
// what the tool can observe here, and what it can only pretend to.
// The probe list is shared with the -json answer: both shapes ask
// the same questions, so they can never disagree.
func capsCmd(args []string) int {
	fs := flag.NewFlagSet("caps", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "structured answer on stdout")
	fs.Parse(args)
	if refusePositionals(fs, "caps") {
		return 2
	}
	if *asJSON {
		ans := capsAnswer{SchemaVersion: infoSchemaVersion}
		for _, c := range capProbes() {
			detail, ok := c.probe()
			ans.Capabilities = append(ans.Capabilities, capEntry{Name: c.name, Available: ok, Detail: detail})
		}
		if err := writeJSONAnswer(ans); err != nil {
			fmt.Fprintf(os.Stderr, "cannot encode capabilities: %v\n", err)
			return 2
		}
		return 0
	}
	writeCaps(os.Stdout)
	return 0
}

// writeCaps renders the caps text for any writer: the terminal path
// and the bundle share one renderer (R09), so a bundled capabilities
// snapshot is exactly what the operator was told live.
func writeCaps(w io.Writer) {
	fmt.Fprintln(w, "telemetry capabilities:")
	for _, c := range capProbes() {
		capLine(w, c.name, c.probe)
	}
}

// capsAnswer is the machine-readable form: available is the same
// three-state honesty as the catalog's - here it is two-state
// because each probe either works or names why it does not, and
// that reason travels in detail.
type capsAnswer struct {
	SchemaVersion int        `json:"schema_version"`
	Capabilities  []capEntry `json:"capabilities"`
}

type capEntry struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Detail    string `json:"detail"` // "" = nothing more to say; when unavailable, the reason
}

type capProbe struct {
	name  string
	probe func() (string, bool)
}

func capProbes() []capProbe {
	return []capProbe{
		{"per-core idle accounting", coreTimesCap},
		{"cycle-time counters", cycleTimeCap},
		{"thermal zone temperature", temperatureCap},
		{"selected temperature sensor", selectedTemperatureCap},
		{"core parking state", parkingCap},
		{"WHEA event source", wheaCap},
		{"HWiNFO shared memory", hwinfoCap},
		{"LHM/OHM WMI sensors", wmiSensorCap},
		{"per-CPU performance clocks", perfClockCap},
	}
}

func selectedTemperatureCap() (string, bool) {
	m := temperature.New()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	// Probe only; no workload is started and the temporary limit is not a
	// user setting. It lets the normal selection/freshness path warm up.
	s := m.Prepare(ctx, 200)
	if !s.Known {
		return s.Reason, false
	}
	return fmt.Sprintf("%.1f°C — %s: %s", s.Reading.Celsius, s.Reading.Source, s.Reading.Sensor), true
}

// wmiSensorCap reports whether LibreHardwareMonitor or
// OpenHardwareMonitor is publishing sensors through WMI - the common
// free path to real clocks, temperatures and power without HWiNFO.
func wmiSensorCap() (string, bool) {
	s, err := wmisensor.Open()
	if err != nil {
		return err.Error(), false
	}
	defer s.Close()
	reads := s.Readings()
	if len(reads) == 0 {
		return "provider connected but published no sensors yet", false
	}
	return fmt.Sprintf("%d sensors, %d per-core clocks", len(reads), len(wmisensor.CoreClocks(reads))), true
}

// perfClockCap reports whether the PDH "% Processor Performance"
// counters plus a firmware-rated base clock can produce real per-CPU
// MHz without HWiNFO.
func perfClockCap() (string, bool) {
	p, err := sysinfo.OpenPerfClock()
	if err != nil {
		return err.Error(), false
	}
	defer p.Close()
	return fmt.Sprintf("rated base %d MHz (SMBIOS)", int(p.Base())), true
}

// hwinfoCap reports whether HWiNFO's shared-memory sensor feed is up —
// the only unprivileged path to real core clocks, voltages, CPU
// temperature and package power on this platform.
func hwinfoCap() (string, bool) {
	src, err := hwinfo.Open()
	if err != nil {
		return err.Error(), false
	}
	defer src.Close()
	readings := src.Readings()
	clocks := len(hwinfo.CoreClocks(readings))
	line := fmt.Sprintf("%d sensors, %d per-core clocks", len(readings), clocks)
	if topology, err := sysinfo.DiscoverTopology(); err == nil && !topology.Estimated {
		n := len(topology.PhysicalCores)
		line += fmt.Sprintf(" for %d physical cores", n)
		if clocks < n {
			line += " - some core clocks are unavailable; check HWiNFO sensor settings"
		}
	}
	return line, true
}

func capLine(w io.Writer, name string, probe func() (string, bool)) {
	detail, ok := probe()
	status := "unavailable"
	if ok {
		status = "available"
	}
	if detail != "" {
		fmt.Fprintf(w, "  %-30s %s: %s\n", name+" ....", status, detail)
		return
	}
	fmt.Fprintf(w, "  %-30s %s\n", name+" ....", status)
}

func coreTimesCap() (string, bool) {
	times, err := sysinfo.CoreTimes()
	if err != nil {
		return err.Error(), false
	}
	return fmt.Sprintf("%d logical CPUs (idle share quantized to ~15.6ms)", len(times)), true
}

func cycleTimeCap() (string, bool) {
	cycles, err := sysinfo.ProcessCycleTimeSelf()
	if err != nil {
		return err.Error(), false
	}
	return fmt.Sprintf("this process has consumed %d cycles", cycles), true
}

func temperatureCap() (string, bool) {
	sensor, err := sysinfo.NewTemperatureSensor()
	if err != nil {
		return err.Error(), false
	}
	defer sensor.Close()
	c, ok := sensor.Read()
	if !ok {
		return "counter present but no current sample", true
	}
	return fmt.Sprintf("%.1f°C — an ACPI board zone, not a CPU core", c), true
}

func parkingCap() (string, bool) {
	states, err := sysinfo.CpuSetStates()
	if err != nil {
		return err.Error(), false
	}
	parked := sysinfo.ParkedCount(states)
	note := "parking is not in use on this platform right now"
	if parked > 0 {
		note = "parking is active"
	}
	return fmt.Sprintf("%d/%d parked now; %s", parked, len(states), note), true
}

func wheaCap() (string, bool) {
	events, err := whea.QueryRecent(5)
	if err != nil {
		return err.Error(), false
	}
	return fmt.Sprintf("queried OK, %d recent event(s); absence of events is not unavailability", len(events)), true
}
