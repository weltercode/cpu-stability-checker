//go:build !windows

package tui

// EscapeWatcher exists as an inert type so callers compile
// everywhere; console input events are a Windows-console thing.
type EscapeWatcher struct{}

func WatchEscape(onEscape func()) *EscapeWatcher { return &EscapeWatcher{} }

func (w *EscapeWatcher) Stop() {}
