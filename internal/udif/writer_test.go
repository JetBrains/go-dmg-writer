package udif

import (
	"bytes"
	"compress/zlib"
	"hash/crc32"
	"io"
	"testing"
	"time"
)

// fakeSrc generates n bytes following a known pattern for round-trip tests.
type fakeSrc struct{ buf []byte; off int }

func newFakeSrc(n int) *fakeSrc {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return &fakeSrc{buf: b}
}

func (f *fakeSrc) Read(p []byte) (int, error) {
	if f.off >= len(f.buf) {
		return 0, io.EOF
	}
	n := copy(p, f.buf[f.off:])
	f.off += n
	return n, nil
}

func TestWriteUDROSingleChunk(t *testing.T) {
	const size = 4 * SectorSize // 2 KiB
	src := newFakeSrc(size)
	var out bytes.Buffer
	if err := Write(&out, src, size, Options{
		VolumeName:   "test",
		Compression:  CompressionNone,
		ChunkSectors: 4,
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if out.Len() <= KolySize {
		t.Fatalf("output too small: %d bytes", out.Len())
	}

	// Parse trailer.
	tail := out.Bytes()[len(out.Bytes())-KolySize:]
	koly, err := DecodeKoly(tail)
	if err != nil {
		t.Fatalf("DecodeKoly: %v", err)
	}
	if koly.Signature != KolySignature {
		t.Errorf("signature: got %#x", koly.Signature)
	}
	if koly.DataForkLength != size {
		t.Errorf("DataForkLength: got %d want %d", koly.DataForkLength, size)
	}
	if koly.SectorCount != size/SectorSize {
		t.Errorf("SectorCount: got %d want %d", koly.SectorCount, size/SectorSize)
	}
	want := crc32.ChecksumIEEE(newFakeSrc(size).buf)
	if koly.DataForkChecksum.Data[0] != want {
		t.Errorf("DataForkChecksum: got %#x want %#x", koly.DataForkChecksum.Data[0], want)
	}
}

func TestWriteUDZOMultiChunk(t *testing.T) {
	const size = 4 * 1024 * 1024 // 4 MiB → 4 chunks of 1 MiB
	src := newFakeSrc(size)
	var out bytes.Buffer
	if err := Write(&out, src, size, Options{
		VolumeName:   "compressed",
		Compression:  CompressionZlib,
		ChunkSectors: 2048,
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	tail := out.Bytes()[len(out.Bytes())-KolySize:]
	koly, err := DecodeKoly(tail)
	if err != nil {
		t.Fatalf("DecodeKoly: %v", err)
	}
	if koly.SectorCount != size/SectorSize {
		t.Errorf("SectorCount: got %d", koly.SectorCount)
	}
	if koly.DataForkLength >= size {
		t.Errorf("DataForkLength %d not smaller than uncompressed %d; zlib did not compress", koly.DataForkLength, size)
	}
}

func TestWriteUDZOAllZeroBecomesSparse(t *testing.T) {
	const size = 2 * 1024 * 1024
	src := bytes.NewReader(make([]byte, size))
	var out bytes.Buffer
	if err := Write(&out, src, size, Options{
		Compression:  CompressionZlib,
		ChunkSectors: 2048,
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	tail := out.Bytes()[len(out.Bytes())-KolySize:]
	koly, err := DecodeKoly(tail)
	if err != nil {
		t.Fatalf("DecodeKoly: %v", err)
	}
	// All-zero source should produce a data fork much smaller than the
	// original — every chunk should be BLOCK_ZERO with CompLength=0.
	if koly.DataForkLength != 0 {
		t.Errorf("DataForkLength: got %d want 0 (all chunks should be sparse)", koly.DataForkLength)
	}
}

func TestWriteDeterministic(t *testing.T) {
	makeOne := func() []byte {
		src := newFakeSrc(64 * 1024)
		var b bytes.Buffer
		_ = Write(&b, src, 64*1024, Options{
			VolumeName:   "deterministic",
			Compression:  CompressionZlib,
			ChunkSectors: 64,
			Time:         time.Unix(1700000000, 0),
		})
		return b.Bytes()
	}
	a := makeOne()
	b := makeOne()
	if !bytes.Equal(a, b) {
		t.Fatalf("output not deterministic across runs (lengths %d vs %d)", len(a), len(b))
	}
}

func TestIsAllZero(t *testing.T) {
	if !isAllZero(make([]byte, 1024)) {
		t.Error("isAllZero(zeros) returned false")
	}
	buf := make([]byte, 1024)
	buf[500] = 1
	if isAllZero(buf) {
		t.Error("isAllZero(non-zero) returned true")
	}
}

func TestZlibRoundTrip(t *testing.T) {
	orig := newFakeSrc(8192).buf
	var compressed bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&compressed, zlib.DefaultCompression)
	zw.Write(orig)
	zw.Close()

	zr, err := zlib.NewReader(&compressed)
	if err != nil {
		t.Fatalf("zlib.NewReader: %v", err)
	}
	defer zr.Close()
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, orig) {
		t.Fatalf("zlib round-trip mismatch")
	}
}
