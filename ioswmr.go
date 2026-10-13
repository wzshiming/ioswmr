package ioswmr

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

var (
	ErrClosedPipe = io.ErrClosedPipe

	// ErrUnsupportedSeek wraps errors.ErrUnsupported; Writer.Seek returns it for anything but a resume
	// (see Writer), the append-only memory buffers for any target other than their current end.
	ErrUnsupportedSeek = fmt.Errorf("ioswmr: seek: %w", errors.ErrUnsupported)

	errNegativeOffset = errors.New("ioswmr: negative offset")
	errNegativeLength = errors.New("ioswmr: negative length")
)

// Writer is an interface that represents a writer that can be closed with an error.
type Writer interface {
	io.Writer
	// Seek resumes a stream over a Buffer that already holds data. Before the first Write it accepts
	// the buffer's current size as its target (Seek(0, io.SeekEnd) or an equivalent form), publishes
	// those bytes to readers by setting Length, and returns the size. Any other target, or any Seek
	// after a Write, fails with ErrUnsupportedSeek and leaves Length unchanged.
	io.Seeker
	io.Closer
	// CloseWithError closes the writer with the given error. If err is nil, it will be treated as io.EOF.
	// A normal close (io.EOF) runs the WithBeforeLastByteFunc hook before returning.
	// The hook's error reaches only the readers; Close or CloseWithError still returns nil.
	CloseWithError(err error) error
}

// SWMR is a single-writer-multiple-reader interface
// that allows for a single writer and multiple readers to access the same stream.
type SWMR interface {
	// Writer returns a Writer for the stream; every call returns a handle on the same single writer.
	// Close or CloseWithError ends the stream for all readers, Seek only resumes (see Writer).
	Writer() Writer
	// Length returns the current length of the stream.
	Length() int
	// WriteDone returns true if the writer has closed the stream, false otherwise.
	WriteDone() bool
	// NewReader returns a ReadCloser over the stream from offset. Reads block until data arrives or the
	// writer closes and end with the writer's error (io.EOF for Close), at which point the reader lets go
	// of the stream; Close lets go early, wakes a blocked Read with ErrClosedPipe, and is idempotent.
	// A negative offset is an error; once the buffer has been released NewReader returns ErrClosedPipe.
	// WithBeforeLastByteFunc makes Reads hold back the newest byte and gates the last one on its hook.
	NewReader(offset int) (io.ReadCloser, error)
	// NewReadSeeker returns a ReadSeekCloser over the first length bytes of the stream from offset, e.g.
	// for http.ServeContent. Reads block like NewReader's and return io.EOF at length; if the writer ends
	// the stream short of length they return its error, or io.ErrUnexpectedEOF when it closed normally.
	// Unlike NewReader the buffer stays alive until Close so the reader can rewind. A negative offset or
	// length is an error; once the buffer has been released NewReadSeeker returns ErrClosedPipe.
	// WithBeforeLastByteFunc gates these Reads like NewReader's; a length that stops short of the
	// stream's last byte never waits for its hook.
	NewReadSeeker(offset int, length int) (io.ReadSeekCloser, error)
	// ReaderUsing returns the number of admitted readers still holding the stream: a NewReader until it
	// returns its terminal error or is closed, a NewReadSeeker until it is closed, plus the
	// WithBeforeLastByteFunc hook's hold while it runs.
	ReaderUsing() int
	// TryClose releases the buffer if the stream is closed and nothing holds it (ReaderUsing() == 0).
	// It returns false if the stream is still in use, and true once the buffer has been released;
	// the error from the Buffer's Close and the after-close hook is reported by the call that released it,
	// later calls return (true, nil).
	TryClose() (bool, error)
}

type swmr struct {
	mut             sync.RWMutex
	buf             Buffer
	isClosed        atomic.Bool
	err             error
	length          int
	written         bool
	using           atomic.Int64
	autoClose       bool
	beforeCloseFunc func()
	afterCloseFunc  func(err error) error
	beforeLastByte  func() error

	chMut     sync.Mutex
	readerChs map[chan struct{}]struct{}
}

type Option func(*swmr)

// WithAutoClose releases the buffer as soon as the writer has closed and no reader holds the stream
// (the TryClose condition), from whichever call completes it; the close error is then only
// observable through WithAfterCloseFunc.
func WithAutoClose() Option {
	return func(m *swmr) {
		m.autoClose = true
	}
}

