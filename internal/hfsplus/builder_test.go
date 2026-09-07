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
		CNID:       CNIDRootFolder,
		ParentCNID: CNIDRootParent,
		Kind:       KindFolder,
		Mode:       0o40755,
		OwnerID:    99, GroupID: 99,
		CreateTime:       macTime,
		ContentModTime:   macTime,
		AttributeModTime: macTime,
		AccessTime:       macTime,
		BackupTime:       macTime,
	}
	e.Name, _ = NewName("")
	return e
}

// TestWriteVolumeDetectsFileSizeChange: the extent for a file is sized
// from the length the caller reported, so the opener has to hand over
// exactly that many bytes. A file that grew between the walk and the
// write used to be truncated into the image silently, because io.CopyN
// stops at the requested count and reports no error.
func TestWriteVolumeDetectsFileSizeChange(t *testing.T) {
	macTime := MacTime(macEpoch.Add(60 * 60 * 24 * 365 * 100))

	tests := []struct {
		name     string
		reported uint64 // what the walk saw
		actual   string // what the opener delivers
		want     string // substring of the expected error
	}{
		{"grew", 8, "the file got longer", "grew"},
		{"shrank", 64, "short", "shrank"},
		{"unchanged", 8, "12345678", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := mkRoot(macTime)
			root.Valence = 1
			name, _ := NewName("f.txt")
			file := &Entry{
				CNID: 16, ParentCNID: CNIDRootFolder, Name: name, Kind: KindFile,
				Mode: 0o100644, OwnerID: 99, GroupID: 99,
				CreateTime: macTime, ContentModTime: macTime, AttributeModTime: macTime,
				AccessTime: macTime, BackupTime: macTime,
				DataLogicalSize: tt.reported,
			}
			inputs := &VolumeInputs{
				Entries: []*Entry{root, file},
				UserFiles: []UserFileInput{{
					Entry: file,
					Size:  tt.reported,
					Opener: func() (io.ReadCloser, error) {
						return io.NopCloser(strings.NewReader(tt.actual)), nil
					},
				}},
				BlockSize: 4096,
				MacTime:   macTime,
			}
			_, err := WriteVolume(&memWriteSeeker{}, inputs)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("WriteVolume: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("WriteVolume accepted a file that %s under it", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error should say the file %s, got: %v", tt.want, err)
			}
		})
	}
}

func TestWriteVolumeSmallImage(t *testing.T) {
	macTime := MacTime(macEpoch.Add(60 * 60 * 24 * 365 * 100)) // ~2004

	root := mkRoot(macTime)
	root.Valence = 1

	name, _ := NewName("hello.txt")
	const helloContent = "hello world\n"
	file := &Entry{
		CNID:       16,
		ParentCNID: CNIDRootFolder,
		Name:       name,
		Kind:       KindFile,
		Mode:       0o100644,
		OwnerID:    99, GroupID: 99,
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

func TestEntryTextEncodingHint(t *testing.T) {
	asciiName, _ := NewName("hello")
	utfName, _ := NewName("héllo")

	e1 := &Entry{Name: asciiName}
	if got := e1.TextEncodingHint(); got != 0 {
		t.Errorf("ASCII text encoding: got %d want 0", got)
	}
	e2 := &Entry{Name: utfName}
	if got := e2.TextEncodingHint(); got != 0x7F {
		t.Errorf("non-ASCII text encoding: got %d want 0x7F", got)
	}
}
