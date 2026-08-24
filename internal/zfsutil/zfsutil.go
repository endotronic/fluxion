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
}

// ListDatasets recursively lists every filesystem and volume under roots,
// sorted by Name. Sorting by name also orders parents before children
// (`"a"` < `"a/b"` lexically), which callers rely on for mount ordering.
func ListDatasets(run Runner, roots []string) ([]Dataset, error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("no roots given")
	}
	args := []string{"list", "-H", "-p", "-t", "filesystem,volume",
		"-o", "name,mountpoint,mounted,canmount,type", "-r"}
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
		if len(fields) != 5 {
			return nil, fmt.Errorf("unexpected `zfs list` output line: %q", line)
		}
		datasets = append(datasets, Dataset{
			Name:       fields[0],
			Mountpoint: fields[1],
			Mounted:    fields[2] == "yes",
			CanMount:   fields[3],
			Type:       fields[4],
		})
	}

	sort.Slice(datasets, func(i, j int) bool { return datasets[i].Name < datasets[j].Name })
	return datasets, nil
}

// Mount runs `zfs mount <dataset>`.
func Mount(run Runner, dataset string) error {
	out, err := run("zfs", "mount", dataset)
	if err != nil {
		return fmt.Errorf("zfs mount %s: %w\n%s", dataset, err, out)
	}
	return nil
}

// Unmount runs `zfs unmount <dataset>`.
func Unmount(run Runner, dataset string) error {
	out, err := run("zfs", "unmount", dataset)
	if err != nil {
		return fmt.Errorf("zfs unmount %s: %w\n%s", dataset, err, out)
	}
	return nil
}
