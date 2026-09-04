package udif

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"runtime"
	"sync"
	"time"
)

// SectorSize is the logical sector size used throughout UDIF (512 bytes).
const SectorSize = 512

// Compression selects how chunks in the data fork are encoded.
type Compression int

const (
	// CompressionNone emits every non-zero chunk as BLOCK_RAW. Used for
	// uncompressed UDRO output.
	CompressionNone Compression = iota
	// CompressionZlib emits chunks as BLOCK_ZLIB, falling back to
	// BLOCK_RAW when compression would inflate the chunk.
	CompressionZlib
)

// PartitionImageVariant selects the value baked into the KOLY's
// ImageVariant field. For partitionless single-volume DMGs (what
// hdiutil create -srcfolder produces for a folder) this is "partition".
const partitionVariantNumber = 0xFFFFFFFE // ENTIRE_DEVICE_DESCRIPTOR

// Region is one span of the disk that gets its own blkx resource. A
// map-less image is a single unnamed region and needs none of this; a
// partitioned image needs one Region per element of its map, because a
// reader that parses the image without mounting it takes the region
// table at face value.
//
// The resource is named "<Name> (<Type> : <index>)", which is the form
// `hdiutil` writes and `hdiutil verify` prints. Name is the partition's
// own name and is empty for free space; Type is the partition type as
// hdiutil spells it, for example, "Apple_HFS", "Apple_Free" or "MBR".
type Region struct {
	Name        string
	Type        string
	SectorCount uint64
}

// Options configure a [Write] call.
type Options struct {
	// Regions describe the disk one span at a time, in sector order,
	// and the counts must add up to the whole image. [Write] then emits
	// one blkx resource per region and marks the image as a device
	// rather than as a bare partition.
	//
	// Leave it empty for a map-less image: the whole data fork is one
	// unnamed partition, which is what `hdiutil create -srcfolder`
	// produces for a folder.
	Regions []Region
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
	// Workers is the size of the per-chunk compression worker pool. A
	// value of 0 means runtime.NumCPU(). Set to 1 for fully sequential
	// behavior (useful for profiling or low-RAM hosts).
	//
	// Output is byte-identical regardless of Workers: chunks are
	// CRC'd in input order on the producer goroutine, compressed
	// independently by the workers, and emitted in input order by the
	// drain. The only thing Workers affect is wall-clock time.
	Workers int
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

	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if opts.Compression == CompressionNone {
		// No compression to parallelize; the producer just CRCs and
		// hands the bytes to the writer. Forcing 1 worker keeps the
		// hot path simple and avoids spawning idle goroutines.
		workers = 1
	}

	// Wrap src in a bufio.Reader so per-chunk io.ReadFull doesn't hit
	// the kernel one read() at a time. The chunk size is 1 MiB by
	// default, which is itself a sensible read syscall granularity,
	// but a slightly larger buffer lets the kernel readahead get a
	// full step ahead of us.
	bsrc := bufio.NewReaderSize(src, 4*chunkBytes)

	// Wrap dst similarly. Per-chunk Write calls otherwise hit the
	// kernel ~3500 times for a 3.5 GiB input.
	bdst := bufio.NewWriterSize(dst, 4*chunkBytes)

	volName := opts.VolumeName
	if volName == "" {
		volName = "disk image"
	}

	specs, imageVariant, err := blkxSpecs(opts.Regions, volName, totalSectors)
	if err != nil {
		return err
	}

	// One blkx table per region, encoded in sector order so the single
	// pass over src stays a single pass. dataForkLen runs across the
	// whole fork, because a run's CompOffset is an absolute offset into
	// it even though its SectorStart is relative to its own region.
	rf := NewResourceFork()
	var (
		dataForkLen uint64
		tableCRCs   []uint32
	)
	dataForkCRC := crc32.NewIEEE()
	for _, s := range specs {
		regionLen := int64(s.sectors) * SectorSize
		largestRunUnc := uint32(chunkBytes)
		if int64(chunkBytes) > regionLen {
			largestRunUnc = uint32(regionLen)
		}

		// The table's own CRC covers only its region; dataForkCRC spans
		// the lot and ends up in the KOLY. Neither sees the sectors of
		// a BlockZero run, which `encodeChunks` leaves out.
		tableCRC := crc32.NewIEEE()
		runs, next, err := encodeChunks(bdst, bsrc, regionLen, dataForkLen, chunkBytes,
			opts.Compression, workers, io.MultiWriter(tableCRC, dataForkCRC))
		if err != nil {
			return err
		}
		dataForkLen = next

		runs = append(runs, BlockRun{
			Type:        BlockTerminator,
			SectorStart: s.sectors,
			SectorCount: 0,
			CompOffset:  dataForkLen,
			CompLength:  0,
		})

		blkx := &BlkxTable{
			Signature:              MishSignature,
			InfoVersion:            1,
			FirstSectorNumber:      s.firstSector,
			SectorCount:            s.sectors,
			DataStart:              0,
			DecompressBufRequested: (largestRunUnc + SectorSize - 1) / SectorSize,
			BlocksDescriptor:       s.descriptor,
			Checksum: UDIFChecksum{
				Type: ChecksumCRC32,
				Size: 32,
				Data: [32]uint32{tableCRC.Sum32()},
			},
			Runs: runs,
		}
		tableCRCs = append(tableCRCs, tableCRC.Sum32())
		rf.Add("blkx", Resource{
			ID:         s.id,
			Name:       s.name,
			Attributes: AttributeHdiutil,
			Data:       blkx.Encode(),
		})
	}
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
	if err := writeBytes(bdst, xmlBuf.Bytes()); err != nil {
		return err
	}

	masterCRC := MasterChecksum(tableCRCs)
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
		ImageVariant:     imageVariant,
		SectorCount:      totalSectors,
	}
	kolyBuf := koly.Encode()
	if err := writeBytes(bdst, kolyBuf[:]); err != nil {
		return err
	}
	return bdst.Flush()
}

