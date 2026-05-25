package udif

import (
	"bytes"
	"testing"
)

func TestKolyRoundTrip(t *testing.T) {
	in := Koly{
		Signature:        KolySignature,
		Version:          KolyVersion,
		HeaderSize:       KolySize,
		Flags:            FlagFlattened,
		DataForkLength:   123456,
		SegmentNumber:    1,
		SegmentCount:     1,
		SegmentID:        UDIFID{Data1: 0x11223344, Data2: 0x55667788, Data3: 0x99aabbcc, Data4: 0xddeeff00},
		DataForkChecksum: UDIFChecksum{Type: ChecksumCRC32, Size: 32, Data: [32]uint32{0xCAFEBABE}},
		XMLOffset:        123456,
		XMLLength:        4096,
		MasterChecksum:   UDIFChecksum{Type: ChecksumCRC32, Size: 32, Data: [32]uint32{0xDEADBEEF}},
		ImageVariant:     ImageVariantPartition,
		SectorCount:      4096,
	}
	buf := in.Encode()
	if len(buf) != KolySize {
		t.Fatalf("Encode returned %d bytes, want %d", len(buf), KolySize)
	}
	out, err := DecodeKoly(buf[:])
	if err != nil {
		t.Fatalf("DecodeKoly: %v", err)
	}
	if out.Signature != in.Signature {
		t.Errorf("Signature: got %#x want %#x", out.Signature, in.Signature)
	}
	if out.DataForkLength != in.DataForkLength {
		t.Errorf("DataForkLength: got %d want %d", out.DataForkLength, in.DataForkLength)
	}
	if out.XMLOffset != in.XMLOffset {
		t.Errorf("XMLOffset: got %d want %d", out.XMLOffset, in.XMLOffset)
	}
	if out.MasterChecksum.Data[0] != 0xDEADBEEF {
		t.Errorf("MasterChecksum.Data[0]: got %#x want 0xDEADBEEF", out.MasterChecksum.Data[0])
	}
	if out.SegmentID != in.SegmentID {
		t.Errorf("SegmentID: got %+v want %+v", out.SegmentID, in.SegmentID)
	}
}

func TestKolyMagicBytes(t *testing.T) {
	k := Koly{Signature: KolySignature, Version: KolyVersion, HeaderSize: KolySize}
	buf := k.Encode()
	if !bytes.Equal(buf[0:4], []byte{'k', 'o', 'l', 'y'}) {
		t.Errorf("signature bytes: got %q want %q", buf[0:4], "koly")
	}
}

func TestBlkxEncodeSize(t *testing.T) {
	bt := &BlkxTable{
		Signature:   MishSignature,
		InfoVersion: 1,
		SectorCount: 8,
		Runs: []BlockRun{
			{Type: BlockRaw, SectorCount: 8, CompLength: 4096},
			{Type: BlockTerminator, SectorStart: 8},
		},
	}
	b := bt.Encode()
	want := blkxHeaderSize + 2*blockRunSize
	if len(b) != want {
		t.Fatalf("Encode size: got %d want %d", len(b), want)
	}
	if string(b[0:4]) != "mish" {
		t.Errorf("Signature bytes: got %q want \"mish\"", b[0:4])
	}
}
