package ioswmr

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBuffer is an in-memory Buffer whose ReadAt and Close can be overridden.
type fakeBuffer struct {
	data       []byte
	closeCalls atomic.Int32
	closeErr   error
	readAt     func(p []byte, off int64) (int, error)
}

func (f *fakeBuffer) Write(p []byte) (int, error) {
	f.data = append(f.data, p...)
	return len(p), nil
}

func (f *fakeBuffer) ReadAt(p []byte, off int64) (int, error) {
	if f.readAt != nil {
		return f.readAt(p, off)
	}
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	return copy(p, f.data[off:]), nil
}

func (f *fakeBuffer) Seek(offset int64, whence int) (int64, error) {
	return int64(len(f.data)), nil
}

func (f *fakeBuffer) Close() error {
	f.closeCalls.Add(1)
	return f.closeErr
}

func waitFor(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal(msg)
	}
}

func waitErr(t *testing.T, ch <-chan error, msg string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(time.Second):
		t.Fatal(msg)
		return nil
	}
}

// pauseBeforeClose returns an auto-closing SWMR whose before-close hook blocks:
// entered is closed when the hook starts, the hook returns once release is closed.
func pauseBeforeClose() (m SWMR, entered chan struct{}, release chan struct{}) {
	entered = make(chan struct{})
	release = make(chan struct{})
	m = NewSWMR(nil,
		WithAutoClose(),
		WithBeforeCloseFunc(func() {
			close(entered)
			<-release
		}),
	)
	return m, entered, release
}

func TestAdmissionDuringTeardown(t *testing.T) {
	payload := []byte("Hello World!")

	t.Run("late joiner rejected", func(t *testing.T) {
		m, entered, release := pauseBeforeClose()
		w := m.Writer()
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}

		closeDone := make(chan error, 1)
		go func() { closeDone <- w.Close() }()
		waitFor(t, entered, "before-close hook did not run")

		if rs, err := m.NewReadSeeker(0, len(payload)); !errors.Is(err, ErrClosedPipe) {
			t.Errorf("NewReadSeeker during teardown = (%v, %v), want ErrClosedPipe", rs, err)
		}
		if r, err := m.NewReader(0); !errors.Is(err, ErrClosedPipe) {
			t.Errorf("NewReader during teardown = (%v, %v), want ErrClosedPipe", r, err)
		}
		if n := m.ReaderUsing(); n != 0 {
			t.Errorf("ReaderUsing() = %d, want 0", n)
		}

		close(release)
		if err := waitErr(t, closeDone, "writer Close did not return"); err != nil {
			t.Fatalf("writer Close: %v", err)
		}
	})

	t.Run("admitted reader keeps buffer alive", func(t *testing.T) {
		var before atomic.Int32
		m := NewSWMR(nil,
			WithAutoClose(),
			WithBeforeCloseFunc(func() { before.Add(1) }),
		)
		w := m.Writer()
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
		r, err := m.NewReader(0)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		if ok, err := m.TryClose(); ok || err != nil {
			t.Fatalf("TryClose with an open reader = (%v, %v), want (false, nil)", ok, err)
		}
		if n := before.Load(); n != 0 {
			t.Fatalf("before-close hook ran %d times with an open reader", n)
		}

		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("ReadAll = %q, want %q", got, payload)
		}
		if n := m.ReaderUsing(); n != 0 {
			t.Fatalf("ReaderUsing() after EOF = %d, want 0", n)
		}
		if n := before.Load(); n != 1 {
			t.Fatalf("before-close hook ran %d times, want 1", n)
		}
	})
}

func TestAdmissionTeardownStress(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 256)
	for i := 0; i < 5000; i++ {
		iteration := i
		m := NewSWMR(nil, WithAutoClose())
		w := m.Writer()
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := w.Close(); err != nil {
				t.Errorf("iteration %d: Close: %v", iteration, err)
			}
		}()
		go func() {
			defer wg.Done()
			r, err := m.NewReader(0)
			if err != nil {
				if !errors.Is(err, ErrClosedPipe) {
					t.Errorf("iteration %d: NewReader: %v", iteration, err)
				}
				return
			}
			defer r.Close()
			got, err := io.ReadAll(r)
			if err != nil {
				t.Errorf("iteration %d: ReadAll: %v", iteration, err)
				return
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("iteration %d: ReadAll returned %d bytes, want %d", iteration, len(got), len(payload))
			}
		}()
		wg.Wait()

		if n := m.ReaderUsing(); n != 0 {
			t.Fatalf("iteration %d: ReaderUsing() = %d, want 0", iteration, n)
		}
		if ok, err := m.TryClose(); !ok || err != nil {
			t.Fatalf("iteration %d: TryClose = (%v, %v), want (true, nil)", iteration, ok, err)
		}
	}
}

