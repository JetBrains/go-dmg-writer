# B-tree investigation — what TN1150 doesn't quite tell you

This was, by a comfortable margin, the hardest problem in the project.
HFS+ B-trees are documented in
[TN1150 §"B-trees"](https://developer.apple.com/library/archive/technotes/tn/tn1150.html#BTrees),
but the description of the per-node **record offset table** is
ambiguous enough that we initially encoded it backwards. The result
was an image that `hexdump`'d as "obviously well-structured" yet
caused `fsck_hfs` to fail with the cryptic:

```
hfs_swap_BTNode: offsets X and Y out of order
```

This document records what we found.

## Node anatomy

Every HFS+ B-tree node is a fixed-size block (we use 8192 bytes for
catalog, 4096 for extents and attributes). The layout is:

```
node[0 .. 13]                            BTNodeDescriptor (14 bytes)
node[14 .. recordsEnd-1]                  records, packed back-to-back
node[recordsEnd .. offsetTableStart-1]    free space (zero-filled)
node[offsetTableStart .. nodeSize-1]      offset table
                                          ((numRecords + 1) × 2 bytes)
```

The offset table holds **`numRecords + 1` uint16s**. The +1 is the
offset of the **first byte of free space** (i.e. `recordsEnd`); this is
how the verifier knows where the records end.

## The catch: offset[0] is at the highest address

This is what we got wrong and what TN1150 doesn't make obvious. The
table indices go **backwards in memory**:

```
node[nodeSize-2 .. nodeSize-1]            ← offset[0]   == 14 (start of first record, always)
node[nodeSize-4 .. nodeSize-3]            ← offset[1]   == start of second record
node[nodeSize-6 .. nodeSize-5]            ← offset[2]   == start of third record
...
node[offsetTableStart .. offsetTableStart+1]
                                          ← offset[numRecords] == start of free space
```

So offsets *increase in value* as their indices increase, but their
storage location *decreases in memory address*. Reading the offset
table left-to-right gives you a strictly **decreasing** sequence of
values. `fsck_hfs` validates exactly this monotonicity, and the error
message `offsets X and Y out of order` means "the values you wrote, in
the order I read them off the high end, are not monotonically
decreasing".

We rediscovered this by `hexdump`'ing the *very last 16 bytes* of a
node from a known-good `hdiutil`-produced DMG, and noticing that
`0x000E` (= 14, start of first record) sat at the very end. Once you
see that, everything else falls into place.

The actual code lives in `internal/hfsplus/btree.go`:

- `writeRecordsNode` — fills `offset[0]…offset[N]` at decreasing
  addresses (`offsetTablePos`, `offsetTablePos − 2`, ...).
- `emitHeaderNode` and `emitMapNode` follow the same convention.

The unit test `internal/hfsplus/btree_test.go` includes a small
golden-bytes assertion that pins the layout for the header node.

## TN1150's wording

The relevant passage in TN1150 reads (emphasis added):

> The record offsets are stored at the end of the node. The last two
> bytes of the node contain the offset of the **first** record. The
> previous two bytes contain the offset of the **second** record, and
> so on. The offset of the free space follows the offset of the last
> record.

So it *is* stated correctly, but it's easy to read past — especially if
you come at the format with the mental model of a packed C array
("`offset[0]` is at the lower address"), which is the natural default.
The empirical test (`hexdump` the end of any real node) is the
unambiguous answer.

## Header node

Node 0 in every B-tree is a **header node** containing exactly three
records:

1. **`BTHeaderRec`** (106 bytes): tree-wide bookkeeping — `treeDepth`,
   `rootNode`, `leafRecords`, `firstLeafNode`, `lastLeafNode`,
   `nodeSize`, `keyCompareType`, `attributes`, etc.
2. **User data record** (128 bytes): reserved area for filesystem
   policy; we zero-fill.
3. **Map record** (the rest of the node minus offset table): a bitmap
   of which nodes in the tree are in use. Bit 0 byte 0 = node 0 (the
   header itself, always set to 1).

The offset table for the header node, by the rule above, is:

```
node[nodeSize-2..nodeSize-1] = 14    // BTHeaderRec starts at byte 14
node[nodeSize-4..nodeSize-3] = 120   // user data record starts at 14+106
node[nodeSize-6..nodeSize-5] = 248   // map record starts at 120+128
node[nodeSize-8..nodeSize-7] = end-of-map
```

`emitHeaderNode` in `btree.go` writes those four offsets.

## Empty trees

If you have no records to emit (e.g., an extents-overflow tree on a
volume with no fragmented files), naively you'd want zero leaf nodes.
`fsck_hfs` actually requires a **single-node tree**: one header node
with `treeDepth=0`, `rootNode=0`, `leafRecords=0`,
`firstLeafNode=0`, `lastLeafNode=0`, `totalNodes=1`. This is what
`emitEmptyTree` in `btree.go` produces.

The "weirdness" that the root node is also the header node only
applies to empty trees; in normal trees `rootNode` points to a separate
index/leaf node.

## Index vs leaf nodes

In our **write-once** packer the algorithm is:

1. Start with a sorted slice of records (catalog records in catalog
   binary order; extents records in `(forkType, fileID, startBlock)`
   order; attribute records in `(fileID, attrName, startBlock)`
   order).
2. Pack them greedily into leaf nodes, each at most ~75% full to
   match `BTHeaderRec.clumpSize` expectations and to give a small
   amount of slack (which we never use, since these trees are
   immutable).
3. If there's more than one leaf, build an index node referencing the
   first key of each leaf. Repeat until one node remains.
4. That last node becomes the **root**; its node number goes into
   `BTHeaderRec.rootNode`.

Leaf nodes' siblings are linked: each `BTNodeDescriptor` carries
`fLink` (next-leaf number) and `bLink` (previous-leaf number).
`firstLeafNode` and `lastLeafNode` in the header record point at the
two endpoints.

## Clump size, padding, and the `maxClump` rule

`PadToBlocks` (in `btree.go`) rounds up the emitted B-tree image to a
whole allocation-block boundary, then rewrites the *inner*
`BTHeaderRec.clumpSize` so it matches the outer `HFSPlusForkData.clumpSize`
in the Volume Header.

The verifier checks both:

- VH `forkData.clumpSize == BT header clumpSize` (consistency).
- `clumpSize ≤ totalBlocks / 4 × blockSize` (the "max clump" rule).

For small volumes the second rule binds, so `layout.go` pads
`totalBlocks` to satisfy it. Details in
[fsck-hfs-rules.md](fsck-hfs-rules.md).

## Take-aways for future maintainers

- **`hexdump` an `hdiutil`-produced reference DMG early and often.**
  Use `hdiutil attach -nomount disk.dmg` to get a `/dev/diskN` you can
  `dd` into a flat file. Most ambiguities in TN1150 dissolve immediately
  on inspection of real bytes.
- **`fsck_hfs -fnd` is your debugger.** The `-d` flag makes it spew
  every record it walks; very useful for narrowing "the catalog tree
  is broken" to "node 3 record 1 is broken".
- **The offset table is `numRecords + 1` entries, not `numRecords`**.
  Forgetting the free-space offset gives subtly broken nodes that may
  still mount.
- **Always 2-byte align records.** See attribute records in
  [fsck-hfs-rules.md](fsck-hfs-rules.md).

## References

- [TN1150 §"B-trees"](https://developer.apple.com/library/archive/technotes/tn/tn1150.html#BTrees).
- [`apple-oss-distributions/hfs:core/hfs_btreeio.c`](https://github.com/apple-oss-distributions/hfs/blob/main/core/hfs_btreeio.c)
  — kernel B-tree I/O. The `hfs_swap_BTNode` function is the one that
  emits the `offsets X and Y out of order` error.
- [`apple-oss-distributions/hfs:lib_fsck_hfs/dfalib/SBTree.c`](https://github.com/apple-oss-distributions/hfs/blob/main/lib_fsck_hfs/dfalib/SBTree.c)
  — `fsck_hfs`'s own B-tree traversal; cross-checks the offset table
  monotonicity.
