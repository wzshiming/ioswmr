package ioswmr

import (
	"io"
	"os"
	"sync"
)

// Buffer is an interface that represents a buffer.
type Buffer interface {
	io.Writer
	io.ReaderAt
	io.Closer
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

// NewMemoryBuffer returns a new memory buffer.
// If buf is nil, it will use a pooled buffer. Otherwise, it will use the provided buffer.
func NewMemoryBuffer(buf []byte) Buffer {
	var isPooled bool
	if buf != nil {
		buf = buf[:0]
	} else {
		buf = *pool.Get().(*[]byte)
		isPooled = true
	}
	return &memory{
		buf:      buf,
		isPooled: isPooled,
	}
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

func (m *memory) Close() error {
	if m.isPooled {
		pool.Put(&m.buf)
		m.buf = nil
	}
	return nil
}

func createTemporaryFile() (*os.File, error) {
	return os.CreateTemp("", "swmr-")
}

type temporaryFile struct {
	file       *os.File
	createTemp func() (*os.File, error)
}

// NewTemporaryFileBuffer returns a new temporary file buffer.
// If createTemp is nil, it will use the default createTemporaryFile function.
func NewTemporaryFileBuffer(createTemp func() (*os.File, error)) Buffer {
	if createTemp == nil {
		createTemp = createTemporaryFile
	}
	return &temporaryFile{
		createTemp: createTemp,
	}
}

func (m *temporaryFile) Write(p []byte) (n int, err error) {
	if m.file == nil {
		tmpFile, err := m.createTemp()
		if err != nil {
			return 0, err
		}
		m.file = tmpFile
	}
	return m.file.Write(p)
}

func (m *temporaryFile) ReadAt(p []byte, off int64) (n int, err error) {
	if m.file == nil {
		return 0, io.EOF
	}
	return m.file.ReadAt(p, off)
}

func (m *temporaryFile) Close() error {
	if m.file == nil {
		return nil
	}
	err := m.file.Close()
	if err != nil {
		return err
	}
	return os.Remove(m.file.Name())
}

type memoryOrTemporaryFile struct {
	buf        []byte
	isPooled   bool
	tempFile   *os.File
	createTemp func() (*os.File, error)
}

// NewMemoryOrTemporaryFileBuffer returns a new buffer that uses memory for small writes and switches to a temporary file when the data exceeds the capacity of the memory buffer.
// If createTemp is nil, it will use the default createTemporaryFile function.
func NewMemoryOrTemporaryFileBuffer(buf []byte, createTemp func() (*os.File, error)) Buffer {
	var isPooled bool
	if buf != nil {
		buf = buf[:0]
	} else {
		buf = *pool.Get().(*[]byte)
		isPooled = true
	}
	if createTemp == nil {
		createTemp = createTemporaryFile
	}
	return &memoryOrTemporaryFile{
		buf:        buf,
		isPooled:   isPooled,
		createTemp: createTemp,
	}
}

func (m *memoryOrTemporaryFile) Write(p []byte) (n int, err error) {
	if m.tempFile != nil {
		return m.tempFile.Write(p)
	}

	if len(m.buf)+len(p) <= cap(m.buf) {
		m.buf = append(m.buf, p...)
		return len(p), nil
	}

	tempFile, err := m.createTemp()
	if err != nil {
		return 0, err
	}
	m.tempFile = tempFile

	_, err = tempFile.Write(m.buf)
	if err != nil {
		return 0, err
	}

	m.buf = nil
	if m.isPooled {
		pool.Put(&m.buf)
		m.isPooled = false
	}

	return m.tempFile.Write(p)
}

func (m *memoryOrTemporaryFile) ReadAt(p []byte, off int64) (n int, err error) {
	if m.tempFile != nil {
		return m.tempFile.ReadAt(p, off)
	}

	if off >= int64(len(m.buf)) {
		return 0, io.EOF
	}

	n = copy(p, m.buf[off:])
	return n, nil
}

func (m *memoryOrTemporaryFile) Close() error {
	if m.buf != nil {
		if m.isPooled {
			pool.Put(&m.buf)
			m.isPooled = false
		}
		m.buf = nil
	}
	if m.tempFile != nil {
		err := m.tempFile.Close()
		if err != nil {
			return err
		}
		err = os.Remove(m.tempFile.Name())
		if err != nil {
			return err
		}
		m.tempFile = nil
	}
	return nil
}

type fileBuffer struct {
	file *os.File
}

// NewFileBuffer creates a buffer from an existing file.
// Unlike NewTemporaryFileBuffer, the file is not created or deleted by the buffer.
// The caller is responsible for managing the file's lifecycle and positioning the
// file offset for writing (e.g., seeking to the end for appending).
// This is useful for recovering progress from a file cache.
func NewFileBuffer(file *os.File) Buffer {
	if file == nil {
		return NewTemporaryFileBuffer(nil)
	}
	return &fileBuffer{file: file}
}

func (f *fileBuffer) Write(p []byte) (int, error) {
	return f.file.Write(p)
}

func (f *fileBuffer) ReadAt(p []byte, off int64) (int, error) {
	return f.file.ReadAt(p, off)
}

func (f *fileBuffer) Close() error {
	return f.file.Close()
}
