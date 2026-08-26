package app

import (
	"fmt"
	"os"
	"strings"

	"fluxion/internal/models"
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"
	"fluxion/internal/zfsutil"

	"github.com/sirupsen/logrus"
)

type ZFSScanConfig struct {
	DBPath             string
	Roots              []string
	Threads            int
	ComputeMD5         bool
	DryRun             bool
	ExcludeDatasets    []string
	IncludeCanMountOff bool // also mount+scan canmount=off datasets, instead of skipping them

	// Runner is injected so tests can substitute a fake `zfs`/`mount`; nil
	// means the real thing.
	Runner zfsutil.Runner
}

// ZFSScanResult is the outcome of one zfs-scan run, dataset names bucketed by
// what happened to them.
type ZFSScanResult struct {
	Scanned     []string
	AlreadyDone []string
	Skipped     []string // expected, non-fatal: not a filesystem, canmount=off (unless included), excluded
	Failed      []string // mount failure, scan error, or blocked by a prior failed snapshot

	// HadFailures is true if anything in Failed is non-empty - the run did not
	// fully cover what it was asked to scan.
	HadFailures bool
}

// RunZFSScan enumerates every dataset under cfg.Roots and scans each into its
// own Fluxion snapshot (named after the dataset) with cross-mounts disabled.
//
// Every dataset is mounted at a fresh, temporary, isolated directory via
// `mount -t zfs -o zfsutil` (zfsutil.MountAt) - even one that's already
// mounted at its usual location. This is deliberate, not just for datasets
// that lack a mountpoint: a dedicated, empty temp directory can never have
// anything else already mounted inside it, so the scan sees exactly that one
// dataset's own on-disk content, including any files left behind underneath
// wherever a child dataset happens to be mounted in the live tree (which
// would otherwise shadow them). It also means the scanner's mount-boundary
// logic never has to reason about the fleet's normal mount layout at all.
// Every mount this run creates is torn down afterward.
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
	}
	var toScan []planned
	var mountedDirs []string // temp dirs we mounted, for teardown

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
		if ds.CanMount == "off" && !cfg.IncludeCanMountOff {
			fmt.Printf("  skip  %-55s canmount=off (use --include-canmount-off to scan it anyway)\n", ds.Name)
			res.Skipped = append(res.Skipped, ds.Name)
			continue
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

		if cfg.DryRun {
			if ds.Mounted {
				fmt.Printf("  plan  %-55s already mounted (mountpoint=%s); mount a second, isolated copy at a temporary location + scan\n", ds.Name, ds.Mountpoint)
			} else {
				fmt.Printf("  plan  %-55s mount at temporary location + scan\n", ds.Name)
			}
			toScan = append(toScan, planned{dataset: ds})
			continue
		}

		if ds.Mounted {
			logrus.Infof("%s is already mounted (mountpoint=%s); mounting an additional, isolated copy for this scan so nothing shadowed by a nested mount is missed", ds.Name, ds.Mountpoint)
		}

		dir, err := os.MkdirTemp("", "fluxion-zfsscan-")
		if err != nil {
			logrus.Errorf("failed to create temp mount dir for %s: %v", ds.Name, err)
			fmt.Printf("  fail  %-55s could not create temp mount dir: %v\n", ds.Name, err)
			res.Failed = append(res.Failed, ds.Name)
			res.HadFailures = true
			continue
		}
		if err := zfsutil.MountAt(run, ds.Name, dir); err != nil {
			os.Remove(dir)
			logrus.Errorf("failed to mount %s: %v", ds.Name, err)
			fmt.Printf("  fail  %-55s mount failed: %v\n", ds.Name, err)
			res.Failed = append(res.Failed, ds.Name)
			res.HadFailures = true
			continue
		}
		mountedDirs = append(mountedDirs, dir)

		toScan = append(toScan, planned{dataset: ds, mountpoint: dir})
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

	// Unmount everything we mounted, by path rather than dataset name. A
	// dataset may already have been mounted elsewhere before this run (see
	// above), so `zfs unmount <dataset>` - which looks the mount up by
	// dataset name - could target the wrong instance. Unmounting the exact
	// path we mounted has no such ambiguity. Order doesn't matter: every
	// mount here is its own freshly created, empty directory, never nested
	// inside another one of them.
	for _, dir := range mountedDirs {
		if err := zfsutil.UnmountPath(run, dir); err != nil {
			logrus.Warnf("failed to unmount %s (left mounted): %v", dir, err)
			continue
		}
		// Only remove the directory itself (never RemoveAll) - if unmount
		// somehow left content behind, os.Remove fails safely on a
		// non-empty directory instead of deleting anything inside it.
		if err := os.Remove(dir); err != nil {
			logrus.Warnf("failed to remove temporary mount directory %s: %v", dir, err)
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
