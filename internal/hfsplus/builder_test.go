package hfsplus

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"strings"
	"testing"
)

// memWriteSeeker is a bytes.Buffer that also implements io.Seeker, used to
// drive WriteVolume from tests without touching disk.
type memWriteSeeker struct {
	buf []byte
	off int64
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	end := m.off + int64(len(p))
	if int64(len(m.buf)) < end {
		nb := make([]byte, end)
		copy(nb, m.buf)
		m.buf = nb
	}
	copy(m.buf[m.off:end], p)
	m.off = end
	return len(p), nil
}

func (m *memWriteSeeker) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.off = off
	case io.SeekCurrent:
		m.off += off
	case io.SeekEnd:
		m.off = int64(len(m.buf)) + off
	}
	return m.off, nil
}

func mkRoot(macTime uint32) *Entry {
	e := &Entry{
		CNID:             CNIDRootFolder,
		ParentCNID:       CNIDRootParent,
		Kind:             KindFolder,
		Mode:             0o40755,
		OwnerID:          99, GroupID: 99,
		CreateTime:       macTime,
		ContentModTime:   macTime,
		AttributeModTime: macTime,
		AccessTime:       macTime,
		BackupTime:       macTime,
	}
	e.Name, _ = NewName("")
	return e
}

func TestWriteVolumeSmallImage(t *testing.T) {
	macTime := MacTime(macEpoch.Add(60 * 60 * 24 * 365 * 100)) // ~2004

	root := mkRoot(macTime)
	root.Valence = 1

	name, _ := NewName("hello.txt")
	const helloContent = "hello world\n"
	file := &Entry{
		CNID:             16,
		ParentCNID:       CNIDRootFolder,
		Name:             name,
		Kind:             KindFile,
		Mode:             0o100644,
		OwnerID:          99, GroupID: 99,
		CreateTime:       macTime,
		ContentModTime:   macTime,
		AttributeModTime: macTime,
		AccessTime:       macTime,
		BackupTime:       macTime,
		DataLogicalSize:  uint64(len(helloContent)),
	}

	inputs := &VolumeInputs{
		Entries: []*Entry{root, file},
		UserFiles: []UserFileInput{{
			Entry:  file,
			Size:   uint64(len(helloContent)),
			Opener: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(helloContent)), nil },
		}},
		BlockSize: 4096,
		MacTime:   macTime,
	}

	mw := &memWriteSeeker{}
	volSize, err := WriteVolume(mw, inputs)
	if err != nil {
		t.Fatalf("WriteVolume: %v", err)
	}
	if int64(volSize) != int64(len(mw.buf)) {
		t.Errorf("volumeSize %d != buf len %d", volSize, len(mw.buf))
	}

	// Spot-check the primary volume header at offset 1024.
	sig := binary.BigEndian.Uint16(mw.buf[1024:1026])
	if sig != VolumeSignature {
		t.Errorf("VH signature at 1024: got %#x want %#x", sig, VolumeSignature)
	}
	// Alternate header at volSize-1024 should be identical.
	altOff := int(volSize) - 1024
	if !bytes.Equal(mw.buf[1024:1024+VolumeHeaderSize], mw.buf[altOff:altOff+VolumeHeaderSize]) {
		t.Errorf("alternate VH differs from primary")
	}

	// Trailing 512 bytes should be zero (the format's reserved tail).
	tail := mw.buf[len(mw.buf)-512:]
	for i, b := range tail {
		if b != 0 {
			t.Errorf("byte at -%d not zero: %#x", 512-i, b)
			break
		}
	}
}

func TestWriteVolumeWritesToFile(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "vol-*.hfs")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()

	macTime := MacTime(macEpoch.Add(60 * 60 * 24 * 365 * 100))
	root := mkRoot(macTime)

	inputs := &VolumeInputs{
		Entries:   []*Entry{root},
		BlockSize: 4096,
		MacTime:   macTime,
	}
	if _, err := WriteVolume(tmp, inputs); err != nil {
		t.Fatalf("WriteVolume: %v", err)
	}
	st, err := tmp.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if st.Size()%4096 != 0 {
		t.Errorf("output size %d not a multiple of block size", st.Size())
	}
}

func TestEntryEncoding(t *testing.T) {
	asciiName, _ := NewName("hello")
	utfName, _ := NewName("héllo")

	e1 := &Entry{Name: asciiName}
	if e1.EncodingBit() != 0 {
		t.Errorf("ASCII encoding bit: got %d want 0", e1.EncodingBit())
	}
	e2 := &Entry{Name: utfName}
	if e2.EncodingBit() != 0x7F {
		t.Errorf("non-ASCII encoding bit: got %d want 0x7F", e2.EncodingBit())
	}
}