// WithBeforeCloseFunc sets a hook that runs exactly once, after the stream has been marked released
// (new readers already get ErrClosedPipe) and before Buffer.Close. It runs outside the internal lock
// and may call back into the SWMR.
func WithBeforeCloseFunc(f func()) Option {
	return func(m *swmr) {
		m.beforeCloseFunc = f
	}
}

// WithAfterCloseFunc sets a hook that runs exactly once with the error from Buffer.Close, outside the
// internal lock; the error it returns replaces that error in the TryClose result.
func WithAfterCloseFunc(f func(err error) error) Option {
	return func(m *swmr) {
		m.afterCloseFunc = f
	}
}

// WithBeforeLastByteFunc gates the stream's last byte on f: readers hold back the newest byte while
// the writer is open, and after a normal close (io.EOF) every reader reaching the last byte gets it
// only if f returns nil, and f's error otherwise. r reads the whole stream, last byte included, and
// is valid only while f runs. f runs exactly once per normally closed stream, empty ones included,
// outside the internal locks: synchronously within the writer's Close, or within the first Read to
// reach the last byte if that starts it first; the other waits for the same result, so Close returns
// only once f has. The stream stays held while f runs, which ReaderUsing counts. If the writer closes
// with another error, f never runs and readers get that error in place of the last byte. Closing a
// reader does not interrupt f; its Read returns ErrClosedPipe once f returns. f must not read this
// stream's last byte through NewReader or NewReadSeeker, as that would wait for its own result.
// The writer's Close returns nil even if f fails. A nil f is ignored.
func WithBeforeLastByteFunc(f func(r io.ReadSeeker) error) Option {
	return func(m *swmr) {
		if f != nil {
			m.beforeLastByte = sync.OnceValue(func() error {
				return f(io.NewSectionReader(m, 0, int64(m.Length())))
			})
		}
	}
}

// NewSWMR returns a new SWMR with a buffer.
// If the buffer is nil, it will use the memory buffer.
func NewSWMR(buf Buffer, opts ...Option) SWMR {
	if buf == nil {
		buf = NewMemoryBuffer(nil)
	}

	m := &swmr{
		buf:       buf,
		readerChs: make(map[chan struct{}]struct{}),
	}

	for _, opt := range opts {
		opt(m)
	}

	return m
}

func (m *swmr) Writer() Writer {
	return &writer{
		swmr: m,
	}
}

func (m *swmr) ReadAt(p []byte, off int64) (n int, err error) {
	m.mut.RLock()
	defer m.mut.RUnlock()
	if m.buf == nil {
		return 0, ErrClosedPipe
	}
	return m.buf.ReadAt(p, off)
}

func (m *swmr) Length() int {
	m.mut.RLock()
	defer m.mut.RUnlock()
	return m.length
}

// available reports how many bytes a reader at off may read now, last when that is the gated last byte.
func (m *swmr) available(off int) (avail int, done bool, last bool) {
	// Loaded before Length: once the writer has closed, Length is final and m.err is safe to read.
	done = m.isClosed.Load()
	avail = m.Length() - off
	if m.beforeLastByte != nil {
		if done && avail == 1 {
			return avail, done, true
		}
		avail--
	}
	return max(avail, 0), done, false
}

func (m *swmr) ReaderUsing() int {
	return int(m.using.Load())
}

func (m *swmr) WriteDone() bool {
	return m.isClosed.Load()
}

func (m *swmr) TryClose() (bool, error) {
	m.mut.Lock()
	if m.buf == nil {
		m.mut.Unlock()
		return true, nil
	}
	// isClosed first: CloseWithError takes the hook's hold before publishing isClosed.
	if !m.isClosed.Load() || m.using.Load() != 0 {
		m.mut.Unlock()
		return false, nil
	}
	buf := m.buf
	m.buf = nil
	m.mut.Unlock()

	// Hooks run outside the lock so they may call back into the SWMR.
	if m.beforeCloseFunc != nil {
		m.beforeCloseFunc()
	}
	err := buf.Close()
	if m.afterCloseFunc != nil {
		err = m.afterCloseFunc(err)
	}
	return true, err
}

