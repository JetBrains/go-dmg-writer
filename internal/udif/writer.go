package udif

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"time"
)

// SectorSize is the logical sector size used throughout UDIF (512 bytes).
const SectorSize = 512

// Compression selects how chunks in the data fork are encoded.
type Compression int

const (
	// CompressionNone emits every non-zero chunk as BLOCK_RAW. Used for
	// both UDRW (where the image is intended to be writable, so the data
	// fork mirrors raw sectors 1:1) and UDRO (read-only but uncompressed).
	CompressionNone Compression = iota
	// CompressionZlib emits chunks as BLOCK_ZLIB, falling back to
	// BLOCK_RAW when compression would inflate the chunk.
	CompressionZlib
)

// PartitionImageVariant selects the value baked into the KOLY's
// ImageVariant field. For partitionless single-volume DMGs (what
// hdiutil create -srcfolder produces for a folder) this is "partition".
const partitionVariantNumber = 0xFFFFFFFE // ENTIRE_DEVICE_DESCRIPTOR

// Options configures a [Write] call.
type Options struct {
	// VolumeName is the user-facing label embedded in the blkx resource
	// "CFName" / "Name" fields ("MyVolume (Apple_HFS : 1)").
	VolumeName string
	// Compression selects the per-chunk encoding.
	Compression Compression
	// ChunkSectors is the number of 512-byte sectors per BLKX run.
	// Default 2048 (1 MiB).
	ChunkSectors uint32
	// Time is used to derive the deterministic SegmentID and for the
	// per-blkx ID. Pass time.Time{} to use 1970-01-01.
	Time time.Time
}

