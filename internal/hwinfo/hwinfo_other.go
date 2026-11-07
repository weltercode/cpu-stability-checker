//go:build !windows

package hwinfo

import (
	"errors"
	"time"
)

// Source exists as an inert type so callers compile everywhere; the
// shared-memory interface is a Windows-HWiNFO thing.
type Source struct{}

func Open() (*Source, error) { return nil, errors.New("HWiNFO shared memory is Windows-only") }

func (s *Source) Close() {}

func (s *Source) Readings() []Reading { return nil }

func (s *Source) Snapshot() ([]Reading, time.Time) { return nil, time.Time{} }
