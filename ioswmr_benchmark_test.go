package ioswmr

import (
	"testing"
)

func benchmarkSWMR(b *testing.B, buf Buffer) {
	swmr := NewSWMR(buf, WithAutoClose())

	reader1, err := swmr.NewReader(0)
	if err != nil {
		b.Fatal(err)
	}
	defer reader1.Close()

	reader2, err := swmr.NewReader(0)
	if err != nil {
		b.Fatal(err)
	}
	defer reader2.Close()

	w := swmr.Writer()
	defer w.Close()
	for i := 0; i < b.N; i++ {
		data := []byte("test data")
		if _, err := w.Write(data); err != nil {
			b.Fatalf("Write failed: %v", err)
		}
		result := make([]byte, len(data))
		if n, err := reader1.Read(result); err != nil || n != len(data) {
			b.Fatalf("reader1 Read = (%d, %v), want (%d, nil)", n, err, len(data))
		}
		if n, err := reader2.Read(result); err != nil || n != len(data) {
			b.Fatalf("reader2 Read = (%d, %v), want (%d, nil)", n, err, len(data))
		}
	}
}

func BenchmarkSWMR(b *testing.B) {
	buf := NewMemoryBuffer(nil)
	defer buf.Close()

	benchmarkSWMR(b, buf)
}

func BenchmarkSWMRWithBuffer(b *testing.B) {
	buf := NewTemporaryFileBuffer(nil)
	defer buf.Close()

	benchmarkSWMR(b, buf)
}

func BenchmarkSWMRWithMemoryOrTemporaryFileBuffer(b *testing.B) {
	buf := NewMemoryOrTemporaryFileBuffer(make([]byte, 0, 1024*1024), nil)
	defer buf.Close()

	benchmarkSWMR(b, buf)
}
