package app

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fluxion/internal/consts"
	"fluxion/internal/models"
	"fluxion/internal/scanner"
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"
	"fluxion/internal/util"

	"github.com/mattn/go-isatty"
	"github.com/schollz/progressbar/v3"
	"github.com/sirupsen/logrus"
)

// progressWriter discards the progress bar's carriage-return-driven redraws
// when stderr isn't a terminal (piped to `tee`/a log file, as zfs-scan runs
// often are), instead of filling the capture with unreadable \r noise.
func progressWriter(quiet bool) io.Writer {
	if quiet {
		return io.Discard
	}
	return os.Stderr
}

type SnapshotConfig struct {
	TargetDir      string
	DBPath         string
	Name           string
	Threads        int
	ForceNew       bool
	ResumeFrom     string
	Hostname       string
	CrossMounts    bool
	FailOnMount    bool
	ComputeMD5     bool
	SkipEstimation bool
	EstimateOnly   bool

	// NonInteractive skips the implicit in-progress resume prompt and always
	// resumes, for callers driving many scans unattended (zfs-scan).
	NonInteractive bool

	// OnHeartbeat, if set, is called every ~30s for the duration of the scan
	// with the number of bytes processed (hashed/recorded) so far - the same
	// value driving this scan's own progress bar. It exists for a caller
	// running several scans in sequence (zfs-scan) to print its own,
	// run-wide progress on top of this one dataset's, since nothing else
	// gives it a periodic hook into an in-progress RunSnapshot call.
	OnHeartbeat func(processedBytes int64)

	// AllowDuplicateName lets a new snapshot be created under a name that's
	// already used by an earlier snapshot in this DB. snapshots.name is
	// unique in the schema, so instead of failing with "already exists",
	// the existing row holding that name is renamed out of the way (kept as
	// history, not deleted) before the new one is created under it. Only
	// meaningful when Name is set explicitly (see explicitName below) and
	// mode ends up "new" - e.g. zfs-scan's --new, which always names a
	// dataset's snapshot after the dataset itself and needs to be able to
	// start over even when a completed/failed/in-progress snapshot already
	// holds that exact name.
	AllowDuplicateName bool
}

