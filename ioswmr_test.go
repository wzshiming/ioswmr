package ioswmr

import (
	"bytes"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemory(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		testBaseCase(t, nil)
		testClose(t, nil)
		testConcurrentReads(t, nil)
		testConcurrentReadSeekers(t, nil)
		testReadSeeker(t, nil)
		testReadSeekerIncompleteWrite(t, nil)
		testReadSeekerBeyondWritten(t, nil)
	}()

	select {
	case <-time.After(time.Second * 10):
		t.Fatal("timeout")
	case <-done:
	}
}

func TestTemporaryFile(t *testing.T) {
	f := NewTemporaryFileBuffer(nil)
	defer func() {
		_ = f.Close()
	}()

	reset := func() {
		f.Close()
		f = NewTemporaryFileBuffer(nil)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		testBaseCase(t, f)
		reset()
		testClose(t, f)
		reset()
		testConcurrentReads(t, f)
		reset()
		testConcurrentReadSeekers(t, f)
		reset()
		testReadSeeker(t, f)
		reset()
		testReadSeekerIncompleteWrite(t, f)
		reset()
		testReadSeekerBeyondWritten(t, f)
	}()

	select {
	case <-time.After(time.Second * 10):
		t.Fatal("timeout")
	case <-done:
	}
}

func TestMemoryOrTemporaryFile(t *testing.T) {
	f := NewMemoryOrTemporaryFileBuffer(nil, nil)
	defer func() {
		_ = f.Close()
	}()

	reset := func() {
		f.Close()
		f = NewMemoryOrTemporaryFileBuffer(nil, nil)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		testBaseCase(t, f)
		reset()
		testClose(t, f)
		reset()
		testConcurrentReads(t, f)
		reset()
		testConcurrentReadSeekers(t, f)
		reset()
		testReadSeeker(t, f)
		reset()
		testReadSeekerIncompleteWrite(t, f)
		reset()
		testReadSeekerBeyondWritten(t, f)
	}()

	select {
	case <-time.After(time.Second * 10):
		t.Fatal("timeout")
	case <-done:
	}
}

func testBaseCase(t *testing.T, buf Buffer) {
	m := NewSWMR(buf)
	defer func() {
		ok, err := m.TryClose()
		if err != nil {
			t.Errorf("TryClose failed: %s", err)
		}

		if !ok {
			t.Errorf("TryClose failed: ReaderUsing() == %d, WriteDone() == %v", m.ReaderUsing(), m.WriteDone())
		}
	}()
	var times atomic.Uint32

	bufs := [][]byte{}
	var mut sync.Mutex

	testFunc := func(mark string) {
		buf := make([]byte, 12)
		mut.Lock()
		bufs = append(bufs, buf)
		mut.Unlock()

		reader := m.NewReader(0)
		defer reader.Close()
		n, err := io.ReadFull(reader, buf)
		if err != nil {
			t.Errorf("on %q: %s", mark, err)
		}

		got := string(buf[:n])

		if got != "Hello World!" {
			t.Errorf("on %q: Expected \"Hello World!\", got %q", mark, got)
		}

		t.Logf("on %q: %s", mark, got)
		times.Add(1)
	}

	data := [][]byte{
		[]byte("Hello"),
		[]byte(" "),
		[]byte("World"),
		[]byte("!"),
	}

	w := m.Writer()
	for _, d := range data {
		for i := 0; i != 3; i++ {
			go testFunc("before " + string(d))
		}

		time.Sleep(10 * time.Millisecond)
		if _, err := w.Write(d); err != nil {
			t.Fatal(err)
		}
	}

	time.Sleep(10 * time.Millisecond)
	for i := 0; i != 3; i++ {
		go testFunc("before close")
	}

	w.Close()
	for i := 0; i != 3; i++ {
		go testFunc("closed")
	}

	time.Sleep(10 * time.Millisecond)

	for times.Load() != 18 {
		time.Sleep(10 * time.Millisecond)
	}
	if m.ReaderUsing() != 0 {
		t.Errorf("Expected ReaderUsing() to be 0 after all readers are done, got %d", m.ReaderUsing())
	}
}

