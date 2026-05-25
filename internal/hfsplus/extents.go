package hfsplus

// BuildExtentsTree builds the extents-overflow B-tree. In the MVP no file
// requires more than 8 extents (we only ever allocate single contiguous
// runs), so the tree is always empty. We still need to emit a real
// header node so fsck doesn't complain.
func BuildExtentsTree() (*BuildResult, error) {
	return BuildTree(nil, BuildOpts{
		NodeSize:       ExtentsNodeSize,
		Attributes:     BTBigKeysMask,
		KeyCompareType: KeyCompareBinary,
		BTreeType:      BTreeHFSType,
		// HFSPlusExtentKey: 1 (forkType) + 1 (pad) + 4 (fileID) + 4 (startBlock) = 10.
		MaxKeyLength: 10,
	})
}
