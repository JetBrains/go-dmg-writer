package udif

import (
	"bytes"
	"compress/zlib"
	"errors"
	"hash/crc32"
	"io"
	"testing"
	"testing/iotest"
	"time"
)

// fakeSrc generates n bytes following a known pattern for round-trip tests.
type fakeSrc struct {
	buf []byte
	off int
}

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
	// original - every chunk should be BLOCK_ZERO with CompLength=0.
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

func TestBlkxSpecsMapLess(t *testing.T) {
	specs, variant, err := blkxSpecs(nil, "vol", 100)
	if err != nil {
		t.Fatalf("blkxSpecs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("got %d specs, want 1 for a map-less image", len(specs))
	}
	if variant != ImageVariantPartition {
		t.Errorf("ImageVariant: got %d want %d (partition)", variant, ImageVariantPartition)
	}
	want := blkxSpec{name: "vol (Apple_HFSX : 1)", id: 0, descriptor: partitionVariantNumber, sectors: 100}
	if specs[0] != want {
		t.Errorf("spec: got %+v want %+v", specs[0], want)
	}
}

func TestBlkxSpecsRegions(t *testing.T) {
	regions := []Region{
		{Name: "Protective Master Boot Record", Type: "MBR", SectorCount: 1},
		{Name: "", Type: "Apple_Free", SectorCount: 6},
		{Name: "disk image", Type: "Apple_HFS", SectorCount: 93},
	}
	specs, variant, err := blkxSpecs(regions, "vol", 100)
	if err != nil {
		t.Fatalf("blkxSpecs: %v", err)
	}
	if variant != ImageVariantDevice {
		t.Errorf("ImageVariant: got %d want %d (device)", variant, ImageVariantDevice)
	}
	// The name carries the region index, the ID counts from -1, and
	// each region starts where the one in front of it ends.
	want := []blkxSpec{
		{name: "Protective Master Boot Record (MBR : 0)", id: -1, descriptor: 0, firstSector: 0, sectors: 1},
		{name: " (Apple_Free : 1)", id: 0, descriptor: 1, firstSector: 1, sectors: 6},
		{name: "disk image (Apple_HFS : 2)", id: 1, descriptor: 2, firstSector: 7, sectors: 93},
	}
	if len(specs) != len(want) {
		t.Fatalf("got %d specs, want %d", len(specs), len(want))
	}
	for i := range want {
		if specs[i] != want[i] {
			t.Errorf("spec %d: got %+v want %+v", i, specs[i], want[i])
		}
	}
}

// TestBlkxSpecsRejectsBadRegions covers the two ways a caller can hand
// over a region list that does not describe the image it is writing.
// Both have to fail before any bytes reach the output.
func TestBlkxSpecsRejectsBadRegions(t *testing.T) {
	tests := []struct {
		name    string
		regions []Region
		total   uint64
	}{
		{
			name:    "empty region",
			regions: []Region{{Type: "MBR", SectorCount: 1}, {Type: "Apple_Free", SectorCount: 0}},
			total:   1,
		},
		{
			name:    "regions shorter than the image",
			regions: []Region{{Type: "MBR", SectorCount: 1}},
			total:   100,
		},
		{
			name:    "regions longer than the image",
			regions: []Region{{Type: "MBR", SectorCount: 1}, {Type: "Apple_HFS", SectorCount: 200}},
			total:   100,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := blkxSpecs(tt.regions, "vol", tt.total); err == nil {
				t.Error("blkxSpecs accepted a region list it should reject")
			}
		})
	}
}

// failAfterWriter accepts `ok` bytes and then fails every write, which
// is how a full disk, a closed file or a broken pipe behaves.
type failAfterWriter struct {
	ok        int
	written   int
	failWith  error
	failCount int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.written >= w.ok {
		w.failCount++
		return 0, w.failWith
	}
	n := len(p)
	if w.written+n > w.ok {
		n = w.ok - w.written
	}
	w.written += n
	if n < len(p) {
		w.failCount++
		return n, w.failWith
	}
	return n, nil
}

// TestWriteReportsWriteError pins the deadlock that used to hide here.
// The drain goroutine reported the failed write and stopped reading
// `results`, so the workers blocked on their sends, nothing closed
// `results`, and the producer wedged on a full `jobs` channel: a failed
// write hung [Write] instead of returning from it.
//
// Both compression modes are covered because they take different paths
// through the worker pool (zlib runs a real pool, none forces workers=1).
func TestWriteReportsWriteError(t *testing.T) {
	errFull := errors.New("no space left on device")
	modes := []struct {
		name string
		comp Compression
	}{
		{"none", CompressionNone},
		{"zlib", CompressionZlib},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			// 8 MiB of incompressible-ish data through 64 KiB chunks
			// gives the producer far more chunks than the channels
			// buffer, so it is guaranteed to still be dispatching when
			// the write fails.
			const size = 8 * 1024 * 1024
			dst := &failAfterWriter{ok: 4096, failWith: errFull}

			donec := make(chan error, 1)
			go func() {
				donec <- Write(dst, newFakeSrc(size), size, Options{
					VolumeName:   "vol",
					Compression:  m.comp,
					ChunkSectors: 128,
					Workers:      4,
				})
			}()

			select {
			case err := <-donec:
				if err == nil {
					t.Fatal("Write returned nil after the destination failed")
				}
				if !errors.Is(err, errFull) {
					t.Errorf("Write error: got %v, want one wrapping %v", err, errFull)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("Write did not return within 30s after a write error (deadlock)")
			}
		})
	}
}

// TestWriteReportsReadError is the mirror image: the source fails
// mid-stream and Write has to surface that rather than pad the image out.
func TestWriteReportsReadError(t *testing.T) {
	errIO := errors.New("input/output error")
	const size = 1024 * 1024
	src := io.MultiReader(bytes.NewReader(make([]byte, 4096)), iotest.ErrReader(errIO))

	donec := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		donec <- Write(&out, src, size, Options{
			Compression:  CompressionZlib,
			ChunkSectors: 8,
			Workers:      4,
		})
	}()

	select {
	case err := <-donec:
		if err == nil {
			t.Fatal("Write returned nil after the source failed")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Write did not return within 30s after a read error (deadlock)")
	}
}

// TestWriteRejectsBadRegions is the same check one level up: Write must
// refuse the image rather than write a resource fork that disagrees with
// the data fork.
func TestWriteRejectsBadRegions(t *testing.T) {
	const size = 4 * SectorSize
	var out bytes.Buffer
	err := Write(&out, newFakeSrc(size), size, Options{
		VolumeName:   "test",
		Compression:  CompressionNone,
		ChunkSectors: 4,
		Regions:      []Region{{Name: "", Type: "Apple_HFS", SectorCount: 3}},
	})
	if err == nil {
		t.Fatal("Write accepted regions that cover 3 of 4 sectors")
	}
}
