package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

func topologyCmd(args []string) int {
	fs := flag.NewFlagSet("topology", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "structured answer on stdout")
	fs.Parse(args)
	if refusePositionals(fs, "topology") {
		return 2
	}
	topology, err := sysinfo.DiscoverTopology()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot discover CPU topology: %v\n", err)
		return 2
	}
	if *asJSON {
		if err := writeJSONAnswer(topologyJSON(topology)); err != nil {
			fmt.Fprintf(os.Stderr, "cannot encode topology: %v\n", err)
			return 2
		}
		return 0
	}
	sysinfo.WriteTopology(os.Stdout, topology)
	return 0
}
