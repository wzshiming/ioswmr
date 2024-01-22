package ioswmr

import (
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemory(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		testCase(t, nil)
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
		testCase(t, f)
	}()

	select {
	case <-time.After(time.Second * 10):
		t.Fatal("timeout")
	case <-done:
	}
}

func testCase(t *testing.T, buf Buffer) {
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
