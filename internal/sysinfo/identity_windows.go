package sysinfo

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

// Windows identity: the processor brand string from the registry (the
// same value msinfo32 shows; CPUID brand parsing would need asm, and
// the registry is what the OS itself publishes) and the true NT version
// from RtlGetVersion - GetVersionEx lies to unsigned binaries, so the
// ntdll route is the honest one.

var (
	modAdvapi32Id   = syscall.NewLazyDLL("advapi32.dll")
	procRegGetValue = modAdvapi32Id.NewProc("RegGetValueW")
	modNtDllId      = syscall.NewLazyDLL("ntdll.dll")
	procRtlGetVer   = modNtDllId.NewProc("RtlGetVersion")
)

const (
	hklm                 = uintptr(0x80000002) // HKEY_LOCAL_MACHINE
	rrfRTRegSZ           = 0x00000002          // return only REG_SZ
	rrfFlagNoExpand      = 0x00000100
	errorSuccess         = 0
	osVersionInfoExWSize = 284
)

func machineIdentity() MachineIdentity {
	var id MachineIdentity
	id.CPUName = registryString(`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, "ProcessorNameString")
	id.OSVersion = ntVersion()
	return id
}

// registryString reads one REG_SZ value. Any failure yields "": the
// identity is unknown, and the caller must not paint a zero for it.
func registryString(subKey, value string) string {
	sub, err := syscall.UTF16PtrFromString(subKey)
	if err != nil {
		return ""
	}
	val, err := syscall.UTF16PtrFromString(value)
	if err != nil {
		return ""
	}
	var buf [512]uint16
	size := uint32(len(buf) * 2)
	r, _, _ := procRegGetValue.Call(hklm, uintptr(unsafe.Pointer(sub)), uintptr(unsafe.Pointer(val)),
		uintptr(rrfRTRegSZ|rrfFlagNoExpand), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r != errorSuccess {
		return ""
	}
	// The registry pads brand strings to a fixed width; trailing
	// spaces are not part of the identity.
	return strings.TrimSpace(syscall.UTF16ToString(buf[:]))
}

// ntVersion asks ntdll for the real version: "Windows NT <major>.<minor>
// build <build>". The marketing name (10/11) is not what the API
// reports, so it is not claimed.
func ntVersion() string {
	type osVersionInfoExW struct {
		size             uint32
		major, minor     uint32
		build            uint32
		platform         uint32
		csd              [128]uint16
		spMajor, spMinor uint16
		suite            uint16
		productType      uint8
		reserved         uint8
	}
	v := osVersionInfoExW{size: osVersionInfoExWSize}
	r, _, _ := procRtlGetVer.Call(uintptr(unsafe.Pointer(&v)))
	if r != 0 {
		return ""
	}
	return fmt.Sprintf("Windows NT %d.%d build %d", v.major, v.minor, v.build)
}
