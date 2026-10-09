package ioswmr

import (
	"errors"
	"io"
	"testing"
	"time"
)

// seekBufferKinds builds one fresh buffer per kind; "hybridSpill" has a 4-byte memory tier so the
// 6-byte payloads below land in a temp file, "hybridMemory" must never spill.
var seekBufferKinds = []struct {
	name string
	new  func(t *testing.T) Buffer
}{
	{"memory", func(t *testing.T) Buffer { return NewMemoryBuffer(nil) }},
	{"temporaryFile", func(t *testing.T) Buffer { return NewTemporaryFileBuffer(newTempRecorder(t).create) }},
	{"hybridSpill", func(t *testing.T) Buffer {
		return NewMemoryOrTemporaryFileBuffer(make([]byte, 0, 4), newTempRecorder(t).create)
	}},
	{"hybridMemory", func(t *testing.T) Buffer { return NewMemoryOrTemporaryFileBuffer(nil, noSpill(t)) }},
}

type readResult struct {
	data string
	err  error
}

// startRead performs one Read in the background so the test can observe whether it blocks.
func startRead(r io.Reader) <-chan readResult {
	ch := make(chan readResult, 1)
	go func() {
		p := make([]byte, 32)
		n, err := r.Read(p)
		ch <- readResult{string(p[:n]), err}
	}()
	return ch
}

func assertBlocked(t *testing.T, ch <-chan readResult, msg string) {
	t.Helper()
	select {
	case res := <-ch:
		t.Fatalf("%s: Read returned (%q, %v)", msg, res.data, res.err)
	case <-time.After(50 * time.Millisecond):
	}
}

func waitRead(t *testing.T, ch <-chan readResult, msg string) readResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(time.Second):
		t.Fatal(msg)
		return readResult{}
	}
}

func assertSeek(t *testing.T, s io.Seeker, offset int64, whence int, want int64) {
	t.Helper()
	if pos, err := s.Seek(offset, whence); err != nil || pos != want {
		t.Fatalf("Seek(%d, %d) = (%d, %v), want (%d, nil)", offset, whence, pos, err, want)
	}
}

// assertSeekRejected requires an error; a nil target accepts any error.
func assertSeekRejected(t *testing.T, s io.Seeker, offset int64, whence int, target error) {
	t.Helper()
	pos, err := s.Seek(offset, whence)
	if err == nil || (target != nil && !errors.Is(err, target)) {
		t.Fatalf("Seek(%d, %d) = (%d, %v), want error %v", offset, whence, pos, err, target)
	}
}

func writeString(t *testing.T, w io.Writer, s string) {
	t.Helper()
	if n, err := w.Write([]byte(s)); err != nil || n != len(s) {
		t.Fatalf("Write(%q) = (%d, %v), want (%d, nil)", s, n, err, len(s))
	}
}