func TestCloseHooksRunOutsideLock(t *testing.T) {
	var before, after atomic.Int32
	var m SWMR
	m = NewSWMR(nil,
		WithBeforeCloseFunc(func() {
			before.Add(1)
			_ = m.Length()
			_ = m.ReaderUsing()
		}),
		WithAfterCloseFunc(func(err error) error {
			after.Add(1)
			_, _ = m.TryClose()
			return err
		}),
	)
	w := m.Writer()
	if _, err := w.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	type result struct {
		ok  bool
		err error
	}
	res := make(chan result, 1)
	go func() {
		ok, err := m.TryClose()
		res <- result{ok, err}
	}()
	select {
	case got := <-res:
		if !got.ok || got.err != nil {
			t.Fatalf("TryClose = (%v, %v), want (true, nil)", got.ok, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("TryClose did not complete within 1s: hooks ran under the lock")
	}
	if n := before.Load(); n != 1 {
		t.Errorf("before-close hook ran %d times, want 1", n)
	}
	if n := after.Load(); n != 1 {
		t.Errorf("after-close hook ran %d times, want 1", n)
	}
}

func TestCloseErrorPublication(t *testing.T) {
	custom := errors.New("custom close error")
	for i := 0; i < 2000; i++ {
		iteration := i
		m := NewSWMR(nil)
		w := m.Writer()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := w.CloseWithError(custom); err != nil {
				t.Errorf("iteration %d: CloseWithError: %v", iteration, err)
			}
		}()
		go func() {
			defer wg.Done()
			r, err := m.NewReader(0)
			if err != nil {
				t.Errorf("iteration %d: NewReader: %v", iteration, err)
				return
			}
			defer r.Close()
			buf := make([]byte, 8)
			for {
				n, err := r.Read(buf)
				if err == nil {
					if n == 0 {
						t.Errorf("iteration %d: Read returned (0, nil)", iteration)
						return
					}
					continue
				}
				if err != custom {
					t.Errorf("iteration %d: terminal Read error = %v, want %v", iteration, err, custom)
				}
				return
			}
		}()
		wg.Wait()

		if ok, err := m.TryClose(); !ok || err != nil {
			t.Fatalf("iteration %d: TryClose = (%v, %v), want (true, nil)", iteration, ok, err)
		}
	}
}

func TestWriteSeekAfterDisposal(t *testing.T) {
	m, entered, release := pauseBeforeClose()
	w := m.Writer()
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- w.Close() }()
	waitFor(t, entered, "before-close hook did not run")

	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrClosedPipe) {
		t.Errorf("Write after disposal: err = %v, want ErrClosedPipe", err)
	}
	if _, err := w.Seek(0, io.SeekEnd); err == nil {
		t.Error("Seek after disposal: err = nil, want error")
	}

	close(release)
	if err := waitErr(t, closeDone, "writer Close did not return"); err != nil {
		t.Fatalf("writer Close: %v", err)
	}
}

func TestWriteRacingAutoCloseDoesNotPanic(t *testing.T) {
	for i := 0; i < 2000; i++ {
		iteration := i
		m := NewSWMR(nil, WithAutoClose())
		w := m.Writer()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("iteration %d: Write panicked: %v", iteration, r)
				}
			}()
			for {
				if _, err := w.Write([]byte("x")); err != nil {
					if !errors.Is(err, ErrClosedPipe) {
						t.Errorf("iteration %d: Write: %v", iteration, err)
					}
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			if err := w.Close(); err != nil {
				t.Errorf("iteration %d: Close: %v", iteration, err)
			}
		}()
		wg.Wait()
	}
}

