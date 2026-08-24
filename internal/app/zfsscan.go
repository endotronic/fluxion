package app

import (
	"fmt"
	"sort"
	"strings"

	"fluxion/internal/models"
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"
	"fluxion/internal/util"
	"fluxion/internal/zfsutil"

	"github.com/sirupsen/logrus"
)

type ZFSScanConfig struct {
	DBPath          string
	Roots           []string
	Threads         int
	ComputeMD5      bool
	DryRun          bool
	ExcludeDatasets []string

	// Runner is injected so tests can substitute a fake `zfs`/`mount`; nil
	// means the real thing.
	Runner zfsutil.Runner
}

// ZFSScanResult is the outcome of one zfs-scan run, dataset names bucketed by
// what happened to them.
type ZFSScanResult struct {
	Scanned     []string
	AlreadyDone []string
	Skipped     []string // expected, non-fatal: not a filesystem, canmount=off, excluded, no mountpoint
	Failed      []string // mount failure, scan error, or blocked by a prior failed snapshot

	// HadFailures is true if anything in Failed is non-empty - the run did not
	// fully cover what it was asked to scan.
	HadFailures bool
}

// RunZFSScan enumerates every dataset under cfg.Roots, mounts whichever are
// not already mounted, scans each into its own Fluxion snapshot (named after
// the dataset) with cross-mounts disabled, and unmounts whatever it mounted.
//
// It does not read ZFS's own snapshot history - see knowledge/fleet.md
// "Verdict: do NOT build per-snapshot scanning". This only drives repeated,
// present-moment `RunSnapshot` calls, one per dataset.
func RunZFSScan(cfg ZFSScanConfig) (ZFSScanResult, error) {
	var res ZFSScanResult

	if cfg.DBPath == "" {
		return res, fmt.Errorf("DB path is required")
	}
	if len(cfg.Roots) == 0 {
		return res, fmt.Errorf("at least one pool or dataset is required")
	}

	run := cfg.Runner
	if run == nil {
		run = zfsutil.DefaultRunner
	}

	datasets, err := zfsutil.ListDatasets(run, cfg.Roots)
	if err != nil {
		return res, fmt.Errorf("error listing datasets: %w", err)
	}

	var dbStore store.Store
	if !cfg.DryRun {
		dbStore, err = sqlite.NewSqliteStore(cfg.DBPath)
		if err != nil {
			return res, fmt.Errorf("error opening DB: %w", err)
		}
		defer dbStore.Close()
	}

	type planned struct {
		dataset    zfsutil.Dataset
		mountpoint string
		weMounted  bool
	}
	var toScan []planned
	var mountedByUs []string // dataset names, in the order we mounted them

	for _, ds := range datasets {
		if excluded(ds.Name, cfg.ExcludeDatasets) {
			fmt.Printf("  skip  %-55s excluded\n", ds.Name)
			res.Skipped = append(res.Skipped, ds.Name)
			continue
		}
		if ds.Type == "volume" {
			fmt.Printf("  skip  %-55s not a filesystem (zvol)\n", ds.Name)
			res.Skipped = append(res.Skipped, ds.Name)
			continue
		}
		if ds.CanMount == "off" {
			fmt.Printf("  skip  %-55s container dataset, no files of its own\n", ds.Name)
			res.Skipped = append(res.Skipped, ds.Name)
			continue
		}

		mountpoint := ds.Mountpoint
		if mountpoint == "none" {
			fmt.Printf("  skip  %-55s no mountpoint set\n", ds.Name)
			res.Skipped = append(res.Skipped, ds.Name)
			continue
		}
		if mountpoint == "legacy" {
			found := ""
			if mp, err := findLegacyMount(ds.Name); err == nil {
				found = mp
			}
			if found == "" {
				fmt.Printf("  skip  %-55s legacy mountpoint, not currently mounted\n", ds.Name)
				res.Skipped = append(res.Skipped, ds.Name)
				continue
			}
			mountpoint = found
			ds.Mounted = true
		}

		if !cfg.DryRun && dbStore != nil {
			existing, _ := dbStore.FindSnapshot(ds.Name)
			if existing != nil {
				switch existing.Status {
				case models.StatusCompleted:
					fmt.Printf("  skip  %-55s already scanned (snapshot #%d)\n", ds.Name, existing.ID)
					res.AlreadyDone = append(res.AlreadyDone, ds.Name)
					continue
				case models.StatusFailed:
					fmt.Printf("  skip  %-55s previous scan failed (snapshot #%d) - delete it to retry\n", ds.Name, existing.ID)
					res.Failed = append(res.Failed, ds.Name)
					res.HadFailures = true
					continue
				}
				// in_progress: fall through, RunSnapshot will resume it.
			}
		}

		weMounted := false
		if !ds.Mounted {
			if cfg.DryRun {
				fmt.Printf("  plan  %-55s mount + scan\n", ds.Name)
			} else {
				if err := zfsutil.Mount(run, ds.Name); err != nil {
					logrus.Errorf("failed to mount %s: %v", ds.Name, err)
					fmt.Printf("  fail  %-55s mount failed: %v\n", ds.Name, err)
					res.Failed = append(res.Failed, ds.Name)
					res.HadFailures = true
					continue
				}
				weMounted = true
				mountedByUs = append(mountedByUs, ds.Name)
			}
		} else if cfg.DryRun {
			fmt.Printf("  plan  %-55s already mounted, scan\n", ds.Name)
		}

		toScan = append(toScan, planned{dataset: ds, mountpoint: mountpoint, weMounted: weMounted})
	}

	if cfg.DryRun {
		return res, nil
	}

	for _, p := range toScan {
		fmt.Printf("\n=== %s (%s) ===\n", p.dataset.Name, p.mountpoint)
		err := RunSnapshot(SnapshotConfig{
			TargetDir:      p.mountpoint,
			DBPath:         cfg.DBPath,
			Name:           p.dataset.Name,
			Threads:        cfg.Threads,
			CrossMounts:    false,
			ComputeMD5:     cfg.ComputeMD5,
			NonInteractive: true,
		})
		if err != nil {
			logrus.Errorf("scan of %s failed: %v", p.dataset.Name, err)
			res.Failed = append(res.Failed, p.dataset.Name)
			res.HadFailures = true
		} else {
			res.Scanned = append(res.Scanned, p.dataset.Name)
		}
	}

	// Unmount everything we mounted, deepest child first, so a still-mounted
	// child never blocks unmounting its parent.
	sort.Sort(sort.Reverse(sort.StringSlice(mountedByUs)))
	for _, name := range mountedByUs {
		if err := zfsutil.Unmount(run, name); err != nil {
			logrus.Warnf("failed to unmount %s (left mounted): %v", name, err)
		}
	}

	printZFSScanSummary(res)
	return res, nil
}

