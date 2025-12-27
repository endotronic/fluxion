package util

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
)

// GetFSUsage returns the used and total bytes for the filesystem containing path.
// It uses syscall.Statfs which is available on Linux and macOS (Darwin).
func GetFSUsage(path string) (usedBytes uint64, totalBytes uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, fmt.Errorf("statfs failed for %s: %w", path, err)
	}

	// Calculate sizes
	// Note: Types of fields in Statfs_t vary by platform (int32/int64/uint64)
	// We cast to uint64 for consistency and safety.
	
	bsize := uint64(stat.Bsize)
	totalBytes = uint64(stat.Blocks) * bsize
	
	// Available blocks to non-root user is usually Bavail, but Bfree is total free blocks.
	// We usually want "Used" = Total - Free.
	freeBytes := uint64(stat.Bfree) * bsize
	
	usedBytes = totalBytes - freeBytes
	
	return usedBytes, totalBytes, nil
}

// GetRecursiveFSUsage calculates the total usage of the directory and any sub-mounts.
// It is useful for estimating scan size when the target path contains other mount points.
// It returns the list of sub-mounts found (excluding root itself).
func GetRecursiveFSUsage(root string) (usedBytes uint64, totalBytes uint64, foundMounts []string, err error) {
	// 1. Get base usage of the root path itself
	baseUsed, baseTotal, err := GetFSUsage(root)
	if err != nil {
		return 0, 0, nil, err
	}
	usedBytes = baseUsed
	totalBytes = baseTotal

	// 2. Find all mount points
	mounts, err := GetMountPoints()
	if err != nil {
		// If we fail to list mounts, just return the base usage.
		return usedBytes, totalBytes, nil, nil 
	}

	// 3. Clean root path for comparison
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return usedBytes, totalBytes, nil, nil
	}
	
	for _, m := range mounts {
		// Skip if it's the root itself (already counted in step 1)
		if m == rootAbs {
			continue
		}

		// Check if m is inside root
		rel, err := filepath.Rel(rootAbs, m)
		if err != nil {
			continue
		}
		
		// If rel does not start with "..", it is inside.
		if !strings.HasPrefix(rel, "..") {
			// Found a sub-mount!
			mUsed, mTotal, err := GetFSUsage(m)
			if err == nil {
				usedBytes += mUsed
				totalBytes += mTotal
				foundMounts = append(foundMounts, m)
			}
		}
	}

	return usedBytes, totalBytes, foundMounts, nil
}