func TestReaderCloseWakesBlockedRead(t *testing.T) {
	cases := []struct {
		name string
		open func(m SWMR) (io.ReadCloser, error)
	}{
		{"reader", func(m SWMR) (io.ReadCloser, error) { return m.NewReader(0) }},
		{"readSeeker", func(m SWMR) (io.ReadCloser, error) { return m.NewReadSeeker(0, 10) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewSWMR(nil)
			w := m.Writer()
			r, err := tc.open(m)
			if err != nil {
				t.Fatal(err)
			}

			type result struct {
				n   int
				err error
			}
			res := make(chan result, 1)
			go func() {
				n, err := r.Read(make([]byte, 4))
				res <- result{n, err}
			}()
			// Give Read time to block on the notification channel.
			time.Sleep(20 * time.Millisecond)

			if err := r.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			select {
			case got := <-res:
				if got.n != 0 || !errors.Is(got.err, ErrClosedPipe) {
					t.Fatalf("blocked Read returned (%d, %v), want (0, ErrClosedPipe)", got.n, got.err)
				}
			case <-time.After(time.Second):
				t.Fatal("Read still blocked 1s after Close")
			}
			if _, err := r.Read(make([]byte, 4)); !errors.Is(err, ErrClosedPipe) {
				t.Fatalf("Read after Close: err = %v, want ErrClosedPipe", err)
			}
			if n := m.ReaderUsing(); n != 0 {
				t.Fatalf("ReaderUsing() = %d, want 0", n)
			}

			if _, err := w.Write([]byte("late data")); err != nil {
				t.Fatalf("Write after reader Close: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("writer Close: %v", err)
			}
		})
	}
}

func TestConcurrentCloseSafety(t *testing.T) {
	custom := errors.New("custom close error")
	for i := 0; i < 1000; i++ {
		iteration := i
		m := NewSWMR(nil, WithAutoClose())
		w := m.Writer()
		r, err := m.NewReader(0)
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		run := func(name string, f func()) {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("iteration %d: %s panicked: %v", iteration, name, p)
				}
			}()
			f()
		}
		wg.Add(3)
		go run("reader Close", func() { _ = r.Close() })
		go run("reader Close", func() { _ = r.Close() })
		go run("CloseWithError", func() { _ = w.CloseWithError(custom) })
		wg.Wait()

		if n := m.ReaderUsing(); n != 0 {
			t.Fatalf("iteration %d: ReaderUsing() = %d, want 0", iteration, n)
		}
		if ok, err := m.TryClose(); !ok || err != nil {
			t.Fatalf("iteration %d: TryClose = (%v, %v), want (true, nil)", iteration, ok, err)
		}
	}
}

func TestTryCloseErrorSemantics(t *testing.T) {
	sentinel := errors.New("buffer close failed")
	fb := &fakeBuffer{closeErr: sentinel}
	var before, after atomic.Int32
	m := NewSWMR(fb,
		WithBeforeCloseFunc(func() { before.Add(1) }),
		WithAfterCloseFunc(func(err error) error {
			after.Add(1)
			return err
		}),
	)
	w := m.Writer()
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if ok, err := m.TryClose(); !ok || err != sentinel {
		t.Fatalf("first TryClose = (%v, %v), want (true, %v)", ok, err, sentinel)
	}
	if ok, err := m.TryClose(); !ok || err != nil {
		t.Fatalf("second TryClose = (%v, %v), want (true, nil)", ok, err)
	}
	if n := before.Load(); n != 1 {
		t.Errorf("before-close hook ran %d times, want 1", n)
	}
	if n := after.Load(); n != 1 {
		t.Errorf("after-close hook ran %d times, want 1", n)
	}
	if n := fb.closeCalls.Load(); n != 1 {
		t.Errorf("Buffer.Close called %d times, want 1", n)
	}
	if _, err := m.NewReader(0); !errors.Is(err, ErrClosedPipe) {
		t.Errorf("NewReader after disposal: err = %v, want ErrClosedPipe", err)
	}
}

func TestReaderConstructorValidation(t *testing.T) {
	m := NewSWMR(nil)
	w := m.Writer()
	defer w.Close()
	r, err := m.NewReader(0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	using := m.ReaderUsing()

	check := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: err = nil, want validation error", name)
		} else if errors.Is(err, ErrClosedPipe) {
			t.Errorf("%s: err = ErrClosedPipe, want validation error", name)
		}
	}
	_, err = m.NewReader(-1)
	check("NewReader(-1)", err)
	_, err = m.NewReadSeeker(-1, 1)
	check("NewReadSeeker(-1, 1)", err)
	_, err = m.NewReadSeeker(0, -1)
	check("NewReadSeeker(0, -1)", err)

	if n := m.ReaderUsing(); n != using {
		t.Errorf("ReaderUsing() = %d after rejected constructors, want %d", n, using)
	}
}