func readStream(t *testing.T, m SWMR) string {
	t.Helper()
	r, err := m.NewReader(0)
	if err != nil {
		t.Fatalf("NewReader(0): %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return string(got)
}

func mustTryClose(t *testing.T, m SWMR) {
	t.Helper()
	if ok, err := m.TryClose(); err != nil || !ok {
		t.Errorf("TryClose() = (%v, %v), want (true, nil); ReaderUsing() = %d, WriteDone() = %v", ok, err, m.ReaderUsing(), m.WriteDone())
	}
}

func TestSeekResume(t *testing.T) {
	for _, kind := range seekBufferKinds {
		t.Run(kind.name, func(t *testing.T) {
			buf := kind.new(t)
			writeString(t, buf, "Hello ")

			m := NewSWMR(buf)
			defer mustTryClose(t, m)
			if m.Length() != 0 {
				t.Fatalf("Length() = %d before Seek, want 0", m.Length())
			}

			w := m.Writer()
			assertSeek(t, w, 0, io.SeekEnd, 6)
			if m.Length() != 6 {
				t.Fatalf("Length() = %d after Seek, want 6", m.Length())
			}
			// Equivalent forms resolve to the same position and stay valid until the first Write.
			assertSeek(t, w, 6, io.SeekStart, 6)
			assertSeek(t, w, 0, io.SeekCurrent, 6)

			writeString(t, w, "World!")
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if got := readStream(t, m); got != "Hello World!" {
				t.Fatalf("stream = %q, want %q", got, "Hello World!")
			}
		})
	}
}

func TestSeekFreshBuffer(t *testing.T) {
	for _, kind := range seekBufferKinds {
		t.Run(kind.name, func(t *testing.T) {
			m := NewSWMR(kind.new(t))
			defer mustTryClose(t, m)

			w := m.Writer()
			assertSeek(t, w, 0, io.SeekEnd, 0)
			assertSeek(t, w, 0, io.SeekStart, 0)
			assertSeek(t, w, 0, io.SeekCurrent, 0)
			if m.Length() != 0 {
				t.Fatalf("Length() = %d after Seek, want 0", m.Length())
			}

			writeString(t, w, "abc")
			if m.Length() != 3 {
				t.Fatalf("Length() = %d after Write, want 3", m.Length())
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if got := readStream(t, m); got != "abc" {
				t.Fatalf("stream = %q, want %q", got, "abc")
			}
		})
	}
}

func TestSeekRejections(t *testing.T) {
	t.Run("otherPositionBeforeWrite", func(t *testing.T) {
		m := NewSWMR(NewMemoryBuffer(nil))
		defer mustTryClose(t, m)
		r, err := m.NewReader(0)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		read := startRead(r)

		w := m.Writer()
		assertSeekRejected(t, w, 3, io.SeekStart, ErrUnsupportedSeek)
		assertSeekRejected(t, w, -1, io.SeekEnd, ErrUnsupportedSeek)
		assertSeekRejected(t, w, 0, 7, ErrUnsupportedSeek)
		if m.Length() != 0 {
			t.Fatalf("Length() = %d after rejected Seek, want 0", m.Length())
		}
		assertBlocked(t, read, "reader woke after a rejected Seek")

		writeString(t, w, "abc")
		if res := waitRead(t, read, "reader did not wake after Write"); res.data != "abc" || res.err != nil {
			t.Fatalf("Read = (%q, %v), want (\"abc\", nil)", res.data, res.err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	})

	for _, kind := range seekBufferKinds {
		t.Run("afterWrite/"+kind.name, func(t *testing.T) {
			m := NewSWMR(kind.new(t))
			defer mustTryClose(t, m)

			w := m.Writer()
			writeString(t, w, "abcdef")
			assertSeekRejected(t, w, 0, io.SeekStart, ErrUnsupportedSeek)
			assertSeekRejected(t, w, 0, io.SeekEnd, ErrUnsupportedSeek)
			assertSeekRejected(t, w, 6, io.SeekStart, ErrUnsupportedSeek)
			if m.Length() != 6 {
				t.Fatalf("Length() = %d after rejected Seek, want 6", m.Length())
			}

			writeString(t, w, "ghijkl")
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if got := readStream(t, m); got != "abcdefghijkl" {
				t.Fatalf("stream = %q, want %q", got, "abcdefghijkl")
			}
			if m.Length() != 12 {
				t.Fatalf("Length() = %d, want 12", m.Length())
			}
		})
	}

	t.Run("closedWriter", func(t *testing.T) {
		m := NewSWMR(NewMemoryBuffer(nil))
		defer mustTryClose(t, m)
		w := m.Writer()
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		assertSeekRejected(t, w, 0, io.SeekEnd, ErrClosedPipe)
	})

	if !errors.Is(ErrUnsupportedSeek, errors.ErrUnsupported) {
		t.Error("ErrUnsupportedSeek does not wrap errors.ErrUnsupported")
	}
}

// The in-memory buffers accept only their current size as a seek target; a temporary
// file buffer behaves the same until its file exists.
func TestBufferSeekAppendOnly(t *testing.T) {
	kinds := []struct {
		name string
		new  func(t *testing.T) Buffer
		data string
	}{
		{"memory", func(t *testing.T) Buffer { return NewMemoryBuffer(nil) }, "abc"},
		{"memoryOrTemporaryFile", func(t *testing.T) Buffer { return NewMemoryOrTemporaryFileBuffer(nil, noSpill(t)) }, "abc"},
		{"freshTemporaryFile", func(t *testing.T) Buffer { return NewTemporaryFileBuffer(newTempRecorder(t).create) }, ""},
	}
	for _, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			buf := kind.new(t)
			defer buf.Close()
			if kind.data != "" {
				writeString(t, buf, kind.data)
			}
			size := int64(len(kind.data))

			assertSeek(t, buf, 0, io.SeekEnd, size)
			assertSeek(t, buf, size, io.SeekStart, size)
			assertSeek(t, buf, 0, io.SeekCurrent, size)
			assertSeekRejected(t, buf, 1, io.SeekStart, errors.ErrUnsupported)
			assertSeekRejected(t, buf, size+1, io.SeekStart, errors.ErrUnsupported)
			assertSeekRejected(t, buf, -1, io.SeekEnd, nil)
			// Rejected seeks leave the buffer untouched.
			assertSeek(t, buf, 0, io.SeekCurrent, size)
			assertContent(t, buf, kind.data)
		})
	}
}

func TestSeekWakesBlockedReader(t *testing.T) {
	buf := NewMemoryBuffer(nil)
	writeString(t, buf, "Hello ")
	m := NewSWMR(buf)
	defer mustTryClose(t, m)

	r, err := m.NewReader(0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	read := startRead(r)
	assertBlocked(t, read, "reader woke before the pre-existing bytes were published")

	w := m.Writer()
	assertSeek(t, w, 0, io.SeekEnd, 6)
	if res := waitRead(t, read, "reader did not wake after Seek"); res.data != "Hello " || res.err != nil {
		t.Fatalf("Read = (%q, %v), want (\"Hello \", nil)", res.data, res.err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
