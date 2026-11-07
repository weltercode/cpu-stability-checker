//go:build !windows

package procjob

import "os"

// Job is a no-op stub on platforms without Job Objects. Stopping a workload
// still kills the direct process (context cancellation); only the
// kill-the-whole-tree guarantee is missing, like affinity on these
// platforms. A POSIX setpgid/kill(-pgid) backend is future work.
type Job struct{}

func New() (*Job, error) { return &Job{}, nil }

func (j *Job) Attach(p *os.Process) error { return nil }

func (j *Job) Close() error { return nil }

// LiveJobs is empty on platforms without Job Objects; rotation there is a
// plain sequential run (the control channel simply has nothing to freeze).
func LiveJobs() []*Job { return nil }

// Suspend/Resume and process freezing are no-ops: the best-effort
// cross-platform answer is "the load keeps running", never a wrong freeze.
func (j *Job) Suspend() error { return nil }

func (j *Job) Resume() error { return nil }

func SuspendProcess(pid int) error { return nil }

func ResumeProcess(pid int) error { return nil }
