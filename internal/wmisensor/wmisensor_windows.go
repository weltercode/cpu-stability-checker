//go:build windows

package wmisensor

import (
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	ole32WM          = syscall.NewLazyDLL("ole32.dll")
	oleaut32WM       = syscall.NewLazyDLL("oleaut32.dll")
	procCoInitWM     = ole32WM.NewProc("CoInitializeEx")
	procCoUninitWM   = ole32WM.NewProc("CoUninitialize")
	procCoCreateWM   = ole32WM.NewProc("CoCreateInstance")
	procCoBlanketWM  = ole32WM.NewProc("CoSetProxyBlanket")
	procCoSecWM      = ole32WM.NewProc("CoInitializeSecurity")
	procVariantClrWM = oleaut32WM.NewProc("VariantClear")
	procBstrAllocWM  = oleaut32WM.NewProc("SysAllocString")
	procBstrFreeWM   = oleaut32WM.NewProc("SysFreeString")
)

// WMI identity (SDK constants, wbemcli.idl). Both GUIDs end in
// 2e24; the class starts 4590f811, the interface dc12a687. Mixing
// them up gives REGDB_E_CLASSNOTREG or E_NOINTERFACE.
var (
	clsidWbemLocator = guid{0x4590F811, 0x1D3A, 0x11D0, [8]byte{0x89, 0x1F, 0x00, 0xAA, 0x00, 0x4B, 0x2E, 0x24}}
	iidIWbemLocator  = guid{0xDC12A687, 0x737F, 0x11CF, [8]byte{0x88, 0x4D, 0x00, 0xAA, 0x00, 0x4B, 0x2E, 0x24}}
)

// namespaces are the two monitors' WMI namespaces, in precedence
// order. Only the first that connects is ever queried - never both.
var namespaces = []string{`root/LibreHardwareMonitor`, `root/OpenHardwareMonitor`}

type guid struct {
	d1 uint32
	d2 uint16
	d3 uint16
	d4 [8]byte
}

// comObj is a COM interface pointer. Calls go through the vtable at
// the dereferenced address; the slots below follow wbemidl.h
// declaration order after IUnknown (QueryInterface=0, AddRef=1,
// Release=2).
type comObj struct{ p unsafe.Pointer }

const (
	slotRelease       = 2
	slotConnectServer = 3  // IWbemLocator's only method
	slotExecQuery     = 20 // IWbemServices
	slotEnumNext      = 4  // IEnumWbemClassObject
	slotClassGet      = 4  // IWbemClassObject, right after GetQualifierSet

	wbemReturnNow   = 0x00000010
	wbemForwardOnly = 0x00000020

	// Success codes from wbemcli.h, not the 0x1xx range I first
	// guessed: WBEM_S_FALSE (fewer objects than asked) is 0x1.
	wbemSFalse      = 0x00000001
	wbemSTimedOut   = 0x00040004
	wbemSNoMoreData = 0x00040005
)

func (c *comObj) call(slot int, args ...uintptr) uintptr {
	vtbl := *(**[64]uintptr)(c.p)
	r, _, _ := syscall.SyscallN(vtbl[slot], append([]uintptr{uintptr(c.p)}, args...)...)
	return r
}

func (c *comObj) release() {
	if c.p != nil {
		c.call(slotRelease)
		c.p = nil
	}
}

// variant mirrors the amd64 VARIANT: 8 bytes of type/reserved, then
// the 8-byte value union.
type variant struct {
	vt  uint16
	r1  uint16
	r2  uint16
	r3  uint16
	val uint64
}

const (
	vtEmpty = 0
	vtNull  = 1
	vtI2    = 2
	vtI4    = 3
	vtR4    = 4
	vtR8    = 5
	vtBSTR  = 8
)

// variantFloat extracts a finite number from a VARIANT, whatever
// numeric flavor the provider used.
func variantFloat(v variant) (float64, bool) {
	switch v.vt {
	case vtR4:
		f := float64(math.Float32frombits(uint32(v.val)))
		return f, !math.IsNaN(f) && !math.IsInf(f, 0)
	case vtR8:
		f := math.Float64frombits(v.val)
		return f, !math.IsNaN(f) && !math.IsInf(f, 0)
	case vtI4:
		return float64(int32(v.val)), true
	case vtI2:
		return float64(int16(v.val)), true
	}
	return 0, false
}