func testClose(t *testing.T, buf Buffer) {
	m := NewSWMR(buf)
	defer func() {
		ok, err := m.TryClose()
		if err != nil {
			t.Errorf("TryClose failed: %s", err)
		}

		if !ok {
			t.Errorf("TryClose failed: ReaderUsing() == %d, WriteDone() == %v", m.ReaderUsing(), m.WriteDone())
		}
	}()

	w := m.Writer()
	defer w.Close()

	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %s", err)
	}

	_, err := w.Write([]byte("Data after close"))
	if err != ErrClosedPipe {
		t.Errorf("Expected ErrClosedPipe, got %v", err)
	}
}

func testConcurrentReads(t *testing.T, buf Buffer) {
	m := NewSWMR(buf)
	defer func() {
		ok, err := m.TryClose()
		if err != nil {
			t.Errorf("TryClose failed: %s", err)
		}

		if !ok {
			t.Errorf("TryClose failed: ReaderUsing() == %d, WriteDone() == %v", m.ReaderUsing(), m.WriteDone())
		}
	}()

	w := m.Writer()
	defer w.Close()

	data := bytes.Repeat([]byte("Concurrent Read Data! "), 1024*100)

	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			readBuf := make([]byte, len(data))
			reader := m.NewReader(0)
			defer reader.Close()
			n, err := io.ReadFull(reader, readBuf)
			if err != nil && err != io.EOF {
				t.Errorf("Concurrent read failed: %s", err)
			}
			if n != len(data) {
				t.Errorf("Expected to read %d bytes, got %d", len(data), n)
			}
			if string(readBuf) != string(data) {
				t.Errorf("Expected %q, got %q", data, readBuf)
			}
		}()
	}

	_, err := w.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %s", err)
	}

	wg.Wait()

	if m.ReaderUsing() != 0 {
		t.Errorf("Expected ReaderUsing() to be 0 after all readers are done, got %d", m.ReaderUsing())
	}
}

func testConcurrentReadSeekers(t *testing.T, buf Buffer) {
	m := NewSWMR(buf)
	defer func() {
		ok, err := m.TryClose()
		if err != nil {
			t.Errorf("TryClose failed: %s", err)
		}

		if !ok {
			t.Errorf("TryClose failed: ReaderUsing() == %d, WriteDone() == %v", m.ReaderUsing(), m.WriteDone())
		}
	}()

	w := m.Writer()
	defer w.Close()

	data := bytes.Repeat([]byte("ReadSeeker Concurrent! "), 1024*10)

	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rs := m.NewReadSeeker(0, len(data))
			defer rs.Close()
			readBuf := make([]byte, len(data))
			n, err := io.ReadFull(rs, readBuf)
			if err != nil {
				t.Errorf("Concurrent ReadSeeker read failed: %s", err)
				return
			}
			if n != len(data) {
				t.Errorf("Expected to read %d bytes, got %d", len(data), n)
			}
			if string(readBuf) != string(data) {
				t.Errorf("Data mismatch in concurrent ReadSeeker")
			}

			// Seek back and re-read to verify seek under concurrency
			pos, err := rs.Seek(0, io.SeekStart)
			if err != nil {
				t.Errorf("Seek failed: %s", err)
				return
			}
			if pos != 0 {
				t.Errorf("Expected position 0, got %d", pos)
			}
			n, err = io.ReadFull(rs, readBuf)
			if err != nil {
				t.Errorf("Concurrent ReadSeeker re-read failed: %s", err)
				return
			}
			if n != len(data) {
				t.Errorf("Expected to re-read %d bytes, got %d", len(data), n)
			}
			if string(readBuf) != string(data) {
				t.Errorf("Data mismatch in concurrent ReadSeeker re-read")
			}
		}()
	}

	_, err := w.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %s", err)
	}

	wg.Wait()
}

