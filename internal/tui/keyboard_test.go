package tui

import (
	"bytes"
	"io"
	"testing"
)

func TestTranslateKeys(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantEmit string
		wantHeld string
	}{
		{"shift+enter -> alt+enter", "\x1b[13;2u", "\x1b\r", ""},
		{"ctrl+enter -> alt+enter", "\x1b[13;5u", "\x1b\r", ""},
		{"ctrl+j -> alt+enter", "\x1b[106;5u", "\x1b\r", ""},
		{"esc restored", "\x1b[27u", "\x1b", ""},
		{"shift+tab restored", "\x1b[9;2u", "\x1b[Z", ""},
		{"alt+tab restored", "\x1b[9;3u", "\x1b\t", ""},
		{"option+delete -> alt+backspace", "\x1b[127;3u", "\x1b\x7f", ""},
		{"plain text untouched", "hello", "hello", ""},
		{"plain enter untouched", "\r", "\r", ""},
		{"arrow key passes through", "\x1b[A", "\x1b[A", ""},
		{"ctrl+left passes through", "\x1b[1;5D", "\x1b[1;5D", ""},
		{"text then shift+enter", "hi\x1b[13;2u", "hi\x1b\r", ""},
		{"shift+enter then text", "\x1b[13;2ubye", "\x1b\rbye", ""},
		{"incomplete sequence held", "ab\x1b[13;2", "ab", "\x1b[13;2"},
		{"lone esc prefix held", "\x1b", "", "\x1b"},
		{"divergent seq passes through", "\x1b[13;3u", "\x1b[13;3u", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			emit, held := translateKeys([]byte(tc.in))
			if string(emit) != tc.wantEmit {
				t.Errorf("emit = %q, want %q", emit, tc.wantEmit)
			}
			if string(held) != tc.wantHeld {
				t.Errorf("held = %q, want %q", held, tc.wantHeld)
			}
		})
	}
}

// TestTranslateSplitSequence verifies a sequence split across two reads is
// reassembled and translated (the case the held buffer exists for).
func TestTranslateSplitSequence(t *testing.T) {
	emit1, held1 := translateKeys([]byte("\x1b[13"))
	if len(emit1) != 0 || string(held1) != "\x1b[13" {
		t.Fatalf("first chunk: emit=%q held=%q", emit1, held1)
	}
	emit2, held2 := translateKeys(append(held1, []byte(";2u")...))
	if string(emit2) != "\x1b\r" || len(held2) != 0 {
		t.Fatalf("second chunk: emit=%q held=%q", emit2, held2)
	}
}

// fakeFile lets us exercise translatingReader.Read without a real terminal by
// feeding it scripted chunks. translatingReader embeds *os.File only for the
// interface methods Bubble Tea needs; Read itself only touches the embedded
// file's Read, which we replace here via a small shim.
type scriptedReader struct {
	*translatingReader
	chunks [][]byte
}

func newScriptedReader(chunks ...string) *scriptedReader {
	s := &scriptedReader{translatingReader: &translatingReader{buf: make([]byte, 256)}}
	for _, c := range chunks {
		s.chunks = append(s.chunks, []byte(c))
	}
	return s
}

// readNext mimics translatingReader.Read but pulls raw bytes from the script
// instead of an *os.File, so we can test the pending/held plumbing in isolation.
func (s *scriptedReader) readNext(p []byte) (int, error) {
	if len(s.pending) > 0 {
		n := copy(p, s.pending)
		s.pending = s.pending[n:]
		return n, nil
	}
	if len(s.chunks) == 0 {
		return 0, io.EOF
	}
	raw := s.chunks[0]
	s.chunks = s.chunks[1:]
	data := append(append([]byte(nil), s.held...), raw...)
	emit, held := translateKeys(data)
	s.held = held
	if len(emit) == 0 {
		return 0, nil
	}
	m := copy(p, emit)
	if m < len(emit) {
		s.pending = append([]byte(nil), emit[m:]...)
	}
	return m, nil
}

func TestReaderReassemblesAcrossReads(t *testing.T) {
	s := newScriptedReader("\x1b[13", ";2u")
	var got bytes.Buffer
	p := make([]byte, 64)
	for {
		n, err := s.readNext(p)
		got.Write(p[:n])
		if err == io.EOF {
			break
		}
	}
	if got.String() != "\x1b\r" {
		t.Fatalf("got %q, want %q", got.String(), "\x1b\r")
	}
}
