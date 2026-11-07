package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/weltercode/cpu-stability-checker/internal/config"
)

func configCmd(args []string) int {
	if len(args) >= 1 && args[0] == "init" {
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
