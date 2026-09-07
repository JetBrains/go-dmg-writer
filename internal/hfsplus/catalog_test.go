package hfsplus

import (
	"testing"
)

func TestCatalogKeyOrdering(t *testing.T) {
	a := encodeCatalogKey(2, mustName(t, "alpha"))
	b := encodeCatalogKey(2, mustName(t, "beta"))
	c := encodeCatalogKey(3, mustName(t, "alpha"))
	if compareCatalogKeys(a, b) >= 0 {
		t.Errorf("(2,alpha) should sort before (2,beta)")
	}
	if compareCatalogKeys(b, c) >= 0 {
		t.Errorf("(2,beta) should sort before (3,alpha)")
	}
	if compareCatalogKeys(a, a) != 0 {
		t.Errorf("equal keys should compare 0")
	}
}

func TestCatalogThreadKey(t *testing.T) {
	// Thread record key is (cnid, "") - should sort before any forward
	// record under the same parent (because empty name < anything).
	thr := encodeCatalogKey(5, HFSName{})
	fwd := encodeCatalogKey(5, mustName(t, "anything"))
	if compareCatalogKeys(thr, fwd) >= 0 {
		t.Errorf("thread key should sort before sibling forward key")
	}
}

func TestBuildCatalogTreeSmall(t *testing.T) {
	root := &Entry{
		CNID:       CNIDRootFolder,
		ParentCNID: CNIDRootParent,
		Name:       HFSName{},
		Kind:       KindFolder,
		Mode:       0o040755,
		Valence:    2,
		CreateTime: 1, ContentModTime: 1, AttributeModTime: 1, AccessTime: 1, BackupTime: 1,
	}
	f1 := &Entry{
		CNID:            CNIDFirstUser,
		ParentCNID:      CNIDRootFolder,
		Name:            mustName(t, "alpha.txt"),
		Kind:            KindFile,
		Mode:            0o100644,
		DataLogicalSize: 123,
		DataTotalBlocks: 1,
		CreateTime:      1, ContentModTime: 1, AttributeModTime: 1, AccessTime: 1, BackupTime: 1,
	}
	d1 := &Entry{
		CNID:       CNIDFirstUser + 1,
		ParentCNID: CNIDRootFolder,
		Name:       mustName(t, "subdir"),
		Kind:       KindFolder,
		Mode:       0o040755,
		Valence:    0,
		CreateTime: 1, ContentModTime: 1, AttributeModTime: 1, AccessTime: 1, BackupTime: 1,
	}
	r, err := BuildCatalogTree([]*Entry{root, f1, d1}, 4096)
	if err != nil {
		t.Fatalf("BuildCatalogTree: %v", err)
	}
	// 3 entries × 2 records each = 6 leaf records.
	if r.LeafRecords != 6 {
		t.Errorf("LeafRecords: got %d want 6", r.LeafRecords)
	}
	if r.TreeDepth != 1 {
		t.Errorf("TreeDepth: got %d want 1", r.TreeDepth)
	}
}

// TestBuildCatalogTreeRejectsDuplicateKeys: NewName applies NFD, so two
// source names that differ only in composition produce one catalog key.
// A tree with two equal keys is corrupt, and because sort.Slice is not
// stable their order can also change between runs. Both spellings below
// are distinct Go string literals and one NFD form.
func TestBuildCatalogTreeRejectsDuplicateKeys(t *testing.T) {
	root := &Entry{
		CNID: CNIDRootFolder, ParentCNID: CNIDRootParent, Kind: KindFolder, Valence: 2,
		CreateTime: 1, ContentModTime: 1, AttributeModTime: 1, AccessTime: 1, BackupTime: 1,
	}
	mk := func(cnid uint32, name string) *Entry {
		return &Entry{
			CNID: cnid, ParentCNID: CNIDRootFolder, Name: mustName(t, name), Kind: KindFile,
			CreateTime: 1, ContentModTime: 1, AttributeModTime: 1, AccessTime: 1, BackupTime: 1,
		}
	}
	entries := []*Entry{
		root,
		mk(CNIDFirstUser, "caf\u00e9.txt"),    // é as U+00E9
		mk(CNIDFirstUser+1, "cafe\u0301.txt"), // e + U+0301
	}
	if _, err := BuildCatalogTree(entries, 4096); err == nil {
		t.Fatal("BuildCatalogTree accepted two entries with the same catalog key")
	}
}

// TestBuildCatalogTreeRejectsUnknownKind: an out-of-range EntryKind used
// to panic out of a library function that already returns an error.
func TestBuildCatalogTreeRejectsUnknownKind(t *testing.T) {
	root := &Entry{
		CNID: CNIDRootFolder, ParentCNID: CNIDRootParent, Kind: KindFolder,
		CreateTime: 1, ContentModTime: 1, AttributeModTime: 1, AccessTime: 1, BackupTime: 1,
	}
	bogus := &Entry{
		CNID: CNIDFirstUser, ParentCNID: CNIDRootFolder, Name: mustName(t, "x"),
		Kind:       EntryKind(99),
		CreateTime: 1, ContentModTime: 1, AttributeModTime: 1, AccessTime: 1, BackupTime: 1,
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("BuildCatalogTree panicked instead of returning an error: %v", r)
		}
	}()
	if _, err := BuildCatalogTree([]*Entry{root, bogus}, 4096); err == nil {
		t.Fatal("BuildCatalogTree accepted an entry with an unknown kind")
	}
}

func mustName(t *testing.T, s string) HFSName {
	t.Helper()
	n, err := NewName(s)
	if err != nil {
		t.Fatalf("NewName(%q): %v", s, err)
	}
	return n
}
