package app

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"fluxion/internal/models"
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"
	"fluxion/internal/util"
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

	// ForceNew starts a brand-new snapshot for every dataset, ignoring any
	// completed, failed, or in-progress snapshot already recorded under that
	// dataset's name - the old row is kept as history, not overwritten.
	ForceNew bool

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

// mountTracker records which temporary directories are currently mounted, so
// the interrupt handler (running on its own goroutine) can see an accurate,
// race-free snapshot of what needs unmounting no matter where in the run a
// signal arrives.
type mountTracker struct {
	mu   sync.Mutex
	dirs []string
}

func (t *mountTracker) add(dir string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dirs = append(t.dirs, dir)
}

func (t *mountTracker) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.dirs...)
}

// unmountRetries/unmountRetryDelay bound how long unmountWithRetry waits out
// a transiently busy mount. Package vars (not consts) so tests can shrink
// the delay instead of actually sleeping several seconds per case.
var (
	unmountRetries    = 5
	unmountRetryDelay = time.Second
)

// unmountWithRetry retries a still-"target is busy" unmount a few times over
// a few seconds before giving up. A scan process that just finished (or was
// just interrupted mid-scan) can hold the mount open for a brief moment
// after RunSnapshot returns - a walker/hasher goroutine still unwinding, or
// the kernel not yet having dropped the last open file reference - and
// umount's EBUSY in that window is transient, not a real conflict. This was
// a real, observed failure: the very first unmount attempt right after a
// SIGINT lost the race and left the mount (and its temp directory) behind
// every time.
func unmountWithRetry(run zfsutil.Runner, dir string) error {
	var err error
	for attempt := 0; attempt < unmountRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(unmountRetryDelay)
		}
		if err = zfsutil.UnmountPath(run, dir); err == nil {
			return nil
		}
	}
	return err
}

