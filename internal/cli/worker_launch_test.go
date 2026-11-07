package cli

import (
	"os"
	"strings"
	"testing"
)

// This is the entry point that used to launch cli.test.exe once per CPU,
// with each child running this suite again. There is intentionally no helper
// dispatch in the CLI test binary: the launcher must refuse it immediately.
func TestRunCmdTestBinaryCannotSpawnWorkers(t *testing.T) {
	for _, mode := range []string{"-per-process", "-bursts"} {
		t.Run(mode, func(t *testing.T) {
			chdirTemp(t)
			f, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			old := os.Stderr
			os.Stderr = f
			t.Cleanup(func() { os.Stderr = old; f.Close() })
			args := []string{"-test", "general", "-a", "int", "-cores", "0", "-control", "-1", mode}
			if mode == "-bursts" {
				args = append(args, "1", "-burst-run", "1ms")
			}
			if code := runCmd(args); code != 2 {
				t.Fatalf("exit=%d, want setup error before starting a child", code)
			}
			data, err := os.ReadFile(f.Name())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "Go test binary needs an explicit worker helper") {
				t.Fatalf("wrong refusal: %s", data)
			}
		})
	}
}