func testReadSeeker(t *testing.T, buf Buffer) {
	m := NewSWMR(buf)
	defer func() {
		ok, err := m.TryClose()
		if err != nil {
			t.Errorf("TryClose failed: %s", err)
		}

		if !ok {
			t.Errorf("TryClose failed: ReaderUsing() == %d, WriteDone() == %v", m.ReaderUsing(), m.WriteDone())
		}
	}()

	w := m.Writer()

	data := []byte("Hello World!")
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	w.Close()

	rs := m.NewReadSeeker(0, len(data))
	defer rs.Close()

	// Read first 5 bytes
	buf1 := make([]byte, 5)
	n, err := rs.Read(buf1)
	if err != nil {
		t.Fatalf("Read failed: %s", err)
	}
	if string(buf1[:n]) != "Hello" {
		t.Fatalf("Expected \"Hello\", got %q", buf1[:n])
	}

	// Seek back to start
	pos, err := rs.Seek(0, io.SeekStart)
	if err != nil {
		t.Fatalf("Seek failed: %s", err)
	}
	if pos != 0 {
		t.Fatalf("Expected position 0, got %d", pos)
	}

	// Read all from start
	buf2 := make([]byte, 12)
	n, err = rs.Read(buf2)
	if err != nil {
		t.Fatalf("Read failed: %s", err)
	}
	if string(buf2[:n]) != "Hello World!" {
		t.Fatalf("Expected \"Hello World!\", got %q", buf2[:n])
	}

	// Seek to offset 6 from start
	pos, err = rs.Seek(6, io.SeekStart)
	if err != nil {
		t.Fatalf("Seek failed: %s", err)
	}
	if pos != 6 {
		t.Fatalf("Expected position 6, got %d", pos)
	}

	buf3 := make([]byte, 6)
	n, err = rs.Read(buf3)
	if err != nil {
		t.Fatalf("Read failed: %s", err)
	}
	if string(buf3[:n]) != "World!" {
		t.Fatalf("Expected \"World!\", got %q", buf3[:n])
	}

	// Seek relative to current (-6)
	pos, err = rs.Seek(-6, io.SeekCurrent)
	if err != nil {
		t.Fatalf("Seek failed: %s", err)
	}
	if pos != 6 {
		t.Fatalf("Expected position 6, got %d", pos)
	}

	buf4 := make([]byte, 6)
	n, err = rs.Read(buf4)
	if err != nil {
		t.Fatalf("Read failed: %s", err)
	}
	if string(buf4[:n]) != "World!" {
		t.Fatalf("Expected \"World!\", got %q", buf4[:n])
	}

	// Seek relative to end
	pos, err = rs.Seek(-6, io.SeekEnd)
	if err != nil {
		t.Fatalf("Seek failed: %s", err)
	}
	if pos != 6 {
		t.Fatalf("Expected position 6, got %d", pos)
	}

	buf5 := make([]byte, 6)
	n, err = rs.Read(buf5)
	if err != nil {
		t.Fatalf("Read failed: %s", err)
	}
	if string(buf5[:n]) != "World!" {
		t.Fatalf("Expected \"World!\", got %q", buf5[:n])
	}

	// Seek to negative position should fail
	_, err = rs.Seek(-100, io.SeekStart)
	if err == nil {
		t.Fatal("Expected error for negative position")
	}
}

func testReadSeekerIncompleteWrite(t *testing.T, buf Buffer) {
	m := NewSWMR(buf)
	defer func() {
		ok, err := m.TryClose()
		if err != nil {
			t.Errorf("TryClose failed: %s", err)
		}

		if !ok {
			t.Errorf("TryClose failed: ReaderUsing() == %d, WriteDone() == %v", m.ReaderUsing(), m.WriteDone())
		}
	}()

	w := m.Writer()

	// Create a ReadSeeker for 12 bytes before all data is written
	rs := m.NewReadSeeker(0, 12)
	defer rs.Close()

	// Write partial data
	if _, err := w.Write([]byte("Hello")); err != nil {
		t.Fatal(err)
	}

	// Read should get partial data
	buf1 := make([]byte, 5)
	n, err := rs.Read(buf1)
	if err != nil {
		t.Fatalf("Read of partial data failed: %s", err)
	}
	if string(buf1[:n]) != "Hello" {
		t.Fatalf("Expected \"Hello\", got %q", buf1[:n])
	}

	// Write remaining data and close
	go func() {
		time.Sleep(10 * time.Millisecond)
		w.Write([]byte(" World!"))
		w.Close()
	}()

	// Read remaining data
	buf2 := make([]byte, 7)
	n, err = io.ReadFull(rs, buf2)
	if err != nil {
		t.Fatalf("Read of remaining data failed: %s", err)
	}
	if string(buf2[:n]) != " World!" {
		t.Fatalf("Expected \" World!\", got %q", buf2[:n])
	}

	// Seek back to start and read all
	rs.Seek(0, io.SeekStart)
	buf3 := make([]byte, 12)
	n, err = rs.Read(buf3)
	if err != nil {
		t.Fatalf("Read after seek failed: %s", err)
	}
	if string(buf3[:n]) != "Hello World!" {
		t.Fatalf("Expected \"Hello World!\", got %q", buf3[:n])
	}
}

