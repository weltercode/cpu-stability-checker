// vtprobe opens a real console window and records what tui.Probe and
// tui.Size report there, so VT/screen-mode failures can be diagnosed
// without a TTY: run it via Start-Process and read the result file.
package main

import (
	"fmt"
	"os"

	"github.com/weltercode/cpu-stability-checker/internal/tui"
)

func main() {
	out := os.Args[1]
	caps := tui.Probe(os.Stdout)
	cols, rows, ok := tui.Size(os.Stdout)
	f, err := os.Create(out)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "terminal=%v vt=%v size=%dx%d sizeok=%v\n",
		caps.Terminal, caps.VT, cols, rows, ok)
}
