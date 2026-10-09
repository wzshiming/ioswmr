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
	NewReader(offset int) (io.ReadCloser, error)
	// NewReadSeeker returns a ReadSeekCloser over the first length bytes of the stream from offset, e.g.
	// for http.ServeContent. Reads block like NewReader's and return io.EOF at length; if the writer ends
	// the stream short of length they return its error, or io.ErrUnexpectedEOF when it closed normally.
	// Unlike NewReader the buffer stays alive until Close so the reader can rewind. A negative offset or
	// length is an error; once the buffer has been released NewReadSeeker returns ErrClosedPipe.
	NewReadSeeker(offset int, length int) (io.ReadSeekCloser, error)
	// ReaderUsing returns the number of admitted readers still holding the stream: a NewReader until it
	// returns its terminal error or is closed, a NewReadSeeker until it is closed.
	ReaderUsing() int
	// TryClose releases the buffer if the writer has closed the stream and there are no active readers.
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
	if m.using.Load() != 0 || !m.isClosed.Load() {
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
	// Published under chMut: readers observe isClosed only under the same lock or via a closed channel.
	m.err = err
	m.isClosed.Store(true)
	for ch := range m.readerChs {
		close(ch)
	}
	m.readerChs = make(map[chan struct{}]struct{})
	m.chMut.Unlock()

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
	for m.off >= m.swmr.Length() {
		_, ok := <-m.ch
		if !ok {
			if m.closed.Load() {
				return 0, ErrClosedPipe
			}
			if m.off >= m.swmr.Length() {
				m.release()
				return 0, m.swmr.err
			}
			break
		}
	}

	n, err = m.swmr.ReadAt(p, int64(m.off))
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

	for m.off >= m.swmr.Length() {
		_, ok := <-m.ch
		if !ok {
			if m.closed.Load() {
				return 0, ErrClosedPipe
			}
			if m.off >= m.swmr.Length() {
				if m.swmr.err == io.EOF {
					return 0, io.ErrUnexpectedEOF
				}
				return 0, m.swmr.err
			}
			break
		}
	}

	remaining := m.length - m.off
	if len(p) > remaining {
		p = p[:remaining]
	}

	n, err = m.swmr.ReadAt(p, int64(m.off))
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
