package ioswmr

import (
	"bytes"
	"io"
	"os"
	"sync/atomic"
)

// SWMR is a single-writer-multiple-reader interface
// that allows for a single writer and multiple readers to access the same stream.
type SWMR interface {
	io.Writer
	io.Closer
	NewReader() io.Reader
}

// Buffer is an interface that represents a buffer.
type Buffer interface {
	io.Writer
	io.ReaderAt
}

type swmr struct {
	buf      Buffer
	isClosed atomic.Bool

	length int

	ch chan struct{}
}

// NewSWMR returns a new SWMR with a buffer.
// If the buffer is nil, it will use the memory buffer.
func NewSWMR(buf Buffer) SWMR {
	if buf == nil {
		buf = &memory{}
	} else {
		switch t := buf.(type) {
		case *os.File:
			buf = &file{f: t}
		}
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

	n, err = m.buf.Write(p)
	m.length += n
	m.targetNotify()
	return n, err
}

func (m *swmr) Len() int {
	return m.length
}

func (m *swmr) Close() error {
	if !m.isClosed.Swap(true) {
		close(m.ch)
	}
	return nil
}

func (m *swmr) targetNotify() {
	for {
		select {
		case m.ch <- struct{}{}:
		default:
			return
		}
	}
}

func (m *swmr) NewReader() io.Reader {
	return &reader{
		swmr: m,
	}
}

type reader struct {
	swmr *swmr
	off  int
}

func (m *reader) Read(p []byte) (n int, err error) {
	if m.off >= m.swmr.Len() {
		_, ok := <-m.swmr.ch
		if !ok {
			if m.off >= m.swmr.Len() {
				return 0, io.EOF
			}
		}
	}

	n, err = m.swmr.buf.ReadAt(p, int64(m.off))
	if err != nil {
		if err == io.EOF {
			if n != 0 {
				err = nil
			} else {
				_, ok := <-m.swmr.ch
				if !ok && m.off < m.swmr.Len() {
					n, err = m.swmr.buf.ReadAt(p, int64(m.off))
				}
			}
		}
	}
	m.off += n
	return n, err
}

type memory struct {
	buf bytes.Buffer
}

func (m *memory) Write(p []byte) (n int, err error) {
	return m.buf.Write(p)
}

func (m *memory) ReadAt(p []byte, off int64) (n int, err error) {
	buf := m.buf.Bytes()
	if off >= int64(len(buf)) {
		return 0, io.EOF
	}

	n = copy(p, buf[off:])
	return n, nil
}

type file struct {
	f *os.File
}

func (f *file) Write(p []byte) (n int, err error) {
	n, err = f.f.Write(p)
	if err != nil {
		return n, err
	}
	err = f.f.Sync()
	if err != nil {
		return n, err
	}
	return n, nil
}

func (f *file) ReadAt(p []byte, off int64) (n int, err error) {
	r, err := os.Open(f.f.Name())
	if err != nil {
		return 0, err
	}
	defer r.Close()

	n, err = r.ReadAt(p, off)
	if err != nil {
		if err == io.EOF && n != 0 {
			err = nil
		} else {
			return n, err
		}
	}
	return n, nil
}
