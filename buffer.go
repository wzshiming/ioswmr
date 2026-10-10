package ioswmr

import (
	"errors"
	"io"
	"os"
	"sync"
)

// Buffer is an interface that represents a buffer.
// Seek reports positions relative to the buffer's current size and must accept Seek(0, io.SeekEnd),
// returning that size (0 for a fresh buffer); the SWMR uses it to learn how much data a buffer
// already holds when its Writer resumes. The buffers in this package are append-only: the memory
// buffers reject any other target with ErrUnsupportedSeek, the temporary file delegates to *os.File,
// opening it on the first Write or Seek.
type Buffer interface {
	io.Writer
	io.ReaderAt
	io.Closer
	io.Seeker
}

// appendOnlySeek resolves a seek on an append-only buffer of size end, where the write position is
// always end; the only valid target is end itself.
func appendOnlySeek(end, offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent, io.SeekEnd:
		target = end + offset
	default:
		return 0, os.ErrInvalid
	}
	if target < 0 {
		return 0, os.ErrInvalid
	}
	if target != end {
		return 0, ErrUnsupportedSeek
	}
	return end, nil
}

type memory struct {
	buf      []byte
	isPooled bool
}

var (
	pool = &sync.Pool{
		New: func() any {
			b := make([]byte, 0, 32*1024)
			return &b
		},
	}
)

func newMemory(buf []byte) memory {
	if buf != nil {
		return memory{buf: buf[:0]}
	}
	return memory{
		buf:      (*pool.Get().(*[]byte))[:0],
		isPooled: true,
	}
}

// NewMemoryBuffer returns a new memory buffer.
// If buf is nil, it will use a pooled buffer. Otherwise, it will use the provided buffer.
func NewMemoryBuffer(buf []byte) Buffer {
	m := newMemory(buf)
	return &m
}

func (m *memory) Write(p []byte) (n int, err error) {
	m.buf = append(m.buf, p...)
	return len(p), nil
}

func (m *memory) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, errNegativeOffset
	}
	if off >= int64(len(m.buf)) {
		return 0, io.EOF
	}

	n = copy(p, m.buf[off:])
	if n < len(p) {
		err = io.EOF
	}
	return n, err
}

func (m *memory) Seek(offset int64, whence int) (int64, error) {
	return appendOnlySeek(int64(len(m.buf)), offset, whence)
}

// release drops the contents; a pooled backing array goes back to the pool exactly once.
func (m *memory) release() {
	if m.isPooled {
		m.isPooled = false
		// The pool must not alias m.buf: the field is cleared below while a Get may read the item.
		buf := m.buf[:0]
		pool.Put(&buf)
	}
	m.buf = nil
}

func (m *memory) Close() error {
	m.release()
	return nil
}

func createTemporaryFile() (*os.File, error) {
	return os.CreateTemp("", "swmr-")
}

// closeAndRemove releases a temporary file, reporting both failures if both happen.
func closeAndRemove(f *os.File) error {
	return errors.Join(f.Close(), os.Remove(f.Name()))
}

type temporaryFile struct {
	file       *os.File
	createTemp func() (*os.File, error)
}

// NewTemporaryFileBuffer returns a new temporary file buffer.
// If createTemp is nil, it will use the default createTemporaryFile function.
// createTemp may return an existing file: Seek(0, io.SeekEnd) reports its contents, Close still removes it.
func NewTemporaryFileBuffer(createTemp func() (*os.File, error)) Buffer {
	if createTemp == nil {
		createTemp = createTemporaryFile
	}
	return &temporaryFile{
		createTemp: createTemp,
	}
}

func (m *temporaryFile) open() error {
	if m.file != nil {
		return nil
	}
	f, err := m.createTemp()
	if err != nil {
		return err
	}
	m.file = f
	return nil
}

func (m *temporaryFile) Write(p []byte) (n int, err error) {
	if err := m.open(); err != nil {
		return 0, err
	}
	return m.file.Write(p)
}

func (m *temporaryFile) ReadAt(p []byte, off int64) (n int, err error) {
	if m.file == nil {
		return 0, io.EOF
	}
	return m.file.ReadAt(p, off)
}

func (m *temporaryFile) Seek(offset int64, whence int) (int64, error) {
	if err := m.open(); err != nil {
		return 0, err
	}
	return m.file.Seek(offset, whence)
}

func (m *temporaryFile) Close() error {
	if m.file == nil {
		return nil
	}
	f := m.file
	m.file = nil
	return closeAndRemove(f)
}

type memoryOrTemporaryFile struct {
	memory
	tempFile   *os.File
	createTemp func() (*os.File, error)
}

// NewMemoryOrTemporaryFileBuffer returns a new buffer that uses memory for small writes and switches to a temporary file when the data exceeds the capacity of the memory buffer.
// If createTemp is nil, it will use the default createTemporaryFile function.
func NewMemoryOrTemporaryFileBuffer(buf []byte, createTemp func() (*os.File, error)) Buffer {
	if createTemp == nil {
		createTemp = createTemporaryFile
	}
	return &memoryOrTemporaryFile{
		memory:     newMemory(buf),
		createTemp: createTemp,
	}
}

func (m *memoryOrTemporaryFile) Write(p []byte) (n int, err error) {
	if m.tempFile != nil {
		return m.tempFile.Write(p)
	}

	if len(m.buf)+len(p) <= cap(m.buf) {
		return m.memory.Write(p)
	}

	if err := m.spill(); err != nil {
		return 0, err
	}
	return m.tempFile.Write(p)
}

// spill moves the contents into a new temporary file; on failure the memory tier is left intact.
func (m *memoryOrTemporaryFile) spill() error {
	f, err := m.createTemp()
	if err != nil {
		return err
	}
	if _, err := f.Write(m.buf); err != nil {
		return errors.Join(err, closeAndRemove(f))
	}
	m.tempFile = f
	m.release()
	return nil
}

func (m *memoryOrTemporaryFile) ReadAt(p []byte, off int64) (n int, err error) {
	if m.tempFile != nil {
		return m.tempFile.ReadAt(p, off)
	}
	return m.memory.ReadAt(p, off)
}

func (m *memoryOrTemporaryFile) Seek(offset int64, whence int) (int64, error) {
	if m.tempFile != nil {
		return m.tempFile.Seek(offset, whence)
	}
	return m.memory.Seek(offset, whence)
}

func (m *memoryOrTemporaryFile) Close() error {
	m.release()
	if m.tempFile == nil {
		return nil
	}
	f := m.tempFile
	m.tempFile = nil
	return closeAndRemove(f)
}
