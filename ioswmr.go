package ioswmr

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

// SWMR is a single-writer-multiple-reader interface
// that allows for a single writer and multiple readers to access the same stream.
type SWMR interface {
	Buffer
	io.Closer
	Length() int
	Using() int
	NewReader(offset int) io.ReadCloser
	NewReadSeeker(offset int, length int) io.ReadSeekCloser
}

// Buffer is an interface that represents a buffer.
type Buffer interface {
	io.Writer
	io.ReaderAt
}

type swmr struct {
	mut      sync.RWMutex
	buf      Buffer
	isClosed atomic.Bool
	length   int
	using    atomic.Int64

	ch chan struct{}
}

// NewSWMR returns a new SWMR with a buffer.
// If the buffer is nil, it will use the memory buffer.
func NewSWMR(buf Buffer) SWMR {
	if buf == nil {
		buf = &memory{}
	}

	m := &swmr{
		buf: buf,
		ch:  make(chan struct{}),
	}

	return m
}

func (m *swmr) Write(p []byte) (n int, err error) {
	if m.isClosed.Load() {
		return 0, io.ErrClosedPipe
	}
	if len(p) == 0 {
		return 0, nil
	}

	m.mut.Lock()
	n, err = m.buf.Write(p)
	if n > 0 {
		m.length += n
	}
	m.mut.Unlock()

	if n > 0 {
		m.targetNotify()
	}
	return n, err
}

func (m *swmr) ReadAt(p []byte, off int64) (n int, err error) {
	m.mut.RLock()
	defer m.mut.RUnlock()
	return m.buf.ReadAt(p, off)
}

func (m *swmr) Length() int {
	m.mut.RLock()
	defer m.mut.RUnlock()
	return m.length
}

func (m *swmr) Using() int {
	return int(m.using.Load())
}

func (m *swmr) Close() error {
	if m.isClosed.Swap(true) {
		return io.ErrClosedPipe
	}
	close(m.ch)
	return nil
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
	_ = m.using.Add(-1)
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

type memory struct {
	buf []byte
}

func (m *memory) Write(p []byte) (n int, err error) {
	m.buf = append(m.buf, p...)
	return len(p), nil
}

func (m *memory) ReadAt(p []byte, off int64) (n int, err error) {
	if off >= int64(len(m.buf)) {
		return 0, io.EOF
	}

	n = copy(p, m.buf[off:])
	return n, nil
}