func testReadSeekerBeyondWritten(t *testing.T, buf Buffer) {
	m := NewSWMR(buf)
	defer func() {
		ok, err := m.TryClose()
		if err != nil {
			t.Errorf("TryClose failed: %s", err)
		}

		if !ok {
			t.Errorf("TryClose failed: ReaderUsing() == %d, WriteDone() == %v", m.ReaderUsing(), m.WriteDone())
		}
	}()

	w := m.Writer()

	data := []byte("Hello World!")
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	w.Close()

	// Create a ReadSeeker with length larger than written data
	rs := m.NewReadSeeker(0, len(data)+100)
	defer rs.Close()

	readBuf := make([]byte, len(data)+100)
	n, err := rs.Read(readBuf)
	if err != nil {
		t.Fatalf("Read failed: %s", err)
	}
	// Should only read the written bytes, not stale data
	if n != len(data) {
		t.Fatalf("Expected to read %d bytes, got %d", len(data), n)
	}
	if string(readBuf[:n]) != string(data) {
		t.Fatalf("Expected %q, got %q", data, readBuf[:n])
	}

	// Further reads should return ErrUnexpectedEOF since we are beyond the written data
	_, err = rs.Read(readBuf)
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("Expected ErrUnexpectedEOF, got %s", err)
	}
}

func TestFileBuffer(t *testing.T) {
	// Create a temporary file and write initial data
	tmpFile, err := os.CreateTemp("", "test-file-buffer-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	data := []byte("Hello World!")
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		t.Fatal(err)
	}

	// Seek to end for appending
	if _, err := tmpFile.Seek(0, io.SeekEnd); err != nil {
		tmpFile.Close()
		t.Fatal(err)
	}

	buf := NewFileBuffer(tmpFile)

	// Verify ReadAt works for existing data
	readBuf := make([]byte, len(data))
	n, err := buf.ReadAt(readBuf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %s", err)
	}
	if string(readBuf[:n]) != string(data) {
		t.Fatalf("Expected %q, got %q", data, readBuf[:n])
	}

	// Verify Write appends
	more := []byte(" More data!")
	n, err = buf.Write(more)
	if err != nil {
		t.Fatalf("Write failed: %s", err)
	}
	if n != len(more) {
		t.Fatalf("Expected to write %d bytes, got %d", len(more), n)
	}

	// Read all data
	allData := append(data, more...)
	allBuf := make([]byte, len(allData))
	n, err = buf.ReadAt(allBuf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %s", err)
	}
	if string(allBuf[:n]) != string(allData) {
		t.Fatalf("Expected %q, got %q", allData, allBuf[:n])
	}

	if err := buf.Close(); err != nil {
		t.Fatalf("Close failed: %s", err)
	}
}

func TestFileBufferNil(t *testing.T) {
	// NewFileBuffer(nil) should behave like NewTemporaryFileBuffer(nil)
	buf := NewFileBuffer(nil)
	defer buf.Close()

	data := []byte("test data")
	n, err := buf.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %s", err)
	}
	if n != len(data) {
		t.Fatalf("Expected to write %d bytes, got %d", len(data), n)
	}

	readBuf := make([]byte, len(data))
	n, err = buf.ReadAt(readBuf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %s", err)
	}
	if string(readBuf[:n]) != string(data) {
		t.Fatalf("Expected %q, got %q", data, readBuf[:n])
	}
}

