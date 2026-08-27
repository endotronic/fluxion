// Package zfsutil enumerates ZFS datasets and mounts/unmounts them.
//
// It does not read ZFS's own snapshot history (`.zfs/snapshot/*`, `zfs diff`)
// — that is a deliberately rejected feature, see knowledge/fleet.md "Verdict:
// do NOT build per-snapshot scanning". This package only drives a single,
// present-moment scan of each live dataset.
package zfsutil

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Runner executes an external command and returns its combined stdout+stderr.
// Indirected so callers can substitute a fake in tests without a real `zfs`
// binary (there is none in this project's build environment).
type Runner func(name string, args ...string) (string, error)

// DefaultRunner shells out for real via os/exec.
func DefaultRunner(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// Dataset is one row of `zfs list`.
type Dataset struct {
	Name       string
	Mountpoint string // "none" or "legacy" are ZFS sentinel values, not paths
	CanMount   string // "on", "off", "noauto"
	Type       string // "filesystem" or "volume"
	Mounted    bool

	// Used is the dataset's `used` property in bytes: physical, on-disk space
	// (post-compression, including data retained only by its own ZFS
	// snapshots), not the logical size of the files it contains. It exists to
	// weight a dataset's share of a multi-dataset zfs-scan run for an overall
	// progress/ETA estimate - the same kind of on-disk figure RunSnapshot
	// already uses for a single dataset's own progress bar (util.GetFSUsage),
	// just read directly off the property instead of re-derived via statfs.
	// A malformed/unparseable value is left as 0 rather than failing the
	// whole listing - it only ever affects a progress estimate, never
	// correctness.
	Used int64
}

// ListDatasets recursively lists every filesystem and volume under roots,
// sorted by Name. Sorting by name also orders parents before children
// (`"a"` < `"a/b"` lexically), which callers rely on for mount ordering.
func ListDatasets(run Runner, roots []string) ([]Dataset, error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("no roots given")
	}
	args := []string{"list", "-H", "-p", "-t", "filesystem,volume",
		"-o", "name,mountpoint,mounted,canmount,type,used", "-r"}
	args = append(args, roots...)

	out, err := run("zfs", args...)
	if err != nil {
		return nil, fmt.Errorf("zfs list failed: %w\n%s", err, out)
	}

	var datasets []Dataset
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 6 {
			return nil, fmt.Errorf("unexpected `zfs list` output line: %q", line)
		}
		used, _ := strconv.ParseInt(fields[5], 10, 64) // 0 on parse failure; see Used's doc comment
		datasets = append(datasets, Dataset{
			Name:       fields[0],
			Mountpoint: fields[1],
			Mounted:    fields[2] == "yes",
			CanMount:   fields[3],
			Type:       fields[4],
			Used:       used,
		})
	}

	sort.Slice(datasets, func(i, j int) bool { return datasets[i].Name < datasets[j].Name })
	return datasets, nil
}

// MountAt mounts a dataset at an arbitrary path via the generic `mount -t
// zfs -o zfsutil`, instead of `zfs mount` (which only knows how to mount a
// dataset at its own `mountpoint` property, refuses when that property is
// "none", and refuses outright if the dataset is already mounted anywhere).
// `-o zfsutil` tells the mount.zfs helper to register the mount with ZFS the
// same way `zfs mount` would, and ZFS-level properties (notably `readonly`)
// are still honored. This never reads or writes the dataset's `mountpoint`
// property.
//
// zfs-scan uses this for every dataset, not only ones without a configured
// mountpoint: mounting a fresh, isolated, empty directory - even for a
// dataset that's already mounted at its usual location - means nothing else
// can already be nested inside it, so a scan sees exactly that one dataset's
// own content, including anything that would otherwise be hidden underneath
// a child dataset's mount at the same path in the live tree.
func MountAt(run Runner, dataset, path string) error {
	out, err := run("mount", "-t", "zfs", "-o", "zfsutil", dataset, path)
	if err != nil {
		return fmt.Errorf("mount -t zfs -o zfsutil %s %s: %w\n%s", dataset, path, err, out)
	}
	return nil
}

// UnmountPath runs the generic `umount <path>`, targeting one specific mount
// point rather than a dataset name. zfs-scan may mount the same dataset a
// second time at a temporary location while it's already mounted elsewhere
// (deliberately - see MountAt) - `zfs unmount <dataset>` looks the mount up
// by dataset name and would be ambiguous about which instance to tear down
// in that case. Unmounting by path has no such ambiguity.
func UnmountPath(run Runner, path string) error {
	out, err := run("umount", path)
	if err != nil {
		return fmt.Errorf("umount %s: %w\n%s", path, err, out)
	}
	return nil
}