// Write streams the HFS+ image in src (whose total size is srcLen bytes)
// to dst, wrapping it as a UDIF DMG. Returns an error if anything goes
// wrong with I/O or compression.
//
// dst must support sequential writing. src must be a stream of exactly
// srcLen bytes; we read it once from start to end.
func Write(dst io.Writer, src io.Reader, srcLen int64, opts Options) error {
	if srcLen <= 0 {
		return errors.New("udif: empty source")
	}
	if srcLen%SectorSize != 0 {
		return fmt.Errorf("udif: source length %d is not a multiple of sector size %d", srcLen, SectorSize)
	}
	chunkSectors := opts.ChunkSectors
	if chunkSectors == 0 {
		chunkSectors = 2048
	}
	chunkBytes := int(chunkSectors) * SectorSize
	totalSectors := uint64(srcLen / SectorSize)

	dataForkCRC := crc32.NewIEEE()
	chunkBuf := make([]byte, chunkBytes)

	var (
		runs           []BlockRun
		dataForkLen    uint64
		largestRunUnc  uint32
		remaining      = uint64(srcLen)
		nextSector     uint64
	)

	for remaining > 0 {
		thisChunk := uint64(chunkBytes)
		if thisChunk > remaining {
			thisChunk = remaining
		}
		buf := chunkBuf[:thisChunk]
		if _, err := io.ReadFull(src, buf); err != nil {
			return fmt.Errorf("udif: reading source: %w", err)
		}
		dataForkCRC.Write(buf)

		secCount := thisChunk / SectorSize
		if thisChunk > uint64(largestRunUnc) {
			largestRunUnc = uint32(thisChunk)
		}

		switch {
		case isAllZero(buf):
			runs = append(runs, BlockRun{
				Type:        BlockZero,
				SectorStart: nextSector,
				SectorCount: secCount,
				CompOffset:  dataForkLen,
				CompLength:  0,
			})
		case opts.Compression == CompressionZlib:
			var cb bytes.Buffer
			zw, _ := zlib.NewWriterLevel(&cb, zlib.DefaultCompression)
			if _, err := zw.Write(buf); err != nil {
				return fmt.Errorf("udif: zlib write: %w", err)
			}
			if err := zw.Close(); err != nil {
				return fmt.Errorf("udif: zlib close: %w", err)
			}
			if cb.Len() >= len(buf) {
				if err := writeBytes(dst, buf); err != nil {
					return err
				}
				runs = append(runs, BlockRun{
					Type:        BlockRaw,
					SectorStart: nextSector,
					SectorCount: secCount,
					CompOffset:  dataForkLen,
					CompLength:  uint64(len(buf)),
				})
				dataForkLen += uint64(len(buf))
			} else {
				if err := writeBytes(dst, cb.Bytes()); err != nil {
					return err
				}
				runs = append(runs, BlockRun{
					Type:        BlockZLIB,
					SectorStart: nextSector,
					SectorCount: secCount,
					CompOffset:  dataForkLen,
					CompLength:  uint64(cb.Len()),
				})
				dataForkLen += uint64(cb.Len())
			}
		default:
			if err := writeBytes(dst, buf); err != nil {
				return err
			}
			runs = append(runs, BlockRun{
				Type:        BlockRaw,
				SectorStart: nextSector,
				SectorCount: secCount,
				CompOffset:  dataForkLen,
				CompLength:  uint64(len(buf)),
			})
			dataForkLen += uint64(len(buf))
		}

		nextSector += secCount
		remaining -= thisChunk
	}

	runs = append(runs, BlockRun{
		Type:        BlockTerminator,
		SectorStart: nextSector,
		SectorCount: 0,
		CompOffset:  dataForkLen,
		CompLength:  0,
	})

	blkx := &BlkxTable{
		Signature:              MishSignature,
		InfoVersion:            1,
		FirstSectorNumber:      0,
		SectorCount:            totalSectors,
		DataStart:              0,
		DecompressBufRequested: (largestRunUnc + SectorSize - 1) / SectorSize,
		BlocksDescriptor:       partitionVariantNumber,
		Checksum: UDIFChecksum{
			Type: ChecksumCRC32,
			Size: 32,
			Data: [32]uint32{dataForkCRC.Sum32()},
		},
		Runs: runs,
	}

	blkxBytes := blkx.Encode()

	rf := NewResourceFork()
	volName := opts.VolumeName
	if volName == "" {
		volName = "disk image"
	}
	rf.Add("blkx", Resource{
		ID:   0,
		// "Apple_HFSX" tells macOS the partition holds an HFSX
		// filesystem (case-sensitive HFS+ variant). Plain HFS+ would
		// be "Apple_HFS"; we use HFSX so the catalog can use simple
		// binary key comparison without an Apple case-folding table.
		Name:       fmt.Sprintf("%s (Apple_HFSX : 1)", volName),
		Attributes: AttributeHdiutil,
		Data:       blkxBytes,
	})
	rf.Add("plst", Resource{
		ID:         0,
		Name:       "",
		Attributes: 0,
		Data:       []byte{},
	})

	xmlOffset := dataForkLen
	xmlBuf := &bytes.Buffer{}
	if _, err := rf.WriteTo(xmlBuf); err != nil {
		return err
	}
	xmlLen := uint64(xmlBuf.Len())
	if err := writeBytes(dst, xmlBuf.Bytes()); err != nil {
		return err
	}

	masterCRC := MasterChecksum([]uint32{dataForkCRC.Sum32()})
	koly := Koly{
		Signature:        KolySignature,
		Version:          KolyVersion,
		HeaderSize:       KolySize,
		Flags:            FlagFlattened,
		DataForkOffset:   0,
		DataForkLength:   dataForkLen,
		SegmentNumber:    1,
		SegmentCount:     1,
		SegmentID:        deriveSegmentID(volName, opts.Time),
		DataForkChecksum: UDIFChecksum{Type: ChecksumCRC32, Size: 32, Data: [32]uint32{dataForkCRC.Sum32()}},
		XMLOffset:        xmlOffset,
		XMLLength:        xmlLen,
		MasterChecksum:   UDIFChecksum{Type: ChecksumCRC32, Size: 32, Data: [32]uint32{masterCRC}},
		ImageVariant:     ImageVariantPartition,
		SectorCount:      totalSectors,
	}
	kolyBuf := koly.Encode()
	return writeBytes(dst, kolyBuf[:])
}

func writeBytes(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

func isAllZero(p []byte) bool {
	// Fast path: compare against a fixed-size zero buffer in 64-byte
	// chunks. The Go compiler unrolls this into a few SIMD-friendly loops.
	const stride = 64
	var zero [stride]byte
	for len(p) >= stride {
		if !bytes.Equal(p[:stride], zero[:]) {
			return false
		}
		p = p[stride:]
	}
	for _, b := range p {
		if b != 0 {
			return false
		}
	}
	return true
}

// deriveSegmentID derives a 16-byte UDIF segment ID from the volume name
// and timestamp so that calling [Write] twice with identical inputs
// produces byte-identical output (useful for reproducible builds).
func deriveSegmentID(volName string, t time.Time) UDIFID {
	h := sha256.New()
	h.Write([]byte(volName))
	var tb [8]byte
	binary.BigEndian.PutUint64(tb[:], uint64(t.Unix()))
	h.Write(tb[:])
	sum := h.Sum(nil)
	return UDIFID{
		Data1: binary.BigEndian.Uint32(sum[0:4]),
		Data2: binary.BigEndian.Uint32(sum[4:8]),
		Data3: binary.BigEndian.Uint32(sum[8:12]),
		Data4: binary.BigEndian.Uint32(sum[12:16]),
	}
}
