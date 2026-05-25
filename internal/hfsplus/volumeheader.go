package hfsplus

// BuildVolumeHeader composes the volume header from a finalized plan and
// the timestamps from the per-volume `Time` knob. All five date fields
// must be non-zero; fsck_hfs flags zero dates as a "checked volume" issue.
func BuildVolumeHeader(plan *Plan, macTime uint32) VolumeHeader {
	return VolumeHeader{
		Signature:          VolumeSignature,
		Version:            VolumeVersion,
		Attributes:         VolUnmounted,
		LastMountedVersion: 0x676F2D64, // 'go-d' so attached volumes carry our fingerprint
		JournalInfoBlock:   0,
		CreateDate:         macTime,
		ModifyDate:         macTime,
		BackupDate:         macTime,
		CheckedDate:        macTime,
		FileCount:          plan.FileCount,
		FolderCount:        plan.FolderCount,
		BlockSize:          plan.BlockSize,
		TotalBlocks:        plan.TotalBlocks,
		FreeBlocks:         plan.AllocBitmap.FreeBlocks(),
		NextAllocation:     plan.NextAllocation,
		RsrcClumpSize:      plan.BlockSize,
		DataClumpSize:      plan.BlockSize,
		NextCatalogID:      plan.NextCatalogID,
		WriteCount:         1,
		EncodingsBitmap:    plan.EncodingsBitmap,
		AllocationFile:     plan.AllocFile,
		ExtentsFile:        plan.ExtentsFile,
		CatalogFile:        plan.CatalogFile,
		AttributesFile:     plan.AttributesFile,
		StartupFile:        ForkData{},
	}
}
