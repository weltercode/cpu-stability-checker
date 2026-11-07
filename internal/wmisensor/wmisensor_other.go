//go:build !windows

package wmisensor

import (
	"errors"
	"time"
)

// Source exists as an inert type so callers compile everywhere; the
// WMI provider is a Windows thing.
type Source struct{}

func Open() (*Source, error) { return nil, errors.New("WMI sensors are Windows-only") }

func (s *Source) Readings() []Reading { return nil }

func (s *Source) Close() {}

func (s *Source) Snapshot() ([]Reading, time.Time) { return nil, time.Time{} }
func (s *Source) Provider() string                 { return "WMI" }
