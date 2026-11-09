//go:build windows

package whea

import (
	"context"
	"time"
)

// shutdownFilter selects the two System-log records Windows writes on the
// boot AFTER an unexpected end of the previous one: EventLog 6008 says the
// previous shutdown was unexpected, Microsoft-Windows-Kernel-Power 41 says
// the system rebooted without cleanly shutting down first. The exact query
// text is pinned by TestWheaQueryTextIsWellFormed: a stray bracket here is
// the wevtutil exit-15001 lesson repeating itself.
//
// The witness deliberately reads only the machine-readable envelope
// (provider, event id, EventRecordID, TimeCreated = when that LATER boot
// wrote the record). The embedded EventData of these two events is never
// interpreted: 6008's time strings are locale-dependent prose, and 41's
// numeric fields (BugcheckCode and friends) are documented to mislead as
// causes; reading meaning into them here would be a diagnosis, and this
// tool states none.
const shutdownFilter = "((Provider[@Name='EventLog'] and EventID=6008) or " +
	"(Provider[@Name='Microsoft-Windows-Kernel-Power'] and EventID=41))"

// shutdowns returns the newest unexpected-shutdown records, newest first.
// It lives on the concrete querier, not the Monitor's interface: this is a
// one-shot read for the next-start witness, not monitored coverage, and it
// shares none of the WHEA severity machinery.
func (w wevtutilQuerier) shutdowns(ctx context.Context, limit int) ([]Event, error) {
	return w.run(ctx, shutdownFilter, limit, false)
}

// QueryShutdownWitness reads the OS's own account of an unexpected end of
// the previous boot: the newest unexpected-shutdown records, bounded. It
// shares the WHEA package's wevtutil transport and XML decoder and nothing
// else - these records name an end of a boot, not a hardware error, and
// they carry no severity claim. Reading the System log usually needs no
// elevation, but nothing is promised: a refusal comes back as the source's
// own named error, verbatim.
func QueryShutdownWitness(limit int) ([]Event, error) {
	q, ok := newQuerier()
	if !ok {
		return nil, errUnsupported
	}
	wq, ok := q.(wevtutilQuerier)
	if !ok {
		return nil, errUnsupported // newQuerier above only ever hands us this type
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return wq.shutdowns(ctx, limit)
}