func (m *swmr) write(p []byte) (int, error) {
	m.mut.Lock()
	defer m.mut.Unlock()
	if m.buf == nil || m.isClosed.Load() {
		return 0, ErrClosedPipe
	}
	n, err := m.buf.Write(p)
	if n > 0 {
		m.length += n
		m.written = true
	}
	return n, err
}

// seek resumes at the buffer's current size; only Seek(0, io.SeekEnd) is ever asked of the Buffer.
func (m *swmr) seek(offset int64, whence int) (int64, error) {
	m.mut.Lock()
	defer m.mut.Unlock()
	if m.buf == nil || m.isClosed.Load() {
		return 0, ErrClosedPipe
	}
	if m.written {
		return 0, ErrUnsupportedSeek
	}
	size, err := m.buf.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	n, err := appendOnlySeek(size, offset, whence)
	if err != nil {
		// Negative targets and bad whence are just as unsupported as a non-resume target here.
		return 0, ErrUnsupportedSeek
	}
	m.length = int(n)
	return n, nil
}

func (m *swmr) targetNotify() {
	m.chMut.Lock()
	defer m.chMut.Unlock()
	for ch := range m.readerChs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (m *swmr) registerReaderCh(ch chan struct{}) {
	m.chMut.Lock()
	defer m.chMut.Unlock()
	if m.isClosed.Load() {
		close(ch)
		return
	}
	m.readerChs[ch] = struct{}{}
}

func (m *swmr) unregisterReaderCh(ch chan struct{}) {
	m.chMut.Lock()
	defer m.chMut.Unlock()
	// Channels leave the map exactly when they are closed, so this never double-closes.
	if _, ok := m.readerChs[ch]; ok {
		delete(m.readerChs, ch)
		close(ch)
	}
}

// admit registers a new reader while the buffer is guaranteed alive, or rejects it once released.
func (m *swmr) admit() (chan struct{}, error) {
	m.mut.RLock()
	defer m.mut.RUnlock()
	if m.buf == nil {
		return nil, ErrClosedPipe
	}
	m.using.Add(1)
	ch := make(chan struct{}, 1)
	m.registerReaderCh(ch)
	return ch, nil
}

func (m *swmr) release() {
	if m.using.Add(-1) == 0 {
		if m.autoClose && m.isClosed.Load() {
			_, _ = m.TryClose()
		}
	}
}

func (m *swmr) NewReader(offset int) (io.ReadCloser, error) {
	if offset < 0 {
		return nil, errNegativeOffset
	}
	ch, err := m.admit()
	if err != nil {
		return nil, err
	}
	return &reader{
		swmr: m,
		off:  offset,
		ch:   ch,
	}, nil
}

func (m *swmr) NewReadSeeker(offset int, length int) (io.ReadSeekCloser, error) {
	if offset < 0 {
		return nil, errNegativeOffset
	}
	if length < 0 {
		return nil, errNegativeLength
	}
	ch, err := m.admit()
	if err != nil {
		return nil, err
	}
	return &readSeeker{
		swmr:   m,
		off:    offset,
		length: length,
		ch:     ch,
	}, nil
}

type writer struct {
	swmr *swmr
}

func (w *writer) Seek(offset int64, whence int) (int64, error) {
	if w.swmr.isClosed.Load() {
		return 0, ErrClosedPipe
	}

	n, err := w.swmr.seek(offset, whence)
	if err != nil {
		return 0, err
	}

	if n > 0 {
		w.swmr.targetNotify()
	}
	return n, nil
}

func (w *writer) Write(p []byte) (n int, err error) {
	if w.swmr.isClosed.Load() {
		return 0, ErrClosedPipe
	}
	if len(p) == 0 {
		return 0, nil
	}

	n, err = w.swmr.write(p)
	if n > 0 {
		w.swmr.targetNotify()
	}
	return n, err
}

func (w *writer) Close() error {
	return w.CloseWithError(nil)
}

func (w *writer) CloseWithError(err error) error {
	if err == nil {
		err = io.EOF
	}

	m := w.swmr
	m.chMut.Lock()
	if m.isClosed.Load() {
		m.chMut.Unlock()
		return ErrClosedPipe
	}
	runHook := err == io.EOF && m.beforeLastByte != nil
	if runHook {
		// Taken before isClosed is published, so it covers the hook whichever goroutine runs it.
		m.using.Add(1)
	}
	// err is stored before isClosed, so any observer of isClosed (lock, closed channel, Load) sees err.
	m.err = err
	m.isClosed.Store(true)
	for ch := range m.readerChs {
		close(ch)
	}
	m.readerChs = make(map[chan struct{}]struct{})
	m.chMut.Unlock()

	if runHook {
		// The hook's error is for the readers reaching the last byte, not for Close.
		_ = m.beforeLastByte()
		m.release()
	}
	if m.autoClose {
		_, _ = m.TryClose()
	}
	return nil
}

type reader struct {
	swmr     *swmr
	off      int
	ch       chan struct{}
	closed   atomic.Bool
	released atomic.Bool
}

func (m *reader) Read(p []byte) (n int, err error) {
	if m.closed.Load() {
		return 0, ErrClosedPipe
	}
	avail, done, last := m.swmr.available(m.off)
	for avail == 0 && !done {
		if _, ok := <-m.ch; !ok && m.closed.Load() {
			return 0, ErrClosedPipe
		}
		avail, done, last = m.swmr.available(m.off)
	}
	if avail == 0 {
		m.release()
		return 0, m.swmr.err
	}
	if last {
		err = m.swmr.err
		if err == io.EOF {
			if len(p) == 0 {
				return 0, nil
			}
			err = m.swmr.beforeLastByte()
			if m.closed.Load() {
				return 0, ErrClosedPipe
			}
		}
		if err != nil {
			m.release()
			return 0, err
		}
	}

	n, err = m.swmr.ReadAt(p[:min(len(p), avail)], int64(m.off))
	if err == io.EOF {
		if n != 0 {
			err = nil
		} else {
			err = io.ErrUnexpectedEOF
		}
	}
	m.off += n
	return n, err
}

func (m *reader) release() {
	if m.released.Swap(true) {
		return
	}
	m.swmr.unregisterReaderCh(m.ch)
	m.swmr.release()
}

func (m *reader) Close() error {
	m.closed.Store(true)
	m.release()
	return nil
}

type readSeeker struct {
	swmr     *swmr
	off      int
	length   int
	ch       chan struct{}
	closed   atomic.Bool
	released atomic.Bool
}

func (m *readSeeker) Read(p []byte) (n int, err error) {
	if m.closed.Load() {
		return 0, ErrClosedPipe
	}
	if m.off >= m.length {
		return 0, io.EOF
	}

	avail, done, last := m.swmr.available(m.off)
	for avail == 0 && !done {
		if _, ok := <-m.ch; !ok && m.closed.Load() {
			return 0, ErrClosedPipe
		}
		avail, done, last = m.swmr.available(m.off)
	}
	if avail == 0 {
		if m.swmr.err == io.EOF {
			return 0, io.ErrUnexpectedEOF
		}
		return 0, m.swmr.err
	}
	if last {
		err = m.swmr.err
		if err == io.EOF {
			if len(p) == 0 {
				return 0, nil
			}
			err = m.swmr.beforeLastByte()
			if m.closed.Load() {
				return 0, ErrClosedPipe
			}
		}
		if err != nil {
			return 0, err
		}
	}

	n, err = m.swmr.ReadAt(p[:min(len(p), avail, m.length-m.off)], int64(m.off))
	if err == io.EOF {
		if n != 0 {
			err = nil
		} else {
			err = io.ErrUnexpectedEOF
		}
	}
	m.off += n
	return n, err
}

func (m *readSeeker) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = int64(m.off) + offset
	case io.SeekEnd:
		newPos = int64(m.length) + offset
	default:
		return int64(m.off), errors.New("ioswmr: invalid whence")
	}
	if newPos < 0 {
		return int64(m.off), errors.New("ioswmr: negative position")
	}
	if newPos > int64(m.length) {
		return int64(m.off), errors.New("ioswmr: position beyond length")
	}
	m.off = int(newPos)
	return newPos, nil
}

func (m *readSeeker) release() {
	if m.released.Swap(true) {
		return
	}
	m.swmr.unregisterReaderCh(m.ch)
	m.swmr.release()
}

func (m *readSeeker) Close() error {
	m.closed.Store(true)
	m.release()
	return nil
}