// blkxSpec is everything about one blkx resource that does not come out
// of the sector data itself.
type blkxSpec struct {
	name        string
	id          int32
	descriptor  uint32
	firstSector uint64
	sectors     uint64
}

// blkxSpecs turns the caller's regions into one spec per blkx resource
// and picks the KOLY's ImageVariant to match. No regions mean the old
// single-table layout, so a map-less image keeps the bytes it had.
func blkxSpecs(regions []Region, volName string, totalSectors uint64) ([]blkxSpec, uint32, error) {
	if len(regions) == 0 {
		return []blkxSpec{{
			// "Apple_HFSX" tells macOS the partition holds an HFSX
			// filesystem (case-sensitive HFS+ variant). Plain HFS+
			// would be "Apple_HFS"; we use HFSX so the catalog can use
			// simple binary key comparison without an Apple
			// case-folding table.
			name:       fmt.Sprintf("%s (Apple_HFSX : 1)", volName),
			id:         0,
			descriptor: partitionVariantNumber,
			sectors:    totalSectors,
		}}, ImageVariantPartition, nil
	}

	specs := make([]blkxSpec, 0, len(regions))
	var first uint64
	for i, r := range regions {
		if r.SectorCount == 0 {
			return nil, 0, fmt.Errorf("udif: region %d (%s) covers no sectors", i, r.Type)
		}
		specs = append(specs, blkxSpec{
			name: fmt.Sprintf("%s (%s : %d)", r.Name, r.Type, i),
			// hdiutil numbers blkx resources from -1, so the payload
			// partition of a GPTSPUD image lands on ID 3. Nothing reads
			// the ID, but a diff against a real image is easier to
			// trust when even the boring fields line up.
			id:          int32(i) - 1,
			descriptor:  uint32(i),
			firstSector: first,
			sectors:     r.SectorCount,
		})
		first += r.SectorCount
	}
	if first != totalSectors {
		return nil, 0, fmt.Errorf("udif: regions cover %d sectors, want %d", first, totalSectors)
	}
	// A region table describes a whole device, not a bare partition.
	return specs, ImageVariantDevice, nil
}