func TestRecoverFromFileCache(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)

		// Simulate writing data to a file (e.g., from a previous run)
		tmpFile, err := os.CreateTemp("", "test-recover-")
		if err != nil {
			t.Errorf("CreateTemp failed: %s", err)
			return
		}
		defer os.Remove(tmpFile.Name())

		initialData := []byte("Hello")
		if _, err := tmpFile.Write(initialData); err != nil {
			tmpFile.Close()
			t.Errorf("Write failed: %s", err)
			return
		}

		// Seek to end for appending
		if _, err := tmpFile.Seek(0, io.SeekEnd); err != nil {
			tmpFile.Close()
			t.Errorf("Seek failed: %s", err)
			return
		}

		// Create SWMR with recovery from file cache
		buf := NewFileBuffer(tmpFile)
		m := NewSWMR(buf, WithRecover(len(initialData)))
		defer func() {
			ok, err := m.TryClose()
			if err != nil {
				t.Errorf("TryClose failed: %s", err)
			}
			if !ok {
				t.Errorf("TryClose failed: ReaderUsing() == %d, WriteDone() == %v", m.ReaderUsing(), m.WriteDone())
			}
		}()

		// Verify Length reflects recovered data
		if m.Length() != len(initialData) {
			t.Errorf("Expected Length() == %d, got %d", len(initialData), m.Length())
			return
		}

		// Reader should be able to read existing data immediately
		reader := m.NewReader(0)
		readBuf := make([]byte, len(initialData))
		n, err := io.ReadFull(reader, readBuf)
		if err != nil {
			t.Errorf("Read of recovered data failed: %s", err)
			reader.Close()
			return
		}
		if string(readBuf[:n]) != string(initialData) {
			t.Errorf("Expected %q, got %q", initialData, readBuf[:n])
			reader.Close()
			return
		}

		// Writer continues writing more data
		w := m.Writer()
		moreData := []byte(" World!")
		go func() {
			time.Sleep(10 * time.Millisecond)
			w.Write(moreData)
			w.Close()
		}()

		// Reader should receive the new data after the recovered data
		moreBuf := make([]byte, len(moreData))
		n, err = io.ReadFull(reader, moreBuf)
		if err != nil {
			t.Errorf("Read of new data failed: %s", err)
			reader.Close()
			return
		}
		if string(moreBuf[:n]) != string(moreData) {
			t.Errorf("Expected %q, got %q", moreData, moreBuf[:n])
		}
		reader.Close()
	}()

	select {
	case <-time.After(time.Second * 10):
		t.Fatal("timeout")
	case <-done:
	}
}

func TestRecoverFromFileCacheConcurrentReads(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)

		// Create a file with existing data
		tmpFile, err := os.CreateTemp("", "test-recover-concurrent-")
		if err != nil {
			t.Errorf("CreateTemp failed: %s", err)
			return
		}
		defer os.Remove(tmpFile.Name())

		initialData := bytes.Repeat([]byte("Recovered Data! "), 1024*10)
		if _, err := tmpFile.Write(initialData); err != nil {
			tmpFile.Close()
			t.Errorf("Write failed: %s", err)
			return
		}

		if _, err := tmpFile.Seek(0, io.SeekEnd); err != nil {
			tmpFile.Close()
			t.Errorf("Seek failed: %s", err)
			return
		}

		buf := NewFileBuffer(tmpFile)
		m := NewSWMR(buf, WithRecover(len(initialData)))
		defer func() {
			ok, err := m.TryClose()
			if err != nil {
				t.Errorf("TryClose failed: %s", err)
			}
			if !ok {
				t.Errorf("TryClose failed: ReaderUsing() == %d, WriteDone() == %v", m.ReaderUsing(), m.WriteDone())
			}
		}()

		w := m.Writer()

		moreData := bytes.Repeat([]byte("New Data! "), 1024*10)
		allData := append(initialData, moreData...)

		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				readBuf := make([]byte, len(allData))
				reader := m.NewReader(0)
				defer reader.Close()
				n, err := io.ReadFull(reader, readBuf)
				if err != nil && err != io.EOF {
					t.Errorf("Concurrent read failed: %s", err)
				}
				if n != len(allData) {
					t.Errorf("Expected to read %d bytes, got %d", len(allData), n)
				}
				if string(readBuf[:n]) != string(allData) {
					t.Errorf("Data mismatch in concurrent read")
				}
			}()
		}

		_, err = w.Write(moreData)
		if err != nil {
			t.Errorf("Write failed: %s", err)
			return
		}
		w.Close()

		wg.Wait()

		if m.ReaderUsing() != 0 {
			t.Errorf("Expected ReaderUsing() to be 0, got %d", m.ReaderUsing())
		}
	}()

	select {
	case <-time.After(time.Second * 10):
		t.Fatal("timeout")
	case <-done:
	}
}

