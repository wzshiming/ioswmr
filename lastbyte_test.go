package ioswmr

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type lbReadResult struct {
	n    int
	data string
	err  error
}

func lbRead(r io.Reader) lbReadResult {
	p := make([]byte, 100)
	n, err := r.Read(p)
	return lbReadResult{n, string(p[:n]), err}
}

func lbStartRead(r io.Reader) <-chan lbReadResult {
	ch := make(chan lbReadResult, 1)
	go func() { ch <- lbRead(r) }()
	return ch
}

func lbExpectResult(t *testing.T, ch <-chan lbReadResult, what, want string, wantErr error) {
	t.Helper()
	var got lbReadResult
	select {
	case got = <-ch:
	case <-time.After(time.Second):
		t.Fatalf("%s did not return within 1s", what)
	}
	errOK := errors.Is(got.err, wantErr)
	if wantErr == io.EOF {
		errOK = got.err == io.EOF
	}
	if got.n != len(want) || got.data != want || !errOK {
		t.Fatalf("%s = (%d, %q, %v), want (%d, %q, %v)", what, got.n, got.data, got.err, len(want), want, wantErr)
	}
}

func lbExpectRead(t *testing.T, r io.Reader, what, want string, wantErr error) {
	t.Helper()
	lbExpectResult(t, lbStartRead(r), what, want, wantErr)
}

func lbAssertPending(t *testing.T, ch <-chan lbReadResult, what string) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("%s returned (%d, %q, %v) within 100ms, want it still blocked", what, got.n, got.data, got.err)
	case <-time.After(100 * time.Millisecond):
	}
}

// lbStartClose closes w on another goroutine, as Close blocks while the hook runs.
func lbStartClose(w io.Closer) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- w.Close() }()
	return ch
}

func lbExpectClose(t *testing.T, ch <-chan error) {
	t.Helper()
	if err := waitErr(t, ch, "writer Close did not return within 1s"); err != nil {
		t.Fatalf("writer Close = %v, want nil", err)
	}
}

type lbCountingHook struct {
	calls atomic.Int32
	err   error
}

func (h *lbCountingHook) run(io.ReadSeeker) error {
	h.calls.Add(1)
	return h.err
}

