//go:build windows

package wmisensor

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

func TestVariantFloat(t *testing.T) {
	cases := []struct {
		name string
		v    variant
		want float64
		ok   bool
	}{
		{"r8", variant{vt: vtR8, val: math.Float64bits(118.5)}, 118.5, true},
		{"r4", variant{vt: vtR4, val: uint64(math.Float32bits(72.5))}, 72.5, true},
		{"i4", variant{vt: vtI4, val: uint64(uint32(0xFFFFFFFB))}, -5, true}, // int32(-5)
		{"i2", variant{vt: vtI2, val: uint64(int16(3))}, 3, true},
		{"nan r8", variant{vt: vtR8, val: math.Float64bits(math.NaN())}, 0, false},
		{"empty", variant{vt: vtEmpty}, 0, false},
		{"bstr", variant{vt: vtBSTR}, 0, false},
	}
	for _, tt := range cases {
		got, ok := variantFloat(tt.v)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("%s: got %v,%v want %v,%v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

// The BSTR decode (length prefix at -4 bytes) is verified against a
// real SysAllocString allocation, not a hand-built buffer.
func TestBstrString(t *testing.T) {
	p := bstr("CPU (Tctl/Tdie)")
	if p == nil {
		t.Fatal("SysAllocString failed")
	}
	defer bstrFree(p)
	var v variant
	v.vt = vtBSTR
	*(*unsafe.Pointer)(unsafe.Pointer(&v.val)) = p
	if got := bstrString(&v); got != "CPU (Tctl/Tdie)" {
		t.Errorf("bstrString = %q", got)
	}
}

// requireWMIService is the capability probe audit A07 asks for: it asks
// the service control manager (plain advapi32, no COM) whether the
// Winmgmt WMI service is running. An absent or stopped service is an
// environment fact and must skip naming the capability; a RUNNING
// service plus a failing chain is our plumbing breaking and must stay
// a hard failure. That keeps the regression net intact while a WMI-less
// machine still reports honestly.
func requireWMIService(t *testing.T) {
	t.Helper()
	advapi := syscall.NewLazyDLL("advapi32.dll")
	openSCM := advapi.NewProc("OpenSCManagerW")
	openSvc := advapi.NewProc("OpenServiceW")
	querySvc := advapi.NewProc("QueryServiceStatus")
	closeSvc := advapi.NewProc("CloseServiceHandle")

	name, err := syscall.UTF16PtrFromString("Winmgmt")
	if err != nil {
		t.Fatalf("UTF16(Winmgmt): %v", err)
	}
	const scManagerConnect = 0x0001
	const serviceQueryStatus = 0x0004
	scm, _, errno := openSCM.Call(0, 0, scManagerConnect)
	if scm == 0 {
		t.Skipf("test level 2 (Windows integration): service control manager unreachable (OpenSCManager): %v", errno)
	}
	defer closeSvc.Call(scm)
	svc, _, errno := openSvc.Call(scm, uintptr(unsafe.Pointer(name)), serviceQueryStatus)
	if svc == 0 {
		if errno == syscall.Errno(1060) { // ERROR_SERVICE_DOES_NOT_EXIST
			t.Skip("test level 2 (Windows integration): the Winmgmt WMI service does not exist on this machine")
		}
		t.Skipf("test level 2 (Windows integration): cannot open the Winmgmt service: %v", errno)
	}
	defer closeSvc.Call(svc)
	var st struct {
		serviceType, currentState, controlsAccepted   uint32
		win32Exit, specificExit, checkPoint, waitHint uint32
	}
	if r, _, errno := querySvc.Call(svc, uintptr(unsafe.Pointer(&st))); r == 0 {
		t.Skipf("test level 2 (Windows integration): cannot query Winmgmt status: %v", errno)
	}
	const serviceRunning = 4
	if st.currentState != serviceRunning {
		t.Skipf("test level 2 (Windows integration): Winmgmt state = %d, want running (4)", st.currentState)
	}
}

// The raw COM chain (locator -> ConnectServer -> ExecQuery -> Next ->
// Get) is verified against root\cimv2 using this package's own ABI
// helpers, so the plumbing is proven even when no monitor is running.
// It doubles as a regression net for the vtable slots and GUIDs.
func TestWMIRawChain(t *testing.T) {
	// Level marking is by BOTH capability and level: the chain needs
	// the live WMI service, so -short keeps it out even on a machine
	// where it would otherwise pass (that is what makes the short gate
	// hermetic on every machine, not only on machines without WMI).
	if testing.Short() {
		t.Skip("test level 2 (Windows integration): raw WMI service chain is excluded by -short")
	}
	requireWMIService(t)
	type result struct {
		name string
		max  float64
		err  error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if r, _, _ := procCoInitWM.Call(0, 0); r != 0 && r != 1 {
			done <- result{err: fmt.Errorf("CoInitializeEx: 0x%x", r)}
			return
		}
		defer procCoUninitWM.Call()
		procCoSecWM.Call(0, ^uintptr(0), 0, 0, 0, 3, 0, 0, 0)

		var locator comObj
		if r, _, _ := procCoCreateWM.Call(
			uintptr(unsafe.Pointer(&clsidWbemLocator)), 0, 0x1,
			uintptr(unsafe.Pointer(&iidIWbemLocator)), uintptr(unsafe.Pointer(&locator.p))); r != 0 {
			done <- result{err: fmt.Errorf("CoCreateInstance: 0x%x", r)}
			return
		}
		defer locator.release()

		nsB := bstr(`root\cimv2`)
		var services comObj
		r := locator.call(slotConnectServer, uintptr(nsB), 0, 0, 0, 0, 0, 0,
			uintptr(unsafe.Pointer(&services.p)))
		bstrFree(nsB)
		if r != 0 || services.p == nil {
			done <- result{err: fmt.Errorf("ConnectServer(cimv2): 0x%x", r)}
			return
		}
		defer services.release()

		lang := bstr("WQL")
		q := bstr("SELECT Name, MaxClockSpeed FROM Win32_Processor")
		var enum comObj
		r = services.call(slotExecQuery, uintptr(lang), uintptr(q),
			uintptr(wbemReturnNow|wbemForwardOnly), 0, uintptr(unsafe.Pointer(&enum.p)))
		bstrFree(lang)
		bstrFree(q)
		if r != 0 || enum.p == nil {
			done <- result{err: fmt.Errorf("ExecQuery: 0x%x", r)}
			return
		}
		defer enum.release()

		var obj comObj
		var got uint32
		r = enum.call(slotEnumNext, 4000, 1,
			uintptr(unsafe.Pointer(&obj.p)), uintptr(unsafe.Pointer(&got)))
		if obj.p == nil {
			done <- result{err: fmt.Errorf("Next: 0x%x got=%d", r, got)}
			return
		}
		defer obj.release()

		var res result
		pName, _ := syscall.UTF16PtrFromString("Name")
		pMax, _ := syscall.UTF16PtrFromString("MaxClockSpeed")
		var vName, vMax variant
		if obj.call(slotClassGet, uintptr(unsafe.Pointer(pName)), 0,
			uintptr(unsafe.Pointer(&vName)), 0, 0) == 0 {
			res.name = bstrString(&vName)
		}
		if obj.call(slotClassGet, uintptr(unsafe.Pointer(pMax)), 0,
			uintptr(unsafe.Pointer(&vMax)), 0, 0) == 0 {
			res.max, _ = variantFloat(vMax)
		}
		procVariantClrWM.Call(uintptr(unsafe.Pointer(&vName)))
		procVariantClrWM.Call(uintptr(unsafe.Pointer(&vMax)))
		done <- res
	}()
	res := <-done
	if res.err != nil {
		t.Fatalf("raw WMI chain: %v", res.err)
	}
	if res.name == "" || res.max <= 0 {
		t.Fatalf("decoded CPU row empty: %q %v", res.name, res.max)
	}
	t.Logf("WMI chain OK: %q @ %v MHz", res.name, res.max)
}

// Live check: when a monitor is running with its WMI provider on,
// CPU sensors must decode end to end. Otherwise skip - absence is a
// normal machine state, not a failure.
func TestWMISensorsLive(t *testing.T) {
	if testing.Short() {
		t.Skip("test level 3 (hardware acceptance): live sensor provider check is excluded by -short")
	}
	s, err := Open()
	if err != nil {
		t.Skipf("test level 3 (hardware acceptance): no live LHM/OHM WMI provider on this machine: %v", err)
	}
	defer s.Close()
	reads := s.Readings()
	if len(reads) == 0 {
		t.Fatal("provider connected but published no sensors")
	}
	var temps int
	for _, r := range reads {
		if r.Type == "Temperature" {
			temps++
		}
	}
	t.Logf("%d sensors, %d temperatures, clocks=%v", len(reads), temps, CoreClocks(reads))
	if _, ok := Find(reads, "Temperature", "tctl", "tdie", "cpu package"); !ok {
		t.Errorf("no CPU temperature among %d sensors", len(reads))
	}
}

// TestRunFlagSelectsWMIGroup proves the documented TestWMI* name prefix
// partitions the WMI-dependent level (audit A07 mandatory: level marking
// distinguishable by command line) - in BOTH directions:
//   - a full run re-executes with -test.run=^TestWMI and must REACH the
//     pair (which then behaves by capability as usual);
//   - a short run re-executes with -test.short too and must SKIP the
//     pair with its level word, so -short keeps the WMI service
//     untouched even on a machine where the pair would pass.
//
// This guard test deliberately does NOT carry the TestWMI prefix - it
// must stay outside the group it verifies, or the re-exec would recurse
// forever.
func TestRunFlagSelectsWMIGroup(t *testing.T) {
	short := testing.Short()
	args := []string{"-test.run=^TestWMI", "-test.v", "-test.timeout=120s"}
	if short {
		args = append(args, "-test.short")
	}
	cmd := exec.Command(os.Args[0], args...)
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		t.Fatalf("WMI group must pass or skip by capability, got %v:\n%s", err, text)
	}
	if strings.Contains(text, "=== RUN   TestVariantFloat") {
		t.Fatalf("the level name pattern pulled unit tests into the run:\n%s", text)
	}
	if short {
		for _, want := range []string{
			"--- SKIP: TestWMIRawChain", "test level 2",
			"--- SKIP: TestWMISensorsLive", "test level 3",
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("under -short the WMI pair must skip naming its level, child output lacks %q:\n%s", want, text)
			}
		}
		return
	}
	for _, want := range []string{
		"=== RUN   TestWMIRawChain",
		"=== RUN   TestWMISensorsLive",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("run=^TestWMI child output lacks %q (err %v):\n%s", want, err, text)
		}
	}
}
