package util

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// MountInfo represents a single mount entry.
type MountInfo struct {
	Source string // The device or filesystem (e.g., /dev/sda1)
	Target string // The mount point (e.g., /mnt/data)
	FSType string // The filesystem type (e.g., ext4, zfs, tmpfs)
}

// GetMounts returns all active mounts with both source and target.
func GetMounts() ([]MountInfo, error) {
	if runtime.GOOS == "linux" {
		return getMountsLinux()
	} else if runtime.GOOS == "darwin" {
		return getMountsDarwin()
	}
	return nil, fmt.Errorf("unsupported OS: %s", runtime.GOOS)
}

// GetMountPoints returns a list of all absolute paths where filesystems are mounted.
// It wraps GetMounts for backward compatibility.
func GetMountPoints() ([]string, error) {
	mounts, err := GetMounts()
	if err != nil {
		return nil, err
	}
	points := make([]string, 0, len(mounts))
	for _, m := range mounts {
		points = append(points, m.Target)
	}
	return points, nil
}

// ResolveMountPoint checks if the argument is a valid directory.
// If it is, it returns the absolute path.
// If not, it checks if the argument matches a mount source and returns the corresponding mount point.
func ResolveMountPoint(arg string) (string, error) {
	// 1. Check if it's already a valid directory
	info, err := os.Stat(arg)
	if err == nil && info.IsDir() {
		return filepath.Abs(arg)
	}

	// 2. If not a directory (or error), check if it's a mount source
	mounts, err := GetMounts()
	if err != nil {
		return "", fmt.Errorf("failed to list mounts for resolution: %w", err)
	}

	for _, m := range mounts {
		if m.Source == arg {
			// Found a match!
			return m.Target, nil
		}
	}

	// 3. Not found
	return "", fmt.Errorf("argument '%s' is neither a valid directory nor a known mount source", arg)
}

// IsMountPoint checks if the given path is a mount point.
func IsMountPoint(path string) (bool, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}

	mounts, err := GetMounts()
	if err != nil {
		return false, err
	}

	for _, m := range mounts {
		if m.Target == absPath {
			return true, nil
		}
	}
	return false, nil
}

func getMountsLinux() ([]MountInfo, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var mounts []MountInfo
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			m := MountInfo{Source: fields[0], Target: fields[1]}
			if len(fields) >= 3 {
				m.FSType = fields[2]
			}
			mounts = append(mounts, m)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return mounts, nil
}

func getMountsDarwin() ([]MountInfo, error) {
	cmd := exec.Command("mount")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	// Output format: /dev/disk1s1 on / (apfs, local, journaled)
	re := regexp.MustCompile(`^(.+) on (.+) \(([^,)]+)`)

	var mounts []MountInfo
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		matches := re.FindStringSubmatch(line)
		if len(matches) > 2 {
			m := MountInfo{Source: matches[1], Target: matches[2]}
			if len(matches) > 3 {
				m.FSType = strings.TrimSpace(matches[3])
			}
			mounts = append(mounts, m)
		}
	}
	return mounts, nil
}

// FSTypeAt returns the filesystem type backing path - the type of the
// most-specific mount point containing it.
//
// Callers use it to tell a real filesystem from a memory-backed one. On most
// Linux systems /tmp is a tmpfs, so a program that "spills to disk" through the
// default temp directory is spilling to RAM, which is worth saying out loud when
// the entire point of spilling was to stay off the heap.
func FSTypeAt(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	mounts, err := GetMounts()
	if err != nil {
		return "", err
	}

	best, bestType := "", ""
	for _, m := range mounts {
		if !mountContains(m.Target, abs) {
			continue
		}
		if len(m.Target) >= len(best) {
			best, bestType = m.Target, m.FSType
		}
	}
	if best == "" {
		return "", fmt.Errorf("no mount point found for %s", path)
	}
	return bestType, nil
}

// IsMemoryBackedFS reports whether a filesystem type stores its contents in RAM.
func IsMemoryBackedFS(fsType string) bool {
	switch fsType {
	case "tmpfs", "ramfs", "devtmpfs":
		return true
	}
	return false
}

// mountContains reports whether path is at or under mount, matching only at a
// path boundary so /mnt/data2 is not treated as living under /mnt/data.
func mountContains(mount, path string) bool {
	mount, path = filepath.Clean(mount), filepath.Clean(path)
	if mount == path {
		return true
	}
	if mount[len(mount)-1] == filepath.Separator {
		return strings.HasPrefix(path, mount) // the root
	}
	return strings.HasPrefix(path, mount+string(filepath.Separator))
}
