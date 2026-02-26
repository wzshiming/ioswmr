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

func TestFile(t *testing.T) {
	f, err := os.CreateTemp("", "ioswmr_test")
	if err != nil {
		t.Fatal(err)
	}

	defer os.Remove(f.Name())

	reset := func() {
		f.Seek(0, 0)
		f.Truncate(0)
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
	var times atomic.Uint32

	bufs := [][]byte{}

	testFunc := func(mark string) {
		buf := make([]byte, 12)
		bufs = append(bufs, buf)

		n, err := io.ReadFull(m.NewReader(), buf)
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

	for _, d := range data {
		for i := 0; i != 3; i++ {
			go testFunc("before " + string(d))
		}

		time.Sleep(10 * time.Millisecond)
		if _, err := m.Write(d); err != nil {
			t.Fatal(err)
		}
	}

	time.Sleep(10 * time.Millisecond)
	for i := 0; i != 3; i++ {
		go testFunc("before close")
	}

	m.Close()
	for i := 0; i != 3; i++ {
		go testFunc("closed")
	}

	time.Sleep(10 * time.Millisecond)

	for times.Load() != 18 {
		time.Sleep(10 * time.Millisecond)
	}
}

func testClose(t *testing.T, buf Buffer) {
	m := NewSWMR(buf)
	defer m.Close()

	if err := m.Close(); err != nil {
		t.Fatalf("Close failed: %s", err)
	}

	_, err := m.Write([]byte("Data after close"))
	if err != io.ErrClosedPipe {
		t.Errorf("Expected ErrClosedPipe, got %v", err)
	}
}

func testConcurrentReads(t *testing.T, buf Buffer) {
	m := NewSWMR(buf)
	defer m.Close()

	data := bytes.Repeat([]byte("Concurrent Read Data! "), 1024*100)

	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			readBuf := make([]byte, len(data))
			n, err := io.ReadFull(m.NewReader(), readBuf)
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

	_, err := m.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %s", err)
	}

	wg.Wait()
}

func testConcurrentReadSeekers(t *testing.T, buf Buffer) {
	m := NewSWMR(buf)
	defer m.Close()

	data := bytes.Repeat([]byte("ReadSeeker Concurrent! "), 1024*10)

	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rs := m.NewReadSeeker(0, len(data))
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

	_, err := m.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %s", err)
	}

	wg.Wait()
}

func testReadSeeker(t *testing.T, buf Buffer) {
	m := NewSWMR(buf)

	data := []byte("Hello World!")
	if _, err := m.Write(data); err != nil {
		t.Fatal(err)
	}
	m.Close()

	rs := m.NewReadSeeker(0, len(data))

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

	// Create a ReadSeeker for 12 bytes before all data is written
	rs := m.NewReadSeeker(0, 12)

	// Write partial data
	if _, err := m.Write([]byte("Hello")); err != nil {
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
		m.Write([]byte(" World!"))
		m.Close()
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

	data := []byte("Hello World!")
	if _, err := m.Write(data); err != nil {
		t.Fatal(err)
	}
	m.Close()

	// Create a ReadSeeker with length larger than written data
	rs := m.NewReadSeeker(0, len(data)+100)
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