func TestShortReadIsUnexpectedEOF(t *testing.T) {
	fb := &fakeBuffer{
		readAt: func(p []byte, off int64) (int, error) { return 0, io.EOF },
	}
	m := NewSWMR(fb)
	w := m.Writer()
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := m.NewReader(0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	n, err := r.Read(make([]byte, 8))
	if n != 0 || err != io.ErrUnexpectedEOF {
		t.Fatalf("Read = (%d, %v), want (0, ErrUnexpectedEOF)", n, err)
	}
}

func TestReadAfterTerminalEOF(t *testing.T) {
	custom := errors.New("custom close error")
	cases := []struct {
		name     string
		closeErr error
		want     error
	}{
		{"EOF", nil, io.EOF},
		{"custom", custom, custom},
	}
	for _, tc := range cases {
		t.Run("late joiner/"+tc.name, func(t *testing.T) {
			m := NewSWMR(nil)
			w := m.Writer()
			if _, err := w.Write([]byte("ab")); err != nil {
				t.Fatal(err)
			}
			if err := w.CloseWithError(tc.closeErr); err != nil {
				t.Fatal(err)
			}
			r, err := m.NewReader(0)
			if err != nil {
				t.Fatal(err)
			}
			checkReadAfterTerminal(t, r, tc.want)
			if ok, err := m.TryClose(); !ok || err != nil {
				t.Fatalf("TryClose = (%v, %v), want (true, nil)", ok, err)
			}
		})
		// The reader self-releases at EOF and autoClose disposes the buffer underneath it.
		t.Run("early reader autoClose/"+tc.name, func(t *testing.T) {
			m := NewSWMR(nil, WithAutoClose())
			w := m.Writer()
			r, err := m.NewReader(0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte("ab")); err != nil {
				t.Fatal(err)
			}
			if err := w.CloseWithError(tc.closeErr); err != nil {
				t.Fatal(err)
			}
			checkReadAfterTerminal(t, r, tc.want)
			if ok, err := m.TryClose(); !ok || err != nil {
				t.Fatalf("TryClose = (%v, %v), want (true, nil)", ok, err)
			}
		})
	}
}

func checkReadAfterTerminal(t *testing.T, r io.Reader, want error) {
	t.Helper()
	got, err := io.ReadAll(r)
	wantReadAll := want
	if wantReadAll == io.EOF {
		wantReadAll = nil
	}
	if err != wantReadAll {
		t.Fatalf("ReadAll err = %v, want %v", err, wantReadAll)
	}
	if string(got) != "ab" {
		t.Fatalf("ReadAll = %q, want %q", got, "ab")
	}
	for i := 0; i < 2; i++ {
		n, err := r.Read(make([]byte, 4))
		if n != 0 || err != want {
			t.Fatalf("Read #%d after terminal = (%d, %v), want (0, %v)", i, n, err, want)
		}
	}
}

// A failed spill must leave the memory tier readable through the SWMR: the pre-rework buffer
// installed the temp file before backfilling it, so readers saw an empty stream and io.EOF.
func TestSpillFailureKeepsStreamReadable(t *testing.T) {
	roCreateTemp := func() (*os.File, error) {
		f, err := os.CreateTemp(t.TempDir(), "swmr-ro-")
		if err != nil {
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		return os.Open(f.Name())
	}

	m := NewSWMR(NewMemoryOrTemporaryFileBuffer(make([]byte, 0, 8), roCreateTemp))
	defer mustTryClose(t, m)
	w := m.Writer()
	writeString(t, w, "abc")

	n, writeErr := w.Write([]byte("0123456789"))
	if writeErr == nil || n != 0 {
		t.Fatalf("Write past capacity with a failing spill = (%d, %v), want (0, error)", n, writeErr)
	}
	if m.Length() != 3 {
		t.Fatalf("Length() = %d after the failed spill, want 3", m.Length())
	}
	if err := w.CloseWithError(writeErr); err != nil {
		t.Fatal(err)
	}

	r, err := m.NewReader(0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if string(got) != "abc" || !errors.Is(err, writeErr) {
		t.Fatalf("ReadAll = (%q, %v), want (\"abc\", %v)", got, err, writeErr)
	}
}