type lbBlockingHook struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func lbNewBlockingHook(t *testing.T) *lbBlockingHook {
	h := &lbBlockingHook{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(h.unblock)
	return h
}

func (h *lbBlockingHook) run(io.ReadSeeker) error {
	if h.calls.Add(1) == 1 {
		close(h.entered)
	}
	<-h.release
	return nil
}

func (h *lbBlockingHook) unblock() {
	h.once.Do(func() { close(h.release) })
}

// lbContentHook counts its runs and keeps how its r differed from want for the test goroutine.
type lbContentHook struct {
	calls    atomic.Int32
	want     string
	mismatch string
}

func (h *lbContentHook) run(r io.ReadSeeker) error {
	h.calls.Add(1)
	h.mismatch = lbContentMismatch(r, h.want)
	return nil
}

func (h *lbContentHook) expectContent(t *testing.T) {
	t.Helper()
	if h.mismatch != "" {
		t.Fatalf("inside the WithBeforeLastByteFunc hook: %s", h.mismatch)
	}
}

// lbContentMismatch reads r, seeks to its end and back and rereads it, describing the first result
// that differs from want, or "" if none does.
func lbContentMismatch(r io.ReadSeeker, want string) string {
	if b, err := io.ReadAll(r); string(b) != want || err != nil {
		return fmt.Sprintf("io.ReadAll(r) = (%q, %v), want (%q, nil)", b, err, want)
	}
	if n, err := r.Seek(0, io.SeekEnd); n != int64(len(want)) || err != nil {
		return fmt.Sprintf("r.Seek(0, io.SeekEnd) = (%d, %v), want (%d, nil)", n, err, len(want))
	}
	if n, err := r.Seek(0, io.SeekStart); n != 0 || err != nil {
		return fmt.Sprintf("r.Seek(0, io.SeekStart) = (%d, %v), want (0, nil)", n, err)
	}
	if b, err := io.ReadAll(r); string(b) != want || err != nil {
		return fmt.Sprintf("io.ReadAll(r) after rewinding = (%q, %v), want (%q, nil)", b, err, want)
	}
	return ""
}

func lbExpectCalls(t *testing.T, calls *atomic.Int32, want int32, when string) {
	t.Helper()
	if n := calls.Load(); n != want {
		t.Fatalf("WithBeforeLastByteFunc hook ran %d times %s, want %d", n, when, want)
	}
}

func lbClosedStream(t *testing.T, data string, closeErr error, hook func(io.ReadSeeker) error) SWMR {
	t.Helper()
	m := NewSWMR(nil, WithBeforeLastByteFunc(hook))
	w := m.Writer()
	writeString(t, w, data)
	if err := w.CloseWithError(closeErr); err != nil {
		t.Fatalf("CloseWithError(%v): %v", closeErr, err)
	}
	return m
}

func lbMustReader(t *testing.T, m SWMR) io.ReadCloser {
	t.Helper()
	r, err := m.NewReader(0)
	if err != nil {
		t.Fatalf("NewReader(0): %v", err)
	}
	return r
}

func lbMustReadSeeker(t *testing.T, m SWMR, length int) io.ReadSeekCloser {
	t.Helper()
	rs, err := m.NewReadSeeker(0, length)
	if err != nil {
		t.Fatalf("NewReadSeeker(0, %d): %v", length, err)
	}
	return rs
}

// A normal Close runs the hook, and the last byte is then returned alone.
func TestLastByteSequence(t *testing.T) {
	expect := func(t *testing.T, m SWMR, r io.Reader, h *lbCountingHook) {
		t.Helper()
		lbExpectCalls(t, &h.calls, 1, "after Close")
		lbExpectRead(t, r, "Read#1", "hell", nil)
		lbExpectCalls(t, &h.calls, 1, "before the last byte")
		lbExpectRead(t, r, "Read#2 at the last byte", "o", nil)
		lbExpectCalls(t, &h.calls, 1, "after the last byte")
		lbExpectRead(t, r, "Read#3 past the last byte", "", io.EOF)
		if n := m.ReaderUsing(); n != 0 {
			t.Fatalf("ReaderUsing() = %d after io.EOF, want 0", n)
		}
	}
	t.Run("reader after close", func(t *testing.T) {
		h := &lbCountingHook{}
		m := lbClosedStream(t, "hello", nil, h.run)
		expect(t, m, lbMustReader(t, m), h)
	})
	t.Run("reader before write", func(t *testing.T) {
		h := &lbCountingHook{}
		m := NewSWMR(nil, WithBeforeLastByteFunc(h.run))
		r := lbMustReader(t, m)
		w := m.Writer()
		writeString(t, w, "hello")
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		expect(t, m, r, h)
	})
}

// A hook error replaces the last byte for every reader and is kept for the stream.
func TestLastByteHookError(t *testing.T) {
	errX := errors.New("before-last-byte hook failed")
	t.Run("autoclose", func(t *testing.T) {
		h := &lbCountingHook{err: errX}
		m := NewSWMR(nil, WithAutoClose(), WithBeforeLastByteFunc(h.run))
		r := lbMustReader(t, m)
		w := m.Writer()
		writeString(t, w, "hello")
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		lbExpectCalls(t, &h.calls, 1, "after Close")
		lbExpectRead(t, r, "Read#1", "hell", nil)
		lbExpectRead(t, r, "Read#2 at the last byte", "", errX)
		lbExpectRead(t, r, "Read#3 at the last byte", "", errX)
		lbExpectCalls(t, &h.calls, 1, "after two reads of the last byte")
		if n := m.ReaderUsing(); n != 0 {
			t.Fatalf("ReaderUsing() = %d after the hook error, want 0", n)
		}
		if ok, err := m.TryClose(); !ok || err != nil {
			t.Fatalf("TryClose() = (%v, %v), want (true, nil)", ok, err)
		}
		if _, err := m.NewReader(0); !errors.Is(err, ErrClosedPipe) {
			t.Fatalf("NewReader(0) after auto close: err = %v, want ErrClosedPipe", err)
		}
	})
	t.Run("no autoclose", func(t *testing.T) {
		h := &lbCountingHook{err: errX}
		m := lbClosedStream(t, "hello", nil, h.run)
		lbExpectCalls(t, &h.calls, 1, "after Close")
		r := lbMustReader(t, m)
		lbExpectRead(t, r, "Read#1", "hell", nil)
		lbExpectRead(t, r, "Read#2 at the last byte", "", errX)
		lbExpectRead(t, r, "Read#3 at the last byte", "", errX)
		if n := m.ReaderUsing(); n != 0 {
			t.Fatalf("ReaderUsing() = %d after the hook error, want 0", n)
		}
		late := lbMustReader(t, m)
		lbExpectRead(t, late, "late reader Read#1", "hell", nil)
		lbExpectRead(t, late, "late reader Read#2 at the last byte", "", errX)
		lbExpectCalls(t, &h.calls, 1, "for two readers")
	})
}

// Close runs the hook once for the stream, and readers reaching the last byte meanwhile wait for it.
func TestLastByteHookOncePerStream(t *testing.T) {
	h := lbNewBlockingHook(t)
	m := NewSWMR(nil, WithBeforeLastByteFunc(h.run))
	readers := []io.Reader{lbMustReader(t, m), lbMustReader(t, m), lbMustReadSeeker(t, m, 5)}
	w := m.Writer()
	writeString(t, w, "hello")
	closed := lbStartClose(w)
	waitFor(t, h.entered, "writer Close did not run the WithBeforeLastByteFunc hook")

	first := make(chan lbReadResult, len(readers))
	second := make(chan lbReadResult, len(readers))
	third := make(chan lbReadResult, len(readers))
	for _, r := range readers {
		go func(r io.Reader) {
			first <- lbRead(r)
			second <- lbRead(r)
			third <- lbRead(r)
		}(r)
	}
	for range readers {
		lbExpectResult(t, first, "Read#1 while the hook runs", "hell", nil)
	}
	lbAssertPending(t, second, "Read#2 at the last byte while the hook runs")
	h.unblock()
	for range readers {
		lbExpectResult(t, second, "Read#2 at the last byte after the hook", "o", nil)
	}
	for range readers {
		lbExpectResult(t, third, "Read#3 past the last byte", "", io.EOF)
	}
	lbExpectClose(t, closed)
	lbExpectCalls(t, &h.calls, 1, "for Close and three readers")
}

// While the writer is open the newest byte is withheld.
func TestLastByteWithheldWhileWriting(t *testing.T) {
	t.Run("chunks", func(t *testing.T) {
		h := &lbCountingHook{}
		m := NewSWMR(nil, WithBeforeLastByteFunc(h.run))
		w := m.Writer()
		r := lbMustReader(t, m)
		writeString(t, w, "hel")
		lbExpectRead(t, r, "Read after writing \"hel\"", "he", nil)
		pending := lbStartRead(r)
		lbAssertPending(t, pending, "Read with only the newest byte unread")
		writeString(t, w, "lo")
		lbExpectResult(t, pending, "Read woken by writing \"lo\"", "ll", nil)
		lbExpectCalls(t, &h.calls, 0, "while the writer is open")
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		lbExpectCalls(t, &h.calls, 1, "after Close")
		lbExpectRead(t, r, "Read of the last byte after Close", "o", nil)
		lbExpectRead(t, r, "Read past the last byte", "", io.EOF)
	})
	t.Run("byte by byte", func(t *testing.T) {
		h := &lbCountingHook{}
		m := NewSWMR(nil, WithBeforeLastByteFunc(h.run))
		w := m.Writer()
		r := lbMustReader(t, m)
		writeString(t, w, "a")
		pending := lbStartRead(r)
		lbAssertPending(t, pending, "Read with only \"a\" written")
		writeString(t, w, "b")
		lbExpectResult(t, pending, "Read woken by writing \"b\"", "a", nil)
		pending = lbStartRead(r)
		writeString(t, w, "c")
		lbExpectResult(t, pending, "Read after writing \"c\"", "b", nil)
		pending = lbStartRead(r)
		lbExpectCalls(t, &h.calls, 0, "while the writer is open")
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		lbExpectCalls(t, &h.calls, 1, "after Close")
		lbExpectResult(t, pending, "Read of the last byte after Close", "c", nil)
		lbExpectRead(t, r, "Read past the last byte", "", io.EOF)
	})
}

// A writer error replaces the last byte for both reader kinds and the hook never runs.
func TestLastByteWriterError(t *testing.T) {
	errX := errors.New("writer failed")
	t.Run("plain", func(t *testing.T) {
		h := &lbCountingHook{}
		m := lbClosedStream(t, "hello", errX, h.run)
		r := lbMustReader(t, m)
		lbExpectRead(t, r, "Read#1", "hell", nil)
		lbExpectRead(t, r, "Read#2 at the last byte", "", errX)
		lbExpectCalls(t, &h.calls, 0, "after a writer error")
		if n := m.ReaderUsing(); n != 0 {
			t.Fatalf("ReaderUsing() = %d after the writer error, want 0", n)
		}
	})
	t.Run("readseeker", func(t *testing.T) {
		h := &lbCountingHook{}
		m := lbClosedStream(t, "hello", errX, h.run)
		rs := lbMustReadSeeker(t, m, 5)
		lbExpectRead(t, rs, "Read#1", "hell", nil)
		lbExpectRead(t, rs, "Read#2 at the last byte", "", errX)
		lbExpectCalls(t, &h.calls, 0, "after a writer error")
	})
}

// A read seeker waits for the hook only when its window reaches the stream's last byte.
func TestLastByteReadSeeker(t *testing.T) {
	t.Run("full", func(t *testing.T) {
		h := &lbCountingHook{}
		m := lbClosedStream(t, "hello", nil, h.run)
		lbExpectCalls(t, &h.calls, 1, "after Close")
		rs := lbMustReadSeeker(t, m, 5)
		lbExpectRead(t, rs, "Read#1", "hell", nil)
		lbExpectRead(t, rs, "Read#2 at the last byte", "o", nil)
		lbExpectRead(t, rs, "Read#3 at length", "", io.EOF)
		assertSeek(t, rs, 0, io.SeekStart, 0)
		lbExpectRead(t, rs, "Read#1 after rewinding", "hell", nil)
		lbExpectRead(t, rs, "Read#2 at the last byte after rewinding", "o", nil)
		lbExpectRead(t, rs, "Read#3 at length after rewinding", "", io.EOF)
		lbExpectCalls(t, &h.calls, 1, "after rereading the last byte")
	})
	t.Run("short", func(t *testing.T) {
		h := lbNewBlockingHook(t)
		m := NewSWMR(nil, WithBeforeLastByteFunc(h.run))
		rs := lbMustReadSeeker(t, m, 3)
		w := m.Writer()
		writeString(t, w, "hello")
		closed := lbStartClose(w)
		waitFor(t, h.entered, "writer Close did not run the WithBeforeLastByteFunc hook")
		lbExpectRead(t, rs, "Read#1 while the hook runs", "hel", nil)
		lbExpectRead(t, rs, "Read#2 at length while the hook runs", "", io.EOF)
		h.unblock()
		lbExpectClose(t, closed)
		lbExpectCalls(t, &h.calls, 1, "for Close")
	})
	t.Run("long", func(t *testing.T) {
		h := &lbCountingHook{}
		m := lbClosedStream(t, "hello", nil, h.run)
		rs := lbMustReadSeeker(t, m, 6)
		lbExpectRead(t, rs, "Read#1", "hell", nil)
		lbExpectRead(t, rs, "Read#2 at the last byte", "o", nil)
		lbExpectRead(t, rs, "Read#3 past the stream end", "", io.ErrUnexpectedEOF)
		lbExpectCalls(t, &h.calls, 1, "after Close and the last byte")
	})
}

// A read seeker that gets the hook error keeps holding the stream until Close and can rewind.
func TestLastByteReadSeekerHookErrorKeepsHold(t *testing.T) {
	errX := errors.New("before-last-byte hook failed")
	h := &lbCountingHook{err: errX}
	m := NewSWMR(nil, WithAutoClose(), WithBeforeLastByteFunc(h.run))
	rs := lbMustReadSeeker(t, m, 5)
	w := m.Writer()
	writeString(t, w, "hello")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	lbExpectRead(t, rs, "Read#1", "hell", nil)
	lbExpectRead(t, rs, "Read#2 at the last byte", "", errX)
	if n := m.ReaderUsing(); n != 1 {
		t.Fatalf("ReaderUsing() = %d after the hook error, want 1 until Close", n)
	}
	if ok, err := m.TryClose(); ok || err != nil {
		t.Fatalf("TryClose() = (%v, %v) before Close, want (false, nil)", ok, err)
	}
	assertSeek(t, rs, 0, io.SeekStart, 0)
	lbExpectRead(t, rs, "Read#1 after rewinding", "hell", nil)
	lbExpectRead(t, rs, "Read#2 at the last byte after rewinding", "", errX)
	lbExpectCalls(t, &h.calls, 1, "after rereading the last byte")
	if err := rs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := m.ReaderUsing(); n != 0 {
		t.Fatalf("ReaderUsing() = %d after Close, want 0", n)
	}
	if ok, err := m.TryClose(); !ok || err != nil {
		t.Fatalf("TryClose() = (%v, %v) after Close, want (true, nil)", ok, err)
	}
}

// Closing a reader does not interrupt the hook; its waiting Read gets ErrClosedPipe once the hook returns.
func TestLastByteCloseDuringHook(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		lbCloseDuringHook(t, func(m SWMR) io.ReadCloser { return lbMustReader(t, m) })
	})
	t.Run("readseeker", func(t *testing.T) {
		lbCloseDuringHook(t, func(m SWMR) io.ReadCloser { return lbMustReadSeeker(t, m, 5) })
	})
}

