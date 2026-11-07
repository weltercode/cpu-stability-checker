package sysinfo

import (
	"strconv"
	"strings"
)

// CoreTime is the OS's accounting for one logical processor: how long it
// spent idle, in kernel code and in user code, in nanoseconds since boot.
// Windows measures these in ~15.6ms quanta, so short windows quantize —
// the numbers describe system busyness of the logical CPU, not a promise
// about C-states (ROADMAP level-2 honesty).
type CoreTime struct {
	IdleNS   uint64
	KernelNS uint64 // includes IdleNS
	UserNS   uint64
}

// IdlePct is the share of the measured window the logical CPU spent idle:
// idle over kernel+user, because kernel time already contains idle.
// ok=false when the window carries no accounting at all.
func (prev CoreTime) IdlePct(now CoreTime) (float64, bool) {
	di := int64(now.IdleNS) - int64(prev.IdleNS)
	dk := int64(now.KernelNS) - int64(prev.KernelNS)
	du := int64(now.UserNS) - int64(prev.UserNS)
	denom := dk + du
	if denom <= 0 {
		return 0, false
	}
	pct := float64(di) * 100 / float64(denom)
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

// CoreTimes returns one entry per logical processor, indexed by logical
// CPU number, or an error where the platform has no such accounting.
func CoreTimes() ([]CoreTime, error) { return coreTimes() }

// ThreadCycleTimeSelf reports the CPU cycles consumed by the calling OS
// thread so far. Sampled around a workload on a pinned thread, the delta
// over wall time is that core's effective clock (it also counts any
// bookkeeping the thread itself does, and preemption lowers it — it is an
// observed cycle rate, not a hardware frequency readout).
func ThreadCycleTimeSelf() (uint64, error) { return threadCycleTimeSelf() }

// ProcessCycleTimeSelf reports the cycles consumed by every thread of the
// current process. For a process pinned to one core (the per-process mode
// worker), that is the core's observed cycle rate.
func ProcessCycleTimeSelf() (uint64, error) { return processCycleTimeSelf() }

// CurrentThreadID reports the OS thread id of the calling goroutine's
// locked thread (0 where the platform cannot say). A pinned worker
// publishes it so the watchdog can observe that thread's cycle rate.
func CurrentThreadID() int { return currentThreadID() }

// ThreadCycleTime asks the OS for the cycles consumed by the given thread
// id of this process. Like ThreadCycleTimeSelf it is an observed cycle
// rate, not a hardware frequency readout.
func ThreadCycleTime(tid int) (uint64, error) { return threadCycleTime(tid) }

// ProcessCycleCounter reads another process's cycle count through a handle
// the caller owns. Reopening by PID every sample could race a recycled PID
// (a dead child's number handed to an unrelated process), so the parent
// opens the handle while it still owns the child and keeps it until the
// child is reaped — the identity is bound to the handle, not the number.
type ProcessCycleCounter struct {
	handle uintptr // platform handle, 0 once closed or never opened
}

// HandleCount reports how many OS handles this process currently holds
// (0, false where the platform cannot say). Spawned children and their
// job objects leave handles behind if we ever forget to close them, so a
// long-running soak watches this number for sustained growth.
func HandleCount() (int, bool) { return handleCount() }

// NewProcessCycleCounter opens the counter for a process id. The error
// explains why the platform or the OS refuses.
func NewProcessCycleCounter(pid int) (*ProcessCycleCounter, error) {
	return newProcessCycleCounter(pid)
}

// Read returns the total cycles the process has consumed so far.
func (c *ProcessCycleCounter) Read() (uint64, error) { return c.read() }

// Close releases the handle; closing twice or closing nil is safe.
func (c *ProcessCycleCounter) Close() { c.close() }

// parseProcessorPerformance decodes SYSTEM_PROCESSOR_PERFORMANCE_INFORMATION
// arrays (x64 layout: five int64 times, one uint32 count, padding — 48
// bytes per logical CPU). Times arrive in 100ns units and are converted
// to nanoseconds here. Trailing bytes that do not form a whole entry are
// ignored.
func parseProcessorPerformance(buf []byte) []CoreTime {
	const entry = 48
	n := len(buf) / entry
	out := make([]CoreTime, 0, n)
	for i := 0; i < n; i++ {
		b := buf[i*entry:]
		idle := le64(b[0:])
		kernel := le64(b[8:])
		user := le64(b[16:])
		out = append(out, CoreTime{
			IdleNS:   idle * 100,
			KernelNS: kernel * 100,
			UserNS:   user * 100,
		})
	}
	return out
}

func le64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

// parseProcStatCPUs decodes the per-CPU "cpuN ..." lines of Linux
// /proc/stat. Fields are user nice system idle iowait irq softirq steal
// ... in USER_HZ ticks (10ms). To keep IdlePct's Windows contract on both
// platforms, kernel time is made inclusive of idle: kernel = system +
// irq + softirq + idle, user = user + nice. Lines are taken in order;
// the aggregate "cpu " line and anything unparsable are ignored.
func parseProcStatCPUs(text string) []CoreTime {
	var out []CoreTime
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "cpu") || strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		nums := make([]uint64, 0, 8)
		bad := false
		for _, f := range fields[1:] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				bad = true
				break
			}
			nums = append(nums, v)
		}
		if bad {
			continue
		}
		idle := nums[3]
		user := nums[0] + nums[1]
		kernel := nums[2] + idle
		if len(nums) > 5 {
			kernel += nums[5] + nums[6] // irq + softirq
		}
		if len(nums) > 7 {
			user += nums[7] // steal counts against the user's view of the CPU
		}
		out = append(out, CoreTime{
			IdleNS:   idle * 10_000_000,
			UserNS:   user * 10_000_000,
			KernelNS: kernel * 10_000_000,
		})
	}
	return out
}