// cleanupMounts unmounts and removes every directory in dirs, best-effort:
// a failure on one (after retrying) is logged and does not stop the rest.
// Shared by normal end-of-run teardown and by the interrupt handler, so both
// behave identically.
func cleanupMounts(run zfsutil.Runner, dirs []string) {
	for _, dir := range dirs {
		if err := unmountWithRetry(run, dir); err != nil {
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
}

// overallProgress prints run-wide progress across every dataset in a
// zfs-scan run, on its own line - each dataset's own progress bar/heartbeat
// (inside RunSnapshot) only ever knows about that one dataset, so without
// this there is no way to tell how far through a multi-dataset run you are
// or estimate when it will finish.
//
// Datasets are weighted by their ZFS `referenced` property: physical,
// on-disk bytes (post-compression) reachable through that dataset's own
// filesystem, not the logical size of the files inside them - the same kind
// of on-disk figure RunSnapshot's own per-dataset bar is scaled against
// (util.GetFSUsage), just read directly off the property instead of
// re-derived via statfs. `used` (cumulative down the tree) is deliberately
// NOT used here even though it sounds like the obvious choice: it already
// includes every descendant dataset's usage, so summing it across both a
// parent and its children - which is exactly what this runs across, since
// zfs-scan scans every dataset independently - multiply-counts the same
// physical bytes once per level of nesting. `referenced` doesn't have that
// problem: a container dataset with no files of its own reports it near
// zero. See zfsutil.Dataset.Referenced's doc comment for the fleet reading
// (205.6T reported vs. a 69.7T pool) that caught this. That mismatch against
// the logical bytes-processed count fed into line() means the percentage is
// an estimate, not an exact fraction - same caveat that already applies to
// each individual dataset's own progress bar today.
type overallProgress struct {
	start           time.Time
	totalDatasets   int
	totalBytes      int64
	bytesDoneBefore int64 // sum of Referenced for datasets already finished, success or failure
}

func newOverallProgress(datasets []zfsutil.Dataset) *overallProgress {
	p := &overallProgress{start: time.Now(), totalDatasets: len(datasets)}
	for _, ds := range datasets {
		p.totalBytes += ds.Referenced
	}
	return p
}

// datasetDone records that one dataset has finished, successfully or not -
// either way it no longer counts as remaining work.
func (p *overallProgress) datasetDone(ds zfsutil.Dataset) {
	p.bytesDoneBefore += ds.Referenced
}

// line renders the current overall-progress line. datasetsDone is how many
// datasets have finished so far (not counting whichever is currently being
// scanned, if any); currentProcessed is the bytes hashed/recorded so far for
// the in-progress dataset (0 between datasets, or when its size couldn't be
// weighted against the run total).
func (p *overallProgress) line(datasetsDone int, currentProcessed int64) string {
	done := p.bytesDoneBefore + currentProcessed
	elapsed := time.Since(p.start).Round(time.Second)

	line := fmt.Sprintf("-- overall: %d/%d datasets done", datasetsDone, p.totalDatasets)
	if p.totalBytes > 0 {
		pct := float64(done) / float64(p.totalBytes) * 100
		line += fmt.Sprintf(", %s / %s (%.1f%%)", util.FormatBytes(done), util.FormatBytes(p.totalBytes), pct)
	}
	line += fmt.Sprintf(", elapsed %s", formatDuration(elapsed))

	if eta, ok := estimateETA(elapsed, done, p.totalBytes); ok {
		line += fmt.Sprintf(", ETA ~%s", formatDuration(eta))
	}
	return line
}

// formatDuration renders d the way time.Duration.String() does (largest unit
// first, smaller units only when nonzero) except minutes and seconds are
// always zero-padded to 2 digits once a larger unit is shown - "4h05m08s",
// not "4h5m8s". time.Duration.String()'s minute/second digit count changes
// as the value crosses 10 (5s -> 15s -> ... rolling into 1m05s), which reads
// as the display "flapping" on a redrawn-in-place progress line where the
// same character position holds a different digit every tick. d is rounded
// to the nearest second first, so this is only meant for the second-or-finer
// granularity durations already used for ETA/elapsed display in this file -
// not sub-second durations, which fall through to the "Ns" case as 0s.
func formatDuration(d time.Duration) string {
	total := int64(d.Round(time.Second) / time.Second)
	if total < 0 {
		total = 0
	}
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// estimateETA predicts remaining time from a whole-run-so-far average rate
// (done/elapsed), not a short rolling window - deliberately simpler than
// schollz/progressbar's own built-in predictor, which samples throughput
// over short (sub-10s) windows and, worse, divides by zero whenever one of
// those windows sees no progress at all. That happens routinely here: a
// single large file produces no progress signal (no processedBytes update)
// for the entire time it's being hashed, so a short window can easily land
// entirely within one multi-minute stall. schollz's zero-rate division
// overflows through a float64->Duration conversion into a large negative
// number, which its own "< 0 means clamp to zero" guard then displays as a
// flatly wrong "ETA ~0s" for the rest of that stall - confirmed by direct
// reproduction against the vendored library. A whole-run average can't hit
// that failure mode (elapsed and done only ever grow), at the cost of
// reacting to a real slowdown/speedup more slowly - an acceptable trade
// given the alternative is a number that reads as more precise than it is
// while sometimes being outright wrong.
//
// ok is false (no estimate) when there isn't enough signal yet: no known
// total, no progress recorded, or under 2 elapsed seconds (too little time
// to divide by without a wild swing off the first sample).
func estimateETA(elapsed time.Duration, done, total int64) (eta time.Duration, ok bool) {
	if total <= 0 {
		return 0, false
	}
	rate, ok := averageRate(elapsed, done)
	if !ok {
		return 0, false
	}
	remaining := total - done
	if remaining < 0 {
		remaining = 0
	}
	return time.Duration(float64(remaining) / rate * float64(time.Second)).Round(time.Second), true
}

// averageRate returns a whole-run-so-far average throughput in bytes/second:
// done/elapsed, guarded the same way as estimateETA (which calls this) - no
// estimate before 2 elapsed seconds or before any progress at all, so it
// can't return a wild swing off the first sample or a divide-by-zero. Used
// directly (not just via estimateETA) to show a live rate figure next to the
// "Scanning..." bar: schollz/progressbar's own rate display is driven by the
// same short rolling-window average that made its ETA unreliable (see
// estimateETA's doc comment) - it silently goes blank rather than showing a
// wrong number, but "the rate disappears mid-scan" is still a worse user
// experience than a slower-to-react but always-present whole-run average.
func averageRate(elapsed time.Duration, done int64) (bytesPerSecond float64, ok bool) {
	if done <= 0 || elapsed < 2*time.Second {
		return 0, false
	}
	return float64(done) / elapsed.Seconds(), true
}

func (p *overallProgress) print(datasetsDone int, currentProcessed int64) {
	fmt.Println(p.line(datasetsDone, currentProcessed))
}

// handleInterrupt runs the same cleanup as normal end-of-run teardown against
// whatever's mounted at the moment of interruption, and returns the
// conventional 128+signal exit code for the caller to exit with.
func handleInterrupt(sig os.Signal, run zfsutil.Runner, dirs []string) int {
	logrus.Warnf("received %s - unmounting %d temporary mount(s) before exiting...", sig, len(dirs))
	cleanupMounts(run, dirs)
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 1
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
// Every mount this run creates is torn down afterward - including on
// SIGINT/SIGTERM, see the interrupt handler installed below.
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

	var mounted mountTracker

	// stopCh is closed on SIGINT/SIGTERM, before cleanup runs, so the
	// currently in-flight RunSnapshot call (if any) stops touching its mount
	// promptly instead of continuing to scan for however long that dataset
	// takes - see the interrupt handler below for why this matters: without
	// it, cleanupMounts's retry loop was racing a scan that never stopped,
	// and could never win.
	stopCh := make(chan struct{})

	// Interrupt handling: a real run can hold open temporary mounts for
	// hours (every dataset is mounted upfront, scanned in turn, then all
	// unmounted at the end - see below). Without this, ^C or a `kill` mid-run
	// leaves those mounts and their temp directories orphaned, since Go runs
	// no cleanup on an unhandled SIGINT/SIGTERM. A second signal while
	// cleanup is in flight forces an immediate exit, in case `umount` itself
	// is stuck on a wedged mount.
	if !cfg.DryRun {
		sigCh := make(chan os.Signal, 2)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigCh)

		done := make(chan struct{})
		defer close(done)

		go func() {
			select {
			case sig := <-sigCh:
				go func() {
					<-sigCh
					os.Exit(1)
				}()
				// Stop the current dataset's scan before attempting to
				// unmount anything: closing stopCh makes RunSnapshot's
				// walker/workers stand down within roughly one in-flight
				// file's hash time, instead of running for however much
				// longer that dataset's scan would otherwise take. Without
				// this, the mount cleanupMounts retries against stays busy
				// for the rest of the run, not just briefly - no amount of
				// retrying wins that race.
				close(stopCh)
				os.Exit(handleInterrupt(sig, run, mounted.snapshot()))
			case <-done:
			}
		}()
	}

	type planned struct {
		dataset    zfsutil.Dataset
		mountpoint string
		resumeFrom bool // an in-progress snapshot for this dataset should be resumed rather than started fresh
	}

	// First pass: split off the datasets that will never be scanned by this
	// command at all (excluded, a zvol, or a container dataset) from the
	// ones that are eligible to count as "this pool's work" - whether
	// already done, blocked by a prior failure, or still to be mounted and
	// scanned below. eligible is what overallProgress is weighted against,
	// so a re-run against an already-mostly-scanned root reports honest
	// overall progress from the very first line instead of only ever
	// showing progress on whatever happens to be left this invocation.
	var eligible []zfsutil.Dataset
	for _, ds := range datasets {
		switch {
		case excluded(ds.Name, cfg.ExcludeDatasets):
			fmt.Printf("  skip  %-55s excluded\n", ds.Name)
			res.Skipped = append(res.Skipped, ds.Name)
		case ds.Type == "volume":
			fmt.Printf("  skip  %-55s not a filesystem (zvol)\n", ds.Name)
			res.Skipped = append(res.Skipped, ds.Name)
		case ds.CanMount == "off" && !cfg.IncludeCanMountOff:
			fmt.Printf("  skip  %-55s canmount=off (use --include-canmount-off to scan it anyway)\n", ds.Name)
			res.Skipped = append(res.Skipped, ds.Name)
		default:
			eligible = append(eligible, ds)
		}
	}

	if cfg.DryRun {
		for _, ds := range eligible {
			if ds.Mounted {
				fmt.Printf("  plan  %-55s already mounted (mountpoint=%s); mount a second, isolated copy at a temporary location + scan\n", ds.Name, ds.Mountpoint)
			} else {
				fmt.Printf("  plan  %-55s mount at temporary location + scan\n", ds.Name)
			}
		}
		return res, nil
	}

	progress := newOverallProgress(eligible)
	doneCount := 0 // datasets already accounted for in progress before the scan loop below starts

	var toScan []planned
	for _, ds := range eligible {
		resumeFrom := false
		if dbStore != nil {
			existing, _ := dbStore.FindSnapshot(ds.Name)
			if existing != nil {
				if cfg.ForceNew {
					fmt.Printf("  new   %-55s --new: starting a fresh snapshot (previous #%d, %s, kept as history)\n", ds.Name, existing.ID, existing.Status)
				} else {
					switch existing.Status {
					case models.StatusCompleted:
						fmt.Printf("  skip  %-55s already scanned (snapshot #%d)\n", ds.Name, existing.ID)
						res.AlreadyDone = append(res.AlreadyDone, ds.Name)
						progress.datasetDone(ds)
						doneCount++
						continue
					case models.StatusFailed:
						fmt.Printf("  skip  %-55s previous scan failed (snapshot #%d) - delete it to retry\n", ds.Name, existing.ID)
						res.Failed = append(res.Failed, ds.Name)
						res.HadFailures = true
						progress.datasetDone(ds)
						doneCount++
						continue
					default:
						// in_progress: resume it by name, rather than by
						// this run's target dir - each run mounts a brand
						// new temp directory (see below), so a path-based
						// resume lookup would never find a snapshot from an
						// earlier, interrupted run.
						resumeFrom = true
					}
				}
			}
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
			progress.datasetDone(ds)
			doneCount++
			continue
		}
		if err := zfsutil.MountAt(run, ds.Name, dir); err != nil {
			os.Remove(dir)
			logrus.Errorf("failed to mount %s: %v", ds.Name, err)
			fmt.Printf("  fail  %-55s mount failed: %v\n", ds.Name, err)
			res.Failed = append(res.Failed, ds.Name)
			res.HadFailures = true
			progress.datasetDone(ds)
			doneCount++
			continue
		}
		mounted.add(dir)

		toScan = append(toScan, planned{dataset: ds, mountpoint: dir, resumeFrom: resumeFrom})
	}

	for i, p := range toScan {
		fmt.Printf("\n=== %s (%s) ===\n", p.dataset.Name, p.mountpoint)
		progress.print(doneCount+i, 0)
		snapCfg := SnapshotConfig{
			TargetDir:      p.mountpoint,
			DBPath:         cfg.DBPath,
			Name:           p.dataset.Name,
			Threads:        cfg.Threads,
			CrossMounts:    false,
			ComputeMD5:     cfg.ComputeMD5,
			NonInteractive: true,
			StopCh:         stopCh,
			OverallLine: func(processed int64) string {
				return progress.line(doneCount+i, processed)
			},
		}
		if p.resumeFrom {
			snapCfg.ResumeFrom = p.dataset.Name
		}
		if cfg.ForceNew {
			snapCfg.AllowDuplicateName = true
		}
		err := RunSnapshot(snapCfg)
		if errors.Is(err, ErrScanInterrupted) {
			// The interrupt handler (running on its own goroutine) is
			// already closing in on cleanupMounts + os.Exit; stop iterating
			// instead of racing it into the next dataset's mount/scan.
			return res, nil
		}
		progress.datasetDone(p.dataset)
		if err != nil {
			logrus.Errorf("scan of %s failed: %v", p.dataset.Name, err)
			res.Failed = append(res.Failed, p.dataset.Name)
			res.HadFailures = true
		} else {
			res.Scanned = append(res.Scanned, p.dataset.Name)
		}
		progress.print(doneCount+i+1, 0)
	}

	// Unmount everything we mounted, by path rather than dataset name. A
	// dataset may already have been mounted elsewhere before this run (see
	// above), so `zfs unmount <dataset>` - which looks the mount up by
	// dataset name - could target the wrong instance. Unmounting the exact
	// path we mounted has no such ambiguity. Order doesn't matter: every
	// mount here is its own freshly created, empty directory, never nested
	// inside another one of them.
	cleanupMounts(run, mounted.snapshot())

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
