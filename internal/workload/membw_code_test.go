package workload

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// The plan demands the machine code itself be checked, not a source
// claim: the emitted runMemBW must contain real stores (the write
// loop) and real loads (the strided reads feeding the checksum). If a
// future compiler optimizes the accesses away, this test fails loudly
// instead of the workload silently becoming a CPU spin with no memory
// traffic.
//
// The probe is built with the same default flags as a release build
// (the toolchain and command are logged so a failure reproduces
// exactly); objdumping the go-test temp binary is not an option -
// measured: it carries no pclntab symbol for objdump to work with.
func TestMemBWMachineCodeHasRealAccesses(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skipf("objdump assertions are written for amd64, running on %s", runtime.GOARCH)
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go tool not found next to the test: %v", err)
	}
	// go test runs with the cwd at the package source dir; walk up to
	// the module root so the main package can be built.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package dir")
		}
		dir = parent
	}
	probe := filepath.Join(t.TempDir(), "probe.exe")
	t.Logf("toolchain: %s %s/%s; building %s -> %s", runtime.Version(), runtime.GOOS, runtime.GOARCH, dir, probe)
	if out, err := exec.Command(goTool, "build", "-o", probe, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	out, err := exec.Command(goTool, "tool", "objdump", "-s", "runMemBW", probe).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go tool objdump: %v, stderr: %s", err, ee.Stderr)
		}
		t.Fatalf("go tool objdump: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "runMemBW") {
		t.Fatalf("objdump found no runMemBW text at all:\n%s", head(text))
	}
	// Go's disassembly addresses slice slots as (BASE)(INDEX*SCALE).
	// The checks below pin exactly that traffic, so stack spills
	// (0x40(SP), no scale) can never satisfy them:
	//
	//	write loop survived: a read-modify-write on a slice slot
	//	(INCQ 0(DX)(CX*8)) or a vector/scalar store into one;
	//	read loop survived: a load from a slice slot feeding a
	//	register (ADDQ 0(DX)(BX*8), CX).
	writeRMW := regexp.MustCompile(`(?i)\b(ADD|SUB|XOR|INC|DEC)[A-Z0-9]*\s+(?:0[x0-9A-F]*)?\([A-Z0-9]+\)\([A-Z0-9]+\*\d+\)`)
	writeSt := regexp.MustCompile(`(?i)\bMOV[A-Z0-9]*\s+[\w$]+,\s*(?:0[x0-9A-F]*)?\([A-Z0-9]+\)\([A-Z0-9]+\*\d+\)`)
	readLd := regexp.MustCompile(`(?i)\b(MOV|ADD|SUB|XOR|CMP)[A-Z0-9]*\s+(?:0[x0-9A-F]*)?\([A-Z0-9]+\)\([A-Z0-9]+\*\d+\),\s*\w`)
	writes, reads := 0, 0
	for _, line := range strings.Split(text, "\n") {
		if writeRMW.MatchString(line) || writeSt.MatchString(line) {
			writes++
		}
		if readLd.MatchString(line) {
			reads++
		}
	}
	if writes == 0 {
		t.Errorf("no slice-slot stores in the emitted runMemBW: the write loop was optimized away:\n%s", head(text))
	}
	if reads == 0 {
		t.Errorf("no slice-slot loads in the emitted runMemBW: the strided read was optimized away:\n%s", head(text))
	}
	t.Logf("machine code check: %d slice-store lines, %d slice-load lines", writes, reads)
}

func head(s string) string {
	return strings.Join(strings.SplitN(strings.TrimSpace(s), "\n", 40), "\n")
}
