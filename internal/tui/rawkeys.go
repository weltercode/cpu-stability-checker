package tui

// R42 (owner-named 20.11): arrow-key navigation of the menus. The
// keys are read as real console KEY_EVENTS (not line text): line
// based arrow interpretation cannot do immediate navigation, and
// cooked escape filtering is unreliable. While a navigating menu
// waits, ONE owner pulls the console input events a record at a
// time (RawKeys.NextKey); nothing else reads stdin at that moment,
// and the owner stops pulling the instant its answer is complete, so
// typeahead beyond the answer keeps its exact cooked-mode meaning for
// the next prompt. Enter and typed characters are reassembled into
// exactly the string the old bufio ReadString would have returned, so
// every downstream flow - consent screens included - and the scripted
// tests keep their exact current semantics. Where no console input
// handle exists (pipes, files, non-Windows), StartRawKeys returns
// nil and callers keep the byte-identical buffered path.

// Key is one decoded console key press.
type Key int

const (
	KeyNone      Key = iota // record not a key press, or a key-UP: ignore
	KeyUp                   // arrow up
	KeyDown                 // arrow down
	KeyLeft                 // arrow left
	KeyRight                // arrow right
	KeyEnter                // Enter/Return
	KeyEsc                  // Escape
	KeyBackspace            // Backspace
	KeyRune                 // a printable character (rune carries it)
	KeyInterrupt            // Ctrl+C arriving as an event (0x03)
	KeyLost                 // the input handle died, or console EOF (Ctrl+Z): the line reader would see exhaustion
	KeyOther                // a key press with no text meaning (F-keys, modifiers...)
)
