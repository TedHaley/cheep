package tui

import (
	"bytes"
	"os"
)

// Terminal keyboard-protocol plumbing.
//
// Terminals send the same bytes for Enter and Shift+Enter by default, so no
// program — Bubble Tea, Claude Code, or anything else — can tell them apart
// until the terminal is told to report the difference. Claude Code does this by
// enabling the kitty keyboard protocol on startup; we do the same here.
//
// kbEnable pushes the kitty "disambiguate escape codes" level, which makes the
// terminal report keys that have no legacy encoding (Shift+Enter, Ctrl+Enter)
// as CSI-u sequences. That level also re-encodes a handful of keys that DID
// have legacy encodings (Esc, Shift+Tab, Alt+Tab). Bubble Tea v1 doesn't speak
// the kitty protocol — its key parser only knows a static table of legacy
// sequences — so translatingReader rewrites the CSI-u sequences we care about
// back into the legacy forms v1 already understands (verified against
// bubbletea@v1.3.10's key.go / key_sequences.go).
const (
	kbEnable  = "\x1b[>1u" // push kitty flags: disambiguate escape codes
	kbDisable = "\x1b[<u"  // pop kitty flags (restore on exit)
)

type kbTranslation struct{ from, to []byte }

// kbTranslations maps the CSI-u sequences the kitty protocol emits to the
// legacy sequences Bubble Tea v1's parser recognizes. The keycodes are: 13 =
// Enter, 9 = Tab, 27 = Esc, 106 = 'j', 127 = Backspace; the modifier is
// (mods+1), so ;2 = Shift, ;3 = Alt/Option and ;5 = Ctrl. No `from` here is a
// prefix of another, so match order is irrelevant.
var kbTranslations = []kbTranslation{
	{[]byte("\x1b[13;2u"), []byte("\x1b\r")},    // shift+enter    -> alt+enter (InsertNewline)
	{[]byte("\x1b[13;5u"), []byte("\x1b\r")},    // ctrl+enter     -> alt+enter (InsertNewline)
	{[]byte("\x1b[106;5u"), []byte("\x1b\r")},   // ctrl+j         -> alt+enter (InsertNewline)
	{[]byte("\x1b[27u"), []byte("\x1b")},        // esc            -> esc (restored)
	{[]byte("\x1b[9;2u"), []byte("\x1b[Z")},     // shift+tab      -> shift+tab (restored)
	{[]byte("\x1b[9;3u"), []byte("\x1b\t")},     // alt+tab        -> alt+tab (restored)
	{[]byte("\x1b[127;3u"), []byte("\x1b\x7f")}, // option+delete -> alt+backspace (delete word)
}

// translatingReader wraps the controlling terminal and rewrites kitty-protocol
// key sequences into the legacy sequences Bubble Tea v1 understands.
//
// It embeds *os.File so it still satisfies term.File and cancelreader.File:
// Bubble Tea needs the former to enter raw mode (tty_unix.go) and the latter to
// build a cancelable reader. Because the default input's Name() is /dev/stdin
// (not /dev/tty), cancelreader takes its kqueue path and calls Read on the
// embedded file — i.e. this override — so translation actually runs.
type translatingReader struct {
	*os.File
	pending []byte // already-translated output not yet delivered to the caller
	held    []byte // raw trailing bytes that may be the start of a sequence we translate
	buf     []byte // read scratch, reused across calls
}

func newTranslatingReader(f *os.File) *translatingReader {
	return &translatingReader{File: f, buf: make([]byte, 256)}
}

func (t *translatingReader) Read(p []byte) (int, error) {
	// Drain any translated bytes left over from a previous read first.
	if len(t.pending) > 0 {
		n := copy(p, t.pending)
		t.pending = t.pending[n:]
		return n, nil
	}

	n, err := t.File.Read(t.buf)
	if n == 0 {
		return 0, err
	}

	// Prepend any partial sequence held back last time, then translate.
	data := append(append([]byte(nil), t.held...), t.buf[:n]...)
	emit, held := translateKeys(data)
	t.held = held

	if len(emit) == 0 {
		// The whole read was an incomplete sequence prefix; hold it and let the
		// caller read again. Returning (0, nil) makes Bubble Tea's read loop
		// wait for the next byte via the cancelable reader (no busy spin).
		return 0, err
	}

	m := copy(p, emit)
	if m < len(emit) {
		t.pending = append([]byte(nil), emit[m:]...)
	}
	return m, err
}

// translateKeys rewrites kitty-protocol sequences in data. It returns the bytes
// ready to emit and, if data ends midway through a sequence we might translate,
// the incomplete tail to prepend to the next read. Any escape sequence we don't
// recognize passes through untouched for Bubble Tea's own parser to handle.
func translateKeys(data []byte) (emit, held []byte) {
	var out []byte
	for i := 0; i < len(data); {
		if data[i] != 0x1b {
			out = append(out, data[i])
			i++
			continue
		}
		rest := data[i:]
		if tr := matchTranslation(rest); tr != nil {
			out = append(out, tr.to...)
			i += len(tr.from)
			continue
		}
		if isSeqPrefix(rest) {
			// Incomplete sequence at the end of the buffer — hold the tail.
			return out, append([]byte(nil), rest...)
		}
		out = append(out, data[i]) // an escape sequence that isn't ours
		i++
	}
	return out, nil
}

func matchTranslation(b []byte) *kbTranslation {
	for i := range kbTranslations {
		if bytes.HasPrefix(b, kbTranslations[i].from) {
			return &kbTranslations[i]
		}
	}
	return nil
}

// isSeqPrefix reports whether b is a proper prefix of a sequence we translate,
// meaning more bytes are needed before we can decide.
func isSeqPrefix(b []byte) bool {
	for i := range kbTranslations {
		f := kbTranslations[i].from
		if len(b) < len(f) && bytes.HasPrefix(f, b) {
			return true
		}
	}
	return false
}