// bstrString decodes a BSTR (length-prefixed UTF-16). The character
// pointer lives in the VARIANT union; reading it through the field's
// own address keeps the pointer out of uintptr arithmetic entirely.
func bstrString(v *variant) string {
	if v.vt != vtBSTR {
		return ""
	}
	p := *(*unsafe.Pointer)(unsafe.Pointer(&v.val))
	if p == nil {
		return ""
	}
	cb := *(*int32)(unsafe.Add(p, -4)) // byte length prefix
	if cb <= 0 {
		return ""
	}
	units := unsafe.Slice((*uint16)(p), int(cb)/2)
	return string(utf16.Decode(units))
}

// Source is an open WMI connection to one monitor, refreshed by a
// dedicated COM worker goroutine. Readings() never blocks on COM: it
// returns the latest cached snapshot.
type Source struct {
	mu        sync.Mutex
	cache     []Reading
	updatedAt time.Time
	provider  string
	worker    *wmiWorker
	stopOnce  sync.Once
}

type wmiWorker struct {
	ready chan error
	stop  chan struct{}
	done  chan struct{}
}

// Open connects to the first available monitor namespace. The error
// explains the real causes: no monitor running, or its WMI provider
// option is off.
func Open() (*Source, error) {
	w := &wmiWorker{ready: make(chan error, 1), stop: make(chan struct{}), done: make(chan struct{})}
	s := &Source{worker: w}
	go s.run(w)
	if err := <-w.ready; err != nil {
		<-w.done
		return nil, err
	}
	return s, nil
}

// run owns the COM apartment: every COM call happens on this one
// locked OS thread, from CoInitializeEx to CoUninitialize.
func (s *Source) run(w *wmiWorker) {
	defer close(w.done)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// COINIT_MULTITHREADED is 0 (not 1 - that is SPEED_UP_MESSAGES
	// and fails E_INVALIDARG on its own). S_FALSE (1) = already init.
	if r, _, _ := procCoInitWM.Call(0, 0); r != 0 && r != 1 {
		w.ready <- fmt.Errorf("CoInitializeEx: 0x%x", r)
		return
	}
	defer procCoUninitWM.Call()
	// Process-wide security init may already exist (RPC_E_TOO_LATE);
	// both outcomes are fine for a local read-only client.
	// (pSecDesc, cAuthSvc, asAuthSvc, pReserved1, dwAuthnLevel,
	// dwImpLevel, pAuthList, dwCapabilities, pReserved3) - authn
	// before imp, swapping them returns E_INVALIDARG.
	procCoSecWM.Call(0, ^uintptr(0), 0, 0, 0 /*AUTHN_DEFAULT*/, 3 /*IMP_PERSONATE*/, 0, 0, 0)

	var locator comObj
	r, _, _ := procCoCreateWM.Call(
		uintptr(unsafe.Pointer(&clsidWbemLocator)), 0, 0x1, /*CLSCTX_INPROC_SERVER*/
		uintptr(unsafe.Pointer(&iidIWbemLocator)), uintptr(unsafe.Pointer(&locator.p)))
	if r != 0 {
		w.ready <- fmt.Errorf("CoCreateInstance(WbemLocator): 0x%x", r)
		return
	}
	defer locator.release()

	var services comObj
	var lastErr error
	for _, ns := range namespaces {
		nsB := bstr(ns)
		// (networkResource, user, password, locale, flags, authority,
		// pCtx, ppServices) - the out pointer is the LAST parameter;
		// putting it in the pCtx slot gives WBEM_E_INVALID_PARAMETER.
		r := locator.call(slotConnectServer,
			uintptr(nsB), 0, 0, 0, 0, 0, 0,
			uintptr(unsafe.Pointer(&services.p)))
		bstrFree(nsB)
		if r == 0 && services.p != nil {
			s.provider = strings.TrimPrefix(ns, "root/")
			break
		}
		services.p = nil
		lastErr = fmt.Errorf("ConnectServer(%s): 0x%x", ns, r)
	}
	if services.p == nil {
		w.ready <- fmt.Errorf("no LibreHardwareMonitor/OpenHardwareMonitor WMI provider: %w (running with the WMI option enabled?)", lastErr)
		return
	}
	defer services.release()
	// The local client must impersonate to reach the provider.
	procCoBlanketWM.Call(uintptr(services.p), 10 /*WINNT*/, 0, 0, 0, 3 /*IMPERSONATE*/, 0, 0)

	w.ready <- nil

	refresh := func() {
		reads, err := querySensors(services)
		if err != nil {
			return // keep the last good snapshot; the monitor may be restarting
		}
		s.mu.Lock()
		s.cache = reads
		s.updatedAt = time.Now()
		s.mu.Unlock()
	}
	refresh()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-tick.C:
			refresh()
		}
	}
}

