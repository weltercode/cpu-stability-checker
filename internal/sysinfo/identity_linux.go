package sysinfo

import (
	"os"
	"strings"
)

// Linux identity: the first "model name" from /proc/cpuinfo and the
// kernel release. Missing files simply leave the field unknown.
func machineIdentity() MachineIdentity {
	var id MachineIdentity
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if name, rest, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(name) == "model name" {
				id.CPUName = strings.TrimSpace(rest)
				break
			}
		}
	}
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		id.OSVersion = strings.TrimSpace(string(data))
	}
	return id
}
