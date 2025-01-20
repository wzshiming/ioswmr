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

	done := make(chan struct{})
	go func() {
		defer close(done)
		testBaseCase(t, f)
		f.Seek(0, 0)
		testClose(t, f)
		f.Seek(0, 0)
		testConcurrentReads(t, f)
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
