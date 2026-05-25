package udif

import (
	"hash/crc32"
	"testing"
)

func TestCRC32Streaming(t *testing.T) {
	data := []byte("the quick brown fox jumps over the lazy dog")
	h := NewCRC32()
	h.Write(data[:10])
	h.Write(data[10:20])
	h.Write(data[20:])
	got := h.Sum32()

	want := crc32.ChecksumIEEE(data)
	if got != want {
		t.Fatalf("streaming CRC32: got %#x want %#x", got, want)
	}
}

func TestMasterChecksum(t *testing.T) {
	got := MasterChecksum([]uint32{0xCAFEBABE, 0xDEADBEEF})
	want := crc32.ChecksumIEEE([]byte{
		0xCA, 0xFE, 0xBA, 0xBE,
		0xDE, 0xAD, 0xBE, 0xEF,
	})
	if got != want {
		t.Fatalf("MasterChecksum: got %#x want %#x", got, want)
	}
}

func TestAsUDIFChecksum(t *testing.T) {
	h := NewCRC32()
	h.Write([]byte("hello"))
	ck := h.AsUDIFChecksum()
	if ck.Type != ChecksumCRC32 {
		t.Errorf("Type: got %d want %d", ck.Type, ChecksumCRC32)
	}
	if ck.Size != 32 {
		t.Errorf("Size: got %d want 32", ck.Size)
	}
	if ck.Data[0] != h.Sum32() {
		t.Errorf("Data[0]: got %#x want %#x", ck.Data[0], h.Sum32())
	}
}
