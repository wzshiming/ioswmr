package ioswmr

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// disableGC keeps sync.Pool items alive for the rest of the test.
func disableGC(t *testing.T) {
	t.Helper()
	prev := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(prev) })
}

// tempRecorder creates temp files in a per-test directory and remembers the last name it handed out.
type tempRecorder struct {
	dir  string
	name string
}

func newTempRecorder(t *testing.T) *tempRecorder {
	return &tempRecorder{dir: t.TempDir()}
}

func (r *tempRecorder) create() (*os.File, error) {
	f, err := os.CreateTemp(r.dir, "swmr-")
	if err != nil {
		return nil, err
	}
	r.name = f.Name()
	return f, nil
}

// noSpill returns a createTemp for buffers that are expected to stay in memory.
func noSpill(t *testing.T) func() (*os.File, error) {
	return func() (*os.File, error) {
		t.Error("buffer spilled to a temp file, expected it to stay in memory")
		return nil, errors.New("unexpected spill")
	}
}

func mustWrite(t *testing.T, buf Buffer, data []byte) {
	t.Helper()
	if n, err := buf.Write(data); err != nil || n != len(data) {
		t.Fatalf("Write(%d bytes) = (%d, %v), want (%d, nil)", len(data), n, err, len(data))
	}
}

func assertContent(t *testing.T, buf Buffer, want string) {
	t.Helper()
	p := make([]byte, len(want)+1)
	n, err := buf.ReadAt(p, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt(%d bytes, 0) = (%d, %v)", len(p), n, err)
	}
	if got := string(p[:n]); got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func assertRemoved(t *testing.T, name string) {
	t.Helper()
	if name == "" {
		t.Fatal("no temp file was created")
	}
	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file %s still exists (stat err = %v)", name, err)
	}
}