// querySensors runs one WQL pass and decodes the Sensor objects.
func querySensors(services comObj) ([]Reading, error) {
	lang := bstr("WQL")
	q := bstr("SELECT SensorType, Name, Value FROM Sensor")
	var enum comObj
	r := services.call(slotExecQuery, uintptr(lang), uintptr(q),
		uintptr(wbemReturnNow|wbemForwardOnly), 0, uintptr(unsafe.Pointer(&enum.p)))
	bstrFree(lang)
	bstrFree(q)
	if r != 0 {
		return nil, fmt.Errorf("ExecQuery: 0x%x", r)
	}
	defer enum.release()

	var out []Reading
	for len(out) < 4096 { // bounded: a provider stuck in Next cannot spin forever
		var obj comObj
		var got uint32
		r := enum.call(slotEnumNext, 4000 /*ms*/, 1,
			uintptr(unsafe.Pointer(&obj.p)), uintptr(unsafe.Pointer(&got)))
		switch {
		case r == wbemSNoMoreData:
			return out, nil
		case r == wbemSTimedOut:
			return nil, fmt.Errorf("WMI sensor enumeration timed out")
		case r == 0 || r == wbemSFalse:
			// WBEM_S_FALSE with nothing returned is the exhausted
			// signal for a forward-only one-at-a-time walk.
			if got == 0 || obj.p == nil {
				return out, nil
			}
		default:
			return out, fmt.Errorf("Next: 0x%x", r)
		}
		rd, ok := decodeSensor(obj)
		obj.release()
		if ok {
			out = append(out, rd)
		}
	}
	return out, nil
}

// decodeSensor pulls SensorType, Name and Value out of one Sensor
// object. Anything malformed is skipped, never guessed.
func decodeSensor(obj comObj) (Reading, bool) {
	var rd Reading
	get := func(prop string, v *variant) bool {
		p, err := syscall.UTF16PtrFromString(prop)
		if err != nil {
			return false
		}
		r := obj.call(slotClassGet, uintptr(unsafe.Pointer(p)), 0,
			uintptr(unsafe.Pointer(v)), 0, 0)
		return r == 0
	}
	var vType, vName, vVal variant
	valueOK := false
	if get("SensorType", &vType) {
		rd.Type = bstrString(&vType)
	}
	if get("Name", &vName) {
		rd.Name = bstrString(&vName)
	}
	if get("Value", &vVal) {
		if f, ok := variantFloat(vVal); ok {
			rd.Value = f
			valueOK = true
		}
	}
	procVariantClrWM.Call(uintptr(unsafe.Pointer(&vType)))
	procVariantClrWM.Call(uintptr(unsafe.Pointer(&vName)))
	procVariantClrWM.Call(uintptr(unsafe.Pointer(&vVal)))
	if rd.Type == "" || rd.Name == "" || !valueOK {
		return Reading{}, false
	}
	return rd, true
}

// Readings returns the latest cached snapshot (never blocks on COM).
func (s *Source) Readings() []Reading {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Reading(nil), s.cache...)
}

// Snapshot keeps the last successful query's time. Failed refreshes must
// not make cached values look new to the temperature cutoff.
func (s *Source) Snapshot() ([]Reading, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Reading(nil), s.cache...), s.updatedAt
}

func (s *Source) Provider() string { return s.provider }

// Close stops the worker and tears down the apartment.
func (s *Source) Close() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.worker.stop) })
	<-s.worker.done
}

// bstr allocates a BSTR from a Go string (UTF-16, length-prefixed).
func bstr(s string) unsafe.Pointer {
	u, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return nil
	}
	p, _, _ := procBstrAllocWM.Call(uintptr(unsafe.Pointer(u)))
	// Read the returned pointer bits through the variable's own
	// address: no uintptr->Pointer conversion for vet to flag.
	return *(*unsafe.Pointer)(unsafe.Pointer(&p))
}

func bstrFree(p unsafe.Pointer) {
	if p != nil {
		procBstrFreeWM.Call(uintptr(p))
	}
}
