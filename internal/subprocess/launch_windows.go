//go:build windows

package subprocess

import (
	"os/exec"
	"syscall"
)

// applyInheritedHandle passes one already-inheritable handle to the
// child through SysProcAttr.AdditionalInheritedHandles (Go maps it to
// PROC_THREAD_ATTRIBUTE_HANDLE_LIST). Cmd.ExtraFiles is NOT supported
// on Windows and must never be used for this - the P07e1 note, now
// wired. The handle's numeric value is identical in the child, which
// is why the coordinator also passes it as a decimal argv flag.
func applyInheritedHandle(cmd *exec.Cmd, handle uintptr) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.AdditionalInheritedHandles = append(
		cmd.SysProcAttr.AdditionalInheritedHandles, syscall.Handle(handle))
}