func printZFSScanSummary(res ZFSScanResult) {
	fmt.Println()
	fmt.Println("Summary")
	fmt.Printf("  scanned:       %d\n", len(res.Scanned))
	fmt.Printf("  already done:  %d\n", len(res.AlreadyDone))
	fmt.Printf("  skipped:       %d\n", len(res.Skipped))
	if len(res.Failed) > 0 {
		fmt.Printf("  FAILED:        %d  (%s)\n", len(res.Failed), strings.Join(res.Failed, ", "))
	}
}

// findLegacyMount looks up where a `mountpoint=legacy` dataset is currently
// mounted, if anywhere: ZFS doesn't know a legacy dataset's path itself, but
// /proc/mounts records the dataset name as the mount source verbatim.
func findLegacyMount(dataset string) (string, error) {
	mounts, err := util.GetMounts()
	if err != nil {
		return "", err
	}
	for _, m := range mounts {
		if m.Source == dataset {
			return m.Target, nil
		}
	}
	return "", fmt.Errorf("not mounted")
}

// excluded reports whether name is excluded by an exact match or a
// path-boundary-aware prefix (prefix or prefix + "/"), unlike the raw
// strings.HasPrefix bug documented for --exclude on diff/coverage.
func excluded(name string, excludes []string) bool {
	for _, ex := range excludes {
		if name == ex || strings.HasPrefix(name, ex+"/") {
			return true
		}
	}
	return false
}
