package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/weltercode/cpu-stability-checker/internal/config"
)

func configCmd(args []string) int {
	if len(args) >= 1 && args[0] == "init" {
		// The sample is a starting point, not a licence to destroy:
		// config.json is where a machine's core selectors, durations,
		// memory budget and registered external-tool paths live. An
		// overwrite would silently discard the work `tools install`
		// and the wizards did, so the default answer is no and the
		// operator has to say otherwise.
		force := false
		for _, a := range args[1:] {
			if a == "-force" || a == "--force" {
				force = true
				continue
			}
			fmt.Fprintf(os.Stderr, "config init: unknown argument %q (only -force is accepted)\n", a)
			return 2
		}
		if _, err := os.Stat(config.DefaultPath); err == nil && !force {
			fmt.Fprintf(os.Stderr, "config init refused: %s already exists and holds this machine's settings (cores, durations, memory budget, registered tools). Edit that file, or pass -force to replace it with the sample.\n", config.DefaultPath)
			return 2
		}
		cfg := config.Sample(nil)
		if err := cfg.Save(config.DefaultPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		fmt.Printf("wrote %s (edit it to preset cores/workloads, then re-run)\n", config.DefaultPath)
		return 0
	}
	cfg := config.Load(config.DefaultPath)
	out, _ := json.MarshalIndent(cfg, "", "  ")
	fmt.Println(string(out))
	return 0
}