func RunSnapshot(cfg SnapshotConfig) error {
	targetDir, err := filepath.Abs(cfg.TargetDir)
	if err != nil {
		return fmt.Errorf("error getting abs path: %w", err)
	}

	quiet := !isatty.IsTerminal(os.Stderr.Fd())

	dbPath := cfg.DBPath
	if dbPath == "" {
		base := filepath.Base(targetDir)
		if base == "." || base == "/" {
			base = "filesystem"
		}
		dbPath = base + ".db"
	}

	// Open DB
	var dbStore store.Store
	dbStore, err = sqlite.NewSqliteStore(dbPath)
	if err != nil {
		return fmt.Errorf("error opening DB: %w", err)
	}
	defer dbStore.Close()

	var snapshotID int64

	var resumeMap map[string]models.FileRecord
	mode := "new"
	var lastSnap *models.Snapshot

	if cfg.ResumeFrom != "" {
		// Explicit Resume
		lastSnap, err = dbStore.FindSnapshot(cfg.ResumeFrom)
		if err != nil {
			return fmt.Errorf("could not find snapshot '%s' to resume: %w", cfg.ResumeFrom, err)
		}

		if lastSnap.Status == models.StatusFailed {
			return fmt.Errorf("cannot resume failed snapshot '%s'", lastSnap.Name)
		}

		if lastSnap.Status == models.StatusCompleted {
			logrus.Warnf("Snapshot '%s' is already completed.", lastSnap.Name)
			fmt.Print("Do you want to re-scan and add to it? (y/N): ")
			reader := bufio.NewReader(os.Stdin)
			text, _ := reader.ReadString('\n')
			text = strings.TrimSpace(strings.ToLower(text))
			if text != "y" && text != "yes" {
				return nil
			}
		}
		// InProgress or User confirmed
	} else {
		// Auto-detect last snapshot
		lastSnap, err = dbStore.GetLastSnapshot(targetDir)
		if err != nil {
			return fmt.Errorf("error checking snapshots: %w", err)
		}
	}

	if lastSnap != nil {
		doResume := false

		// 1. Explicit Resume?
		if cfg.ResumeFrom != "" {
			doResume = true
		} else if lastSnap.Status == models.StatusInProgress {
			// 2. Implicit Resume (In Progress)
			if cfg.ForceNew {
				doResume = false
			} else if cfg.NonInteractive {
				doResume = true
			} else {
				// Prompt
				fmt.Printf("Found incomplete snapshot started at %s. Resume? [y/N]: ", lastSnap.StartedAt)
				reader := bufio.NewReader(os.Stdin)
				text, _ := reader.ReadString('\n')
				text = strings.TrimSpace(strings.ToLower(text))
				if text == "y" || text == "yes" {
					doResume = true
				}
			}
		}

		if doResume {
			mode = "resume"
			snapshotID = lastSnap.ID
			logrus.Info("Resuming snapshot...")

			// Get total files for progress bar
			totalFiles, err := dbStore.GetFileCount(snapshotID)
			if err != nil {
				logrus.Warnf("Failed to get file count: %v", err)
				totalFiles = -1
			}

			bar := progressbar.NewOptions64(
				totalFiles,
				progressbar.OptionSetDescription("Loading existing"),
				progressbar.OptionSetWriter(progressWriter(quiet)),
				progressbar.OptionSetWidth(10),
				progressbar.OptionShowTotalBytes(true),
				progressbar.OptionThrottle(65*time.Millisecond),
				progressbar.OptionShowCount(),
				progressbar.OptionShowIts(),
				progressbar.OptionOnCompletion(func() {
					if !quiet {
						fmt.Fprint(os.Stderr, "\n")
					}
				}),
				progressbar.OptionSpinnerType(14),
				progressbar.OptionSetRenderBlankState(true),
			)

			if totalFiles > 0 {
				logrus.Infof("Loading %d existing files...", totalFiles)
			} else {
				logrus.Info("Loading existing map...")
			}

			resumeMap, err = dbStore.GetFilesForSnapshot(snapshotID, func(count int) {
				if bar != nil {
					bar.Set(count)
				}
			})
			if err != nil {
				return fmt.Errorf("error loading resume data: %w", err)
			}
			if bar != nil {
				bar.Finish()
				fmt.Println()
			}
			logrus.Infof("Already processed %d files. Skipping them.", len(resumeMap))
		} else {
			// Start new (abandon old)
			if !cfg.ForceNew && lastSnap.Status == models.StatusCompleted {
				logrus.Infof("Found previous completed snapshot from %v. Starting new scan.", lastSnap.FinishedAt)
			}
		}
	}
	// Determine Base Name and Uniqueness Strategy
	baseName := cfg.Name
	explicitName := true
	if baseName == "" {
		explicitName = false
		base := filepath.Base(targetDir)
		if base == "." || base == "/" {
			base = "root"
		}
		baseName = fmt.Sprintf("%s_%s", base, time.Now().Format("2006-01-02"))
	}

	// Resolve final name
	finalName := baseName
	if mode == "new" {
		// AllowDuplicateName: baseName is about to be reused (zfs-scan's
		// --new re-scanning a dataset it already has history for, always
		// under that dataset's exact name). snapshots.name is unique in the
		// schema, so whatever currently holds that name has to be renamed
		// out of the way first - it's kept as history, not deleted, but the
		// name is what FindSnapshot/GetLastSnapshot resolve by, and the new
		// scan should be what they find from here on.
		if cfg.AllowDuplicateName && explicitName {
			if existing, _ := dbStore.FindSnapshot(baseName); existing != nil {
				supersededName := fmt.Sprintf("%s_superseded_%d", baseName, existing.ID)
				if err := dbStore.RenameSnapshot(existing.ID, supersededName); err != nil {
					return fmt.Errorf("error renaming previous snapshot '%s' out of the way: %w", baseName, err)
				}
			}
		}

		var err error
		finalName, err = getUniqueSnapshotName(dbStore, baseName, explicitName)
		if err != nil {
			return fmt.Errorf("error resolving snapshot name: %w", err)
		}
	}

	// Resolve Hostname
	hostname := cfg.Hostname
	if hostname == "" {
		h, err := os.Hostname()
		if err == nil {
			hostname = h
		} else {
			hostname = "unknown"
		}
	}

	if mode == "new" && !cfg.EstimateOnly {
		snap, err := dbStore.CreateSnapshot(targetDir, finalName, hostname)
		if err != nil {
			return fmt.Errorf("error creating snapshot: %w", err)
		}
		snapshotID = snap.ID
		logrus.Infof("Created new snapshot ID %d (Name: %s) [Host: %s]", snapshotID, finalName, hostname)
	}

	// Run Scan
	results := make(chan scanner.ScanResult, cfg.Threads)

	// Progress Reporting
	var foundCount atomic.Int64
	var foundBytes atomic.Int64
	var processedBytes atomic.Int64
	var processedCount atomic.Int64

	// Files the scan could not read, and batches that failed to write. Both mean
	// the snapshot is missing data, and a snapshot missing data is dangerous: the
	// files it never recorded will look like deletions the next time it is
	// diffed. We count them here and refuse to mark the snapshot completed below.
	var scanErrors atomic.Int64
	var writeErrors atomic.Int64
	var errSamplesMu sync.Mutex
	var errSamples []string
	const maxErrSamples = 10
	recordErr := func(err error) {
		scanErrors.Add(1)
		errSamplesMu.Lock()
		defer errSamplesMu.Unlock()
		if len(errSamples) < maxErrSamples {
			errSamples = append(errSamples, err.Error())
		}
	}

	walkDone := make(chan bool)
	done := make(chan bool)

	scanConfig := scanner.ScannerConfig{
		RootPath:    targetDir,
		SnapshotID:  snapshotID,
		NumWorkers:  cfg.Threads,
		ResumeMap:   resumeMap,
		CrossMounts: cfg.CrossMounts,
		FailOnMount: cfg.FailOnMount,
		ComputeMD5:  cfg.ComputeMD5,
		OnFileFound: func(path string, size int64) {
			foundCount.Add(1)
			foundBytes.Add(size)
		},
		OnWalkComplete: func() {
			close(walkDone)
		},
	}

	// Estimate size
	// Estimate size
	var estimatedTotal int64 = -1
	var used uint64
	var errEst error
	var subMounts []string

	isMount, _ := util.IsMountPoint(targetDir)
	if !isMount && cfg.EstimateOnly {
		return fmt.Errorf("Cannot estimate size for non-mount point: %s", targetDir)
	}

	if isMount && (!cfg.SkipEstimation || cfg.EstimateOnly) {
		if cfg.CrossMounts {
			used, _, subMounts, errEst = util.GetRecursiveFSUsage(targetDir)
		} else {
			used, _, errEst = util.GetFSUsage(targetDir)
		}

		if errEst == nil && used > 0 {
			estimatedTotal = int64(used)
			mountMsg := ""
			if len(subMounts) > 0 {
				mountMsg = fmt.Sprintf(" (including %d sub-mounts: %v)", len(subMounts), subMounts)
			}
			if !cfg.EstimateOnly {
				logrus.Infof("Estimated scan size based on FS usage: %s%s", util.FormatBytes(estimatedTotal), mountMsg)
			}
		} else if cfg.EstimateOnly {
			return fmt.Errorf("estimation failed: %v", errEst)
		}
	}

	if isMount && cfg.EstimateOnly {
		fmt.Printf("Estimating scan size for: %s\n", targetDir)
		if cfg.CrossMounts {
			fmt.Println("Cross-mounts: ENABLED (recursive check)")
		} else {
			fmt.Println("Cross-mounts: DISABLED (single filesystem)")
		}

		var alreadyScanned int64
		if mode == "resume" {
			var err error
			alreadyScanned, err = dbStore.GetSnapshotBytes(snapshotID)
			if err != nil {
				logrus.Warnf("Failed to get already scanned bytes: %v", err)
			}
		}

		remaining := estimatedTotal - alreadyScanned
		if remaining < 0 {
			remaining = 0
		}

		// We don't have total capacity here easily from GetFSUsage helper return (it returns used, totalCapacity).
		// Note: util.GetFSUsage returns (used, totalCapacity).
		// My earlier edit to RunSnapshot only captured `used`.
		// I need to adjust the call above to capture `totalCapacity` if I want to show it.
		// For now, let's just show the estimated scan size (Used space).

		fmt.Println("----------------------------------------------------------------")
		fmt.Printf("Total Estimate:   %s\n", util.FormatBytes(estimatedTotal))
		if mode == "resume" {
			fmt.Printf("Already Scanned:  %s\n", util.FormatBytes(alreadyScanned))
			fmt.Printf("Remaining:        %s\n", util.FormatBytes(remaining))
		}
		fmt.Println("----------------------------------------------------------------")

		if len(subMounts) > 0 {
			fmt.Printf("Found %d sub-mount paths included in this estimate:\n", len(subMounts))
			for _, m := range subMounts {
				fmt.Printf("  - %s\n", m)
			}
		}

		return nil
	}

	// Prepare Bar
	// We want 2 modes: Indeterminate (while walking), then Determinate (when walk done)
	bar := progressbar.NewOptions64(
		estimatedTotal,
		progressbar.OptionSetDescription("Scanning..."),
		progressbar.OptionSetWriter(progressWriter(quiet)),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(15),
		progressbar.OptionThrottle(65*time.Millisecond),
		progressbar.OptionShowCount(),
		progressbar.OptionOnCompletion(func() {
			if !quiet {
				fmt.Fprint(os.Stderr, "\n")
			}
		}),
		progressbar.OptionSpinnerType(14),
	)

	// UI Loop
	totalBuffer := float64(scanConfig.NumWorkers * consts.ScannerChannelBufferMultiplier)
	uiDone := make(chan bool)
	go func() {
		defer close(uiDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		// When the progress bar itself is discarded (piped/non-TTY stderr),
		// nothing else stands in for it - a multi-hour scan would otherwise go
		// completely silent between the "Estimated scan size" line and
		// "Finished", which reads as a hang. Log the same status periodically
		// instead, at a rate sane for a log file rather than a terminal.
		//
		// The same ticker also drives cfg.OnHeartbeat (e.g. zfs-scan's overall
		// run progress line), so it runs regardless of quiet whenever a caller
		// has asked for it - a TTY still gets its live per-dataset bar, but a
		// multi-dataset run has no other periodic hook to piggyback an
		// overall-progress line onto.
		var heartbeat <-chan time.Time
		if quiet || cfg.OnHeartbeat != nil {
			hb := time.NewTicker(30 * time.Second)
			defer hb.Stop()
			heartbeat = hb.C
		}

		walking := true

		// walkDone is closed rather than sent to, so once the walk finishes this
		// case is permanently ready. Nil the local copy after the first receive:
		// a nil channel blocks forever, which stops the select from spinning at
		// full tilt for the whole hashing phase.
		walkSignal := walkDone

		for {
			select {
			case <-walkSignal:
				walkSignal = nil
				walking = false
				// Switch to determinate
				bar.ChangeMax64(foundBytes.Load())
			case <-ticker.C:
				current := processedBytes.Load()
				if walking {
					found := foundCount.Load()
					delta := foundCount.Load() - processedCount.Load()
					desc := fmt.Sprintf("Scanning (Found %d/%s)...", found, util.FormatBytes(foundBytes.Load()))

					saturation := float64(delta) / totalBuffer
					if saturation > 0.5 {
						desc = fmt.Sprintf("Scanning (Found %d/%s) [%.0f%% Saturated]...", found, util.FormatBytes(foundBytes.Load()), saturation*100)
					}

					bar.Describe(desc)
				} else {
					desc := fmt.Sprintf("Found (%d). Hashing (%d done)...", foundCount.Load(), processedCount.Load())
					bar.Describe(desc)
				}
				bar.Set64(current)
			case <-heartbeat:
				if quiet {
					if walking {
						logrus.Infof("Scanning... found %d files (%s) so far", foundCount.Load(), util.FormatBytes(foundBytes.Load()))
					} else {
						logrus.Infof("Hashing... %d/%d files done", processedCount.Load(), foundCount.Load())
					}
				}
				if cfg.OnHeartbeat != nil {
					cfg.OnHeartbeat(processedBytes.Load())
				}
			case <-done:
				bar.Finish()
				return
			}
		}
	}()

	// Start collector
	go func() {
		batch := make([]*models.FileRecord, 0, consts.DBBatchSize)
		// count := 0 // use processedCount
		flush := func() {
			if len(batch) == 0 {
				return
			}
			if err := dbStore.BatchAddFiles(batch); err != nil {
				logrus.Errorf("Error writing batch: %v", err)
				writeErrors.Add(1)
				errSamplesMu.Lock()
				if len(errSamples) < maxErrSamples {
					errSamples = append(errSamples, fmt.Sprintf("write batch of %d records: %v", len(batch), err))
				}
				errSamplesMu.Unlock()
			}
			var sum int64
			for _, f := range batch {
				sum += f.SizeBytes
			}
			processedBytes.Add(sum)
			processedCount.Add(int64(len(batch)))
			batch = batch[:0]
		}

		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case res, ok := <-results:
				if !ok {
					// Channel closed, flush remaining and exit.
					// Close rather than send: both the UI goroutine and the caller
					// below wait on this, and a single send would wake only one of
					// them, leaving the other blocked forever.
					flush()
					close(done)
					return
				}
				if res.Error != nil {
					recordErr(res.Error)
					continue
				}
				if res.FromResume {
					processedBytes.Add(res.File.SizeBytes)
					processedCount.Add(1)
					continue
				}

				batch = append(batch, res.File)
				if len(batch) >= consts.DBBatchSize {
					flush()
				}
			case <-ticker.C:
				flush()
			}
		}
	}()

	start := time.Now()
	scanner.RunScan(scanConfig, results)

	logrus.Infof("Scan duration: %v", time.Since(start))

	// Permission Warning Check
	if estimatedTotal > 0 {
		finalBytes := foundBytes.Load()
		// If we scanned less than 50% of the estimate, warn the user.
		// This is a heuristic to catch "Permission Denied" on top-level folders without spamming.
		if float64(finalBytes) < float64(estimatedTotal)*0.5 {
			logrus.Warnf("Scanned size (%s) is significantly lower than estimated FS usage (%s).", util.FormatBytes(finalBytes), util.FormatBytes(estimatedTotal))
			logrus.Warn("You may be missing files due to permissions. Try running as root.")
		}
	}

	<-done

	totalErrors := scanErrors.Load() + writeErrors.Load()
	if totalErrors > 0 {
		errSamplesMu.Lock()
		samples := append([]string(nil), errSamples...)
		errSamplesMu.Unlock()

		logrus.Errorf("%d file(s) could not be read or recorded. This snapshot is INCOMPLETE.", totalErrors)
		for _, e := range samples {
			logrus.Errorf("  %s", e)
		}
		if totalErrors > int64(len(samples)) {
			logrus.Errorf("  ... and %d more", totalErrors-int64(len(samples)))
		}
		logrus.Error("Marking snapshot as failed. Files it did not record would otherwise")
		logrus.Error("look like deletions when this snapshot is diffed. Fix the cause")
		logrus.Error("(often permissions - try running as root) and scan again.")

		if err := dbStore.FailSnapshot(snapshotID, time.Time{}, totalErrors); err != nil {
			fmt.Printf("Error recording snapshot failure: %v\n", err)
		}
		logrus.Infof("Finished with errors. Total duration: %v", time.Since(start))
		return fmt.Errorf("scan incomplete: %d file(s) could not be read or recorded", totalErrors)
	}

	if err := dbStore.CompleteSnapshot(snapshotID, time.Time{}); err != nil {
		fmt.Printf("Error completing snapshot: %v\n", err)
	}

	logrus.Infof("Finished. Total duration: %v", time.Since(start))
	return nil
}