func TestPooledBufferReuse(t *testing.T) {
	disableGC(t)

	tests := []struct {
		name string
		use  func(t *testing.T)
	}{
		{"memory", func(t *testing.T) {
			buf := NewMemoryBuffer(nil)
			mustWrite(t, buf, []byte("x"))
			if err := buf.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{"memoryOrTemporaryFile", func(t *testing.T) {
			buf := NewMemoryOrTemporaryFileBuffer(nil, nil)
			mustWrite(t, buf, []byte("x"))
			if err := buf.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{"memoryOrTemporaryFile spilled", func(t *testing.T) {
			buf := NewMemoryOrTemporaryFileBuffer(nil, newTempRecorder(t).create)
			mustWrite(t, buf, bytes.Repeat([]byte("x"), cap(buf.(*memoryOrTemporaryFile).buf)+1))
			if err := buf.Close(); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.use(t)

			buf := NewMemoryOrTemporaryFileBuffer(nil, noSpill(t))
			defer buf.Close()
			if _, err := buf.Write([]byte("y")); err != nil {
				t.Fatalf("1-byte write into a fresh pooled buffer after a pooled Close: %v", err)
			}
		})
	}
}

func TestPooledBufferConcurrentLifecycle(t *testing.T) {
	const goroutines, rounds = 4, 300

	dir := t.TempDir()
	var spills atomic.Int32
	countSpills := func() (*os.File, error) {
		spills.Add(1)
		return os.CreateTemp(dir, "swmr-")
	}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			want := bytes.Repeat([]byte{byte('A' + g)}, 64)
			got := make([]byte, len(want))
			for i := 0; i < rounds; i++ {
				bufs := []Buffer{NewMemoryBuffer(nil), NewMemoryOrTemporaryFileBuffer(nil, countSpills)}
				for _, buf := range bufs {
					if _, err := buf.Write(want); err != nil {
						t.Errorf("goroutine %d round %d: Write: %v", g, i, err)
						return
					}
				}
				for _, buf := range bufs {
					if n, err := buf.ReadAt(got, 0); err != nil || n != len(got) {
						t.Errorf("goroutine %d round %d: ReadAt = (%d, %v), want (%d, nil)", g, i, n, err, len(got))
						return
					}
					if !bytes.Equal(got, want) {
						t.Errorf("goroutine %d round %d: read back %q, want %q", g, i, got, want)
						return
					}
				}
				for _, buf := range bufs {
					if err := buf.Close(); err != nil {
						t.Errorf("goroutine %d round %d: Close: %v", g, i, err)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()

	if n := spills.Load(); n != 0 {
		t.Errorf("%d small writes into pooled buffers spilled to temp files", n)
	}
}

func TestSpillFailureKeepsMemory(t *testing.T) {
	var name string
	readOnlyTemp := func() (*os.File, error) {
		f, err := os.CreateTemp(t.TempDir(), "swmr-ro-")
		if err != nil {
			return nil, err
		}
		name = f.Name()
		if err := f.Close(); err != nil {
			return nil, err
		}
		return os.Open(name)
	}

	buf := NewMemoryOrTemporaryFileBuffer(make([]byte, 0, 8), readOnlyTemp)
	mustWrite(t, buf, []byte("abc"))

	if n, err := buf.Write([]byte("0123456789")); err == nil || n != 0 {
		t.Fatalf("Write past capacity with a failing spill = (%d, %v), want (0, error)", n, err)
	}
	assertRemoved(t, name)
	assertContent(t, buf, "abc")

	mustWrite(t, buf, []byte("de"))
	assertContent(t, buf, "abcde")

	if err := buf.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

func TestCloseTwice(t *testing.T) {
	tests := []struct {
		name string
		open func(create func() (*os.File, error)) Buffer
	}{
		{"temporaryFile", func(create func() (*os.File, error)) Buffer {
			return NewTemporaryFileBuffer(create)
		}},
		{"memoryOrTemporaryFile spilled", func(create func() (*os.File, error)) Buffer {
			return NewMemoryOrTemporaryFileBuffer(make([]byte, 0, 2), create)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newTempRecorder(t)
			buf := tt.open(rec.create)
			mustWrite(t, buf, []byte("data"))

			if err := buf.Close(); err != nil {
				t.Fatalf("first Close = %v, want nil", err)
			}
			assertRemoved(t, rec.name)
			if err := buf.Close(); err != nil {
				t.Errorf("second Close = %v, want nil", err)
			}
		})
		t.Run(tt.name+" removed externally", func(t *testing.T) {
			rec := newTempRecorder(t)
			buf := tt.open(rec.create)
			mustWrite(t, buf, []byte("data"))

			if err := os.Remove(rec.name); err != nil {
				t.Fatal(err)
			}
			if err := buf.Close(); err == nil {
				t.Errorf("first Close = nil, want the remove error")
			}
			if err := buf.Close(); err != nil {
				t.Errorf("second Close = %v, want nil", err)
			}
		})
	}

	t.Run("pooled memory", func(t *testing.T) {
		disableGC(t)
		marker := []byte(fmt.Sprintf("swmr-marker-%d", time.Now().UnixNano()))

		buf := NewMemoryBuffer(nil)
		mustWrite(t, buf, marker)
		for i := 1; i <= 2; i++ {
			if err := buf.Close(); err != nil {
				t.Fatalf("Close #%d = %v, want nil", i, err)
			}
		}

		returned := 0
		for i := 0; i < 8; i++ {
			p := pool.Get().(*[]byte)
			if cap(*p) == 0 {
				t.Fatal("pool handed out an empty slice after a double Close")
			}
			if bytes.HasPrefix((*p)[:cap(*p)], marker) {
				returned++
			}
		}
		if returned > 1 {
			t.Fatalf("closed buffer's backing array was returned to the pool %d times", returned)
		}
	})
}

func TestReadAtContract(t *testing.T) {
	tests := []struct {
		name string
		open func(t *testing.T) Buffer
	}{
		{"memory", func(t *testing.T) Buffer {
			return NewMemoryBuffer(nil)
		}},
		{"memoryOrTemporaryFile", func(t *testing.T) Buffer {
			return NewMemoryOrTemporaryFileBuffer(nil, noSpill(t))
		}},
		{"memoryOrTemporaryFile provided buffer", func(t *testing.T) Buffer {
			return NewMemoryOrTemporaryFileBuffer(make([]byte, 0, 32), noSpill(t))
		}},
		{"memoryOrTemporaryFile spilled", func(t *testing.T) Buffer {
			return NewMemoryOrTemporaryFileBuffer(make([]byte, 0, 2), newTempRecorder(t).create)
		}},
		{"temporaryFile", func(t *testing.T) Buffer {
			return NewTemporaryFileBuffer(newTempRecorder(t).create)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := tt.open(t)
			defer buf.Close()
			mustWrite(t, buf, []byte("hello"))

			p := make([]byte, 10)
			if n, err := buf.ReadAt(p, 0); n != 5 || err != io.EOF || string(p[:n]) != "hello" {
				t.Errorf("ReadAt(10 bytes, 0) = (%d, %v) %q, want (5, io.EOF) \"hello\"", n, err, p[:n])
			}
			if n, err := buf.ReadAt(p[:2], 0); n != 2 || err != nil || string(p[:2]) != "he" {
				t.Errorf("ReadAt(2 bytes, 0) = (%d, %v) %q, want (2, nil) \"he\"", n, err, p[:2])
			}
			if n, err := buf.ReadAt(p, 5); n != 0 || err != io.EOF {
				t.Errorf("ReadAt(p, 5) = (%d, %v), want (0, io.EOF)", n, err)
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("ReadAt(p, -1) panicked: %v", r)
					}
				}()
				if n, err := buf.ReadAt(p, -1); n != 0 || err == nil {
					t.Errorf("ReadAt(p, -1) = (%d, %v), want (0, error)", n, err)
				}
			}()
		})
	}
}