func lbCloseDuringHook(t *testing.T, newReader func(m SWMR) io.ReadCloser) {
	h := lbNewBlockingHook(t)
	m := NewSWMR(nil, WithBeforeLastByteFunc(h.run))
	a := newReader(m)
	b := newReader(m)
	w := m.Writer()
	writeString(t, w, "hello")
	writerClosed := lbStartClose(w)
	waitFor(t, h.entered, "writer Close did not run the WithBeforeLastByteFunc hook")
	lbExpectRead(t, a, "reader A Read#1", "hell", nil)
	lbExpectRead(t, b, "reader B Read#1", "hell", nil)

	resA := lbStartRead(a)
	resB := lbStartRead(b)
	time.Sleep(20 * time.Millisecond)
	closed := make(chan struct{})
	go func() {
		_ = a.Close()
		_ = b.Close()
		close(closed)
	}()
	waitFor(t, closed, "reader Close blocked while the hook runs")
	lbAssertPending(t, resA, "reader A Read#2 closed while waiting for the hook")
	h.unblock()

	lbExpectResult(t, resA, "reader A Read#2 closed while waiting for the hook", "", ErrClosedPipe)
	lbExpectResult(t, resB, "reader B Read#2 closed while waiting for the hook", "", ErrClosedPipe)
	lbExpectClose(t, writerClosed)
	if n := m.ReaderUsing(); n != 0 {
		t.Fatalf("ReaderUsing() = %d after closing both readers and the writer, want 0", n)
	}
}

