package ioswmr

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

var ErrClosedPipe = io.ErrClosedPipe

// SWMR is a single-writer-multiple-reader interface
// that allows for a single writer and multiple readers to access the same stream.
type SWMR interface {
	// Writer returns a WriteCloser that can be used to write to the stream.
	Writer() io.WriteCloser
	// Length returns the current length of the stream.
	Length() int
	// WriteDone returns true if the writer has closed the stream, false otherwise.
	WriteDone() bool
	// NewReader returns a ReadCloser that can be used to read from the stream starting at the given offset.
	NewReader(offset int) io.ReadCloser
	// NewReadSeeker returns a ReadSeekCloser that can be used to read from the stream starting at the given offset and with the given length.
	NewReadSeeker(offset int, length int) io.ReadSeekCloser
	// ReaderUsing returns the number of readers currently using the stream.
	ReaderUsing() int
	// TryClose attempts to close the stream if there are no active readers and the writer has closed the stream.
	// It returns true if the stream was successfully closed, false if it was not closed due to active readers or an open writer, and an error if an error occurred during closing.
	TryClose() (bool, error)
}

type swmr struct {
	mut             sync.RWMutex
	buf             Buffer
	isClosed        atomic.Bool
	length          int
	using           atomic.Int64
	autoClose       bool
	beforeCloseFunc func()
	afterCloseFunc  func(err error) error

	ch chan struct{}
}

type Option func(*swmr)

// WithAutoClose configures the SWMR to automatically close the buffer when the writer is closed and there are no active readers.
// This option is useful for ensuring that resources are released promptly without requiring explicit calls to TryClose.
func WithAutoClose() Option {
	return func(m *swmr) {
		m.autoClose = true
	}
}

// WithBeforeCloseFunc sets a function to be called before the buffer is closed.
func WithBeforeCloseFunc(f func()) Option {
	return func(m *swmr) {
		m.beforeCloseFunc = f
	}
}

// WithAfterCloseFunc sets a function to be called after the buffer is closed, allowing for error handling or cleanup based on the result of the close operation.
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
		buf: buf,
		ch:  make(chan struct{}),
	}

	for _, opt := range opts {
		opt(m)
	}

	return m
}

func (m *swmr) Writer() io.WriteCloser {
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
	if m.ReaderUsing() != 0 {
		return false, nil
	}
	if !m.WriteDone() {
		return false, nil
	}

	m.mut.Lock()
	defer m.mut.Unlock()
	if m.buf == nil {
		return true, nil
	}

	if m.beforeCloseFunc != nil {
		m.beforeCloseFunc()
	}
	err := m.buf.Close()
	if m.afterCloseFunc != nil {
		err = m.afterCloseFunc(err)
	}
	if err != nil {
		return true, err
	}
	m.buf = nil
	return true, nil
}

func (m *swmr) targetNotify() {
	for i := 0; i < m.Length(); i++ {
		select {
		case m.ch <- struct{}{}:
		default:
			return
		}
	}
}

func (m *swmr) acquire() {
	_ = m.using.Add(1)
}

func (m *swmr) release() {
	if m.using.Add(-1) == 0 {
		if m.autoClose && m.WriteDone() {
			m.TryClose()
		}
	}
}

func (m *swmr) NewReader(offset int) io.ReadCloser {
	m.acquire()
	return &reader{
		swmr: m,
		off:  offset,
	}
}

func (m *swmr) NewReadSeeker(offset int, length int) io.ReadSeekCloser {
	m.acquire()
	return &readSeeker{
		swmr:   m,
		off:    offset,
		length: length,
	}
}

type writer struct {
	swmr *swmr
}

func (w *writer) Write(p []byte) (n int, err error) {
	if w.swmr.isClosed.Load() {
		return 0, ErrClosedPipe
	}
	if len(p) == 0 {
		return 0, nil
	}

	w.swmr.mut.Lock()
	n, err = w.swmr.buf.Write(p)
	if n > 0 {
		w.swmr.length += n
	}
	w.swmr.mut.Unlock()

	if n > 0 {
		w.swmr.targetNotify()
	}
	return n, err
}

func (w *writer) Close() error {
	if w.swmr.isClosed.Swap(true) {
		return ErrClosedPipe
	}
	close(w.swmr.ch)

	if w.swmr.autoClose && w.swmr.ReaderUsing() == 0 {
		w.swmr.TryClose()
	}
	return nil
}

type reader struct {
	swmr *swmr
	off  int
}

func (m *reader) Read(p []byte) (n int, err error) {
	for m.off >= m.swmr.Length() {
		_, ok := <-m.swmr.ch
		if !ok {
			if m.off >= m.swmr.Length() {
				return 0, io.EOF
			}
			break
		}
	}

	n, err = m.swmr.ReadAt(p, int64(m.off))
	if err == io.EOF {
		if n != 0 {
			err = nil
		}
	}
	m.off += n
	return n, err
}

func (m *reader) Close() error {
	m.swmr.release()
	return nil
}

type readSeeker struct {
	swmr   *swmr
	off    int
	length int
}

func (m *readSeeker) Read(p []byte) (n int, err error) {
	if m.off >= m.length {
		return 0, io.EOF
	}

	for m.off >= m.swmr.Length() {
		_, ok := <-m.swmr.ch
		if !ok {
			if m.off >= m.swmr.Length() {
				return 0, io.ErrUnexpectedEOF
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

func (m *readSeeker) Close() error {
	m.swmr.release()
	return nil
}
