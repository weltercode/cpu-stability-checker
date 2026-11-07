//go:build !windows

package subprocess

import "os/exec"

// applyInheritedHandle has no native handle inheritance on this
// platform. It is unreachable in practice: the handoff coordinator
// fails at CreateSharedMapping (ErrUnsupported) long before any
// spawn, so no child is ever started without the sharing it needs.
// The no-op exists for compilation, never as a silent fallback.
func applyInheritedHandle(cmd *exec.Cmd, handle uintptr) {}