// Close runs the hook on an empty stream too, with an empty r, and readers get io.EOF.
func TestLastByteEmptyStream(t *testing.T) {
	h := &lbContentHook{want: ""}
	m := NewSWMR(nil, WithBeforeLastByteFunc(h.run))
	if err := m.Writer().Close(); err != nil {
		t.Fatal(err)
	}
	lbExpectCalls(t, &h.calls, 1, "for an empty stream")
	h.expectContent(t)
	lbExpectRead(t, lbMustReader(t, m), "Read of an empty stream", "", io.EOF)
}

// The hook's r reads the whole stream, last byte included, and readers still get all of it afterwards.
func TestLastByteHookReadsContent(t *testing.T) {
	h := &lbContentHook{want: "hello"}
	m := lbClosedStream(t, "hello", nil, h.run)
	lbExpectCalls(t, &h.calls, 1, "after Close")
	h.expectContent(t)
	r := lbMustReader(t, m)
	lbExpectRead(t, r, "Read#1", "hell", nil)
	lbExpectRead(t, r, "Read#2 at the last byte", "o", nil)
	lbExpectRead(t, r, "Read#3 past the last byte", "", io.EOF)
	lbExpectCalls(t, &h.calls, 1, "after the last byte")
}

// Close runs the hook with no reader, and auto close releases the buffer once it returns.
func TestLastByteCloseRunsHook(t *testing.T) {
	h := &lbContentHook{want: "hello"}
	m := NewSWMR(nil, WithAutoClose(), WithBeforeLastByteFunc(h.run))
	w := m.Writer()
	writeString(t, w, "hello")
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	lbExpectCalls(t, &h.calls, 1, "by Close with no reader")
	h.expectContent(t)
	if _, err := m.NewReader(0); !errors.Is(err, ErrClosedPipe) {
		t.Fatalf("NewReader(0) after Close: err = %v, want ErrClosedPipe from auto close", err)
	}
	if ok, err := m.TryClose(); !ok || err != nil {
		t.Fatalf("TryClose() = (%v, %v) after Close, want (true, nil)", ok, err)
	}
}

// The stream stays held while the hook runs, so closing the last reader meanwhile does not release it.
func TestLastByteHookHoldsStream(t *testing.T) {
	block := lbNewBlockingHook(t)
	content := &lbContentHook{want: "hello"}
	var using int
	var m SWMR
	m = NewSWMR(nil, WithAutoClose(), WithBeforeLastByteFunc(func(r io.ReadSeeker) error {
		using = m.ReaderUsing()
		_ = block.run(r)
		return content.run(r)
	}))
	a := lbMustReader(t, m)
	w := m.Writer()
	writeString(t, w, "hello")
	closed := lbStartClose(w)
	waitFor(t, block.entered, "writer Close did not run the WithBeforeLastByteFunc hook")
	if using != 2 {
		t.Fatalf("ReaderUsing() = %d inside the hook with one reader open, want 2", using)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("reader Close: %v", err)
	}
	block.unblock()
	lbExpectClose(t, closed)
	content.expectContent(t)
	if ok, err := m.TryClose(); !ok || err != nil {
		t.Fatalf("TryClose() = (%v, %v) after the hook, want (true, nil)", ok, err)
	}
}