// encodeChunks streams `regionLen` bytes from src in chunks of chunkBytes,
// optionally zlib-compressing each chunk, and writes the encoded bytes
// to dst in input order.
//
// SectorStart in the returned runs counts from the start of this region,
// which is what a blkx table wants, while CompOffset counts from the
// start of the whole data fork: hence baseOffset, the fork length so far.
// Returns the run slice (without the trailing terminator) and the fork
// length after this region.
//
// CRC sees every chunk that reaches the data fork and never sees an
// all-zero one, because an all-zero chunk becomes a [BlockZero] run and
// a blkx checksum leaves those sectors out.
//
// Concurrency: the caller's goroutine reads chunks sequentially (so
// CRC32 and the per-chunk read order stay deterministic), dispatches
// them to a pool of `workers` compression goroutines, and a single
// drain goroutine collects results in input order and writes them.
//
// Output is byte-identical regardless of `workers`. Set workers=1 to
// run fully sequentially (no goroutine setup overhead, easier to
// profile).
func encodeChunks(
	dst io.Writer,
	src io.Reader,
	regionLen int64,
	baseOffset uint64,
	chunkBytes int,
	compression Compression,
	workers int,
	crc io.Writer,
) ([]BlockRun, uint64, error) {
	type chunkJob struct {
		idx         int
		sectorStart uint64
		zero        bool   // decided by the producer, which also owns the CRC
		buf         []byte // newly allocated; worker takes ownership
	}
	type chunkResult struct {
		idx         int
		runType     uint32
		sectorStart uint64
		secCount    uint64
		payload     []byte // bytes to write to dst (nil for BlockZero)
	}

	if workers < 1 {
		workers = 1
	}

	// Channel capacities are sized to the worker count so the producer
	// can keep workers busy without unbounded memory growth.
	jobs := make(chan chunkJob, workers*2)
	results := make(chan chunkResult, workers*2)
	errCh := make(chan error, workers+1)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				secCount := uint64(len(j.buf)) / SectorSize
				switch {
				case j.zero:
					results <- chunkResult{
						idx:         j.idx,
						runType:     BlockZero,
						sectorStart: j.sectorStart,
						secCount:    secCount,
						payload:     nil,
					}
				case compression == CompressionZlib:
					var cb bytes.Buffer
					cb.Grow(len(j.buf))
					zw, _ := zlib.NewWriterLevel(&cb, zlib.DefaultCompression)
					if _, err := zw.Write(j.buf); err != nil {
						trySendErr(errCh, fmt.Errorf("udif: zlib write: %w", err))
						return
					}
					if err := zw.Close(); err != nil {
						trySendErr(errCh, fmt.Errorf("udif: zlib close: %w", err))
						return
					}
					if cb.Len() >= len(j.buf) {
						// Compression made the chunk bigger; fall back
						// to BlockRaw, reusing the input buffer.
						results <- chunkResult{
							idx:         j.idx,
							runType:     BlockRaw,
							sectorStart: j.sectorStart,
							secCount:    secCount,
							payload:     j.buf,
						}
					} else {
						results <- chunkResult{
							idx:         j.idx,
							runType:     BlockZLIB,
							sectorStart: j.sectorStart,
							secCount:    secCount,
							payload:     cb.Bytes(),
						}
					}
				default:
					results <- chunkResult{
						idx:         j.idx,
						runType:     BlockRaw,
						sectorStart: j.sectorStart,
						secCount:    secCount,
						payload:     j.buf,
					}
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	// Drain goroutine: collects results into a small in-order buffer
	// and writes them to dst as soon as the next-in-line chunk is
	// available. This is what enforces deterministic output order.
	type drainOut struct {
		runs        []BlockRun
		dataForkLen uint64
		err         error
	}
	drainCh := make(chan drainOut, 1)
	go func() {
		var (
			runs        []BlockRun
			dataForkLen = baseOffset
			pending     = map[int]chunkResult{}
			nextIdx     int
		)
		for r := range results {
			pending[r.idx] = r
			for {
				cur, ok := pending[nextIdx]
				if !ok {
					break
				}
				delete(pending, nextIdx)
				nextIdx++

				run := BlockRun{
					Type:        cur.runType,
					SectorStart: cur.sectorStart,
					SectorCount: cur.secCount,
					CompOffset:  dataForkLen,
				}
				if len(cur.payload) > 0 {
					run.CompLength = uint64(len(cur.payload))
					if err := writeBytes(dst, cur.payload); err != nil {
						drainCh <- drainOut{err: err}
						return
					}
					dataForkLen += uint64(len(cur.payload))
				}
				runs = append(runs, run)
			}
		}
		drainCh <- drainOut{runs: runs, dataForkLen: dataForkLen}
	}()

	// Producer loop: read chunks sequentially, CRC them, dispatch to
	// workers. CRC32 stays on the producer goroutine so its output
	// is independent of the worker count and the order of result
	// processing.
	var (
		idx         int
		nextSector  uint64
		producerErr error
	)
	remaining := regionLen
producer:
	for remaining > 0 {
		thisChunk := chunkBytes
		if int64(thisChunk) > remaining {
			thisChunk = int(remaining)
		}
		buf := make([]byte, thisChunk)
		if _, err := io.ReadFull(src, buf); err != nil {
			producerErr = fmt.Errorf("udif: reading source: %w", err)
			break
		}
		// An all-zero chunk becomes a BlockZero run, whose sectors a
		// blkx checksum skips, so it must not reach the CRC either.
		// The worker is told the answer rather than recomputing it.
		zero := isAllZero(buf)
		if !zero {
			if _, err := crc.Write(buf); err != nil {
				producerErr = fmt.Errorf("udif: crc32 update: %w", err)
				break
			}
		}
		secCount := uint64(thisChunk) / SectorSize

		// If a worker has already errored, stop dispatching so we
		// don't pile up jobs that nobody will consume.
		select {
		case err := <-errCh:
			producerErr = err
			break producer
		default:
		}

		jobs <- chunkJob{idx: idx, sectorStart: nextSector, zero: zero, buf: buf}

		idx++
		nextSector += secCount
		remaining -= int64(thisChunk)
	}
	close(jobs)

	// Wait for drain to finish (workers feed into results, which the
	// goroutine above closes once wg is done).
	out := <-drainCh

	// Surface the first observed error: producer's I/O / CRC, then any
	// worker error that landed in errCh, then drain's write error.
	switch {
	case producerErr != nil:
		// Drain any extra worker errors so goroutines exit cleanly.
		drainErrCh(errCh)
		return nil, 0, producerErr
	case len(errCh) > 0:
		err := <-errCh
		drainErrCh(errCh)
		return nil, 0, err
	case out.err != nil:
		return nil, 0, out.err
	}
	return out.runs, out.dataForkLen, nil
}

// trySendErr is a non-blocking send onto a buffered errCh; if the
// channel is already full (another worker reported first) we just drop
// the duplicate so the goroutine can exit promptly.
func trySendErr(ch chan<- error, err error) {
	select {
	case ch <- err:
	default:
	}
}

// drainErrCh drains any remaining errors out of ch so the goroutines
// blocked on send can exit. We only care about the first error; the
// rest are diagnostic and would be lost anyway.
func drainErrCh(ch <-chan error) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
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