func TestRecoverFromFileCacheReadSeeker(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)

		// Create a file with existing data
		tmpFile, err := os.CreateTemp("", "test-recover-seeker-")
		if err != nil {
			t.Errorf("CreateTemp failed: %s", err)
			return
		}
		defer os.Remove(tmpFile.Name())

		data := []byte("Hello World!")
		if _, err := tmpFile.Write(data); err != nil {
			tmpFile.Close()
			t.Errorf("Write failed: %s", err)
			return
		}

		if _, err := tmpFile.Seek(0, io.SeekEnd); err != nil {
			tmpFile.Close()
			t.Errorf("Seek failed: %s", err)
			return
		}

		buf := NewFileBuffer(tmpFile)
		m := NewSWMR(buf, WithRecover(len(data)))
		defer func() {
			ok, err := m.TryClose()
			if err != nil {
				t.Errorf("TryClose failed: %s", err)
			}
			if !ok {
				t.Errorf("TryClose failed: ReaderUsing() == %d, WriteDone() == %v", m.ReaderUsing(), m.WriteDone())
			}
		}()

		// Close writer immediately since all data is recovered
		w := m.Writer()
		w.Close()

		// Use ReadSeeker on recovered data
		rs := m.NewReadSeeker(0, len(data))
		defer rs.Close()

		// Read first 5 bytes
		buf1 := make([]byte, 5)
		n, err := rs.Read(buf1)
		if err != nil {
			t.Errorf("Read failed: %s", err)
			return
		}
		if string(buf1[:n]) != "Hello" {
			t.Errorf("Expected \"Hello\", got %q", buf1[:n])
			return
		}

		// Seek back to start
		pos, err := rs.Seek(0, io.SeekStart)
		if err != nil {
			t.Errorf("Seek failed: %s", err)
			return
		}
		if pos != 0 {
			t.Errorf("Expected position 0, got %d", pos)
			return
		}

		// Read all
		buf2 := make([]byte, len(data))
		n, err = rs.Read(buf2)
		if err != nil {
			t.Errorf("Read failed: %s", err)
			return
		}
		if string(buf2[:n]) != string(data) {
			t.Errorf("Expected %q, got %q", data, buf2[:n])
		}
	}()

	select {
	case <-time.After(time.Second * 10):
		t.Fatal("timeout")
	case <-done:
	}
}

func TestRecoverWithZeroLength(t *testing.T) {
	// WithRecover(0) should not change behavior
	m := NewSWMR(nil, WithRecover(0))
	defer func() {
		ok, err := m.TryClose()
		if err != nil {
			t.Errorf("TryClose failed: %s", err)
		}
		if !ok {
			t.Errorf("TryClose failed")
		}
	}()

	if m.Length() != 0 {
		t.Fatalf("Expected Length() == 0, got %d", m.Length())
	}

	w := m.Writer()
	data := []byte("test")
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	w.Close()

	reader := m.NewReader(0)
	defer reader.Close()
	buf := make([]byte, len(data))
	n, err := io.ReadFull(reader, buf)
	if err != nil {
		t.Fatalf("Read failed: %s", err)
	}
	if string(buf[:n]) != string(data) {
		t.Fatalf("Expected %q, got %q", data, buf[:n])
	}
}
