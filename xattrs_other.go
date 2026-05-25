//go:build !darwin && !linux

package dmg

import "github.com/jetbrains/go-dmg-writer/internal/hfsplus"

// readXattrs is a no-op on platforms that don't expose POSIX extended
// attributes. The returned slice is always nil.
func readXattrs(path string, fileID uint32) ([]hfsplus.Attr, error) {
	_ = path
	_ = fileID
	return nil, nil
}
