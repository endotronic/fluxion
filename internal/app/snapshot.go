package app

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"fluxion/internal/consts"
	"fluxion/internal/models"
	"fluxion/internal/scanner"
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"
	"fluxion/internal/util"

	"github.com/schollz/progressbar/v3"
)

type SnapshotConfig struct {
	TargetDir    string
	DBPath       string
	Name         string
	Threads      int
	ForceNew     bool
	ForceResume  bool
	Hostname     string
	CrossMounts  bool
	FailOnMount  bool
	ComputeMD5   bool
}

func RunSnapshot(cfg SnapshotConfig) error {
	targetDir, err := filepath.Abs(cfg.TargetDir)
	if err != nil {
		return fmt.Errorf("error getting abs path: %w", err)
	}

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

	// Check last snapshot
	lastSnap, err := dbStore.GetLastSnapshot(targetDir)
	if err != nil {
		return fmt.Errorf("error checking snapshots: %w", err)
	}

	var snapshotID int64
	var resumeMap map[string]models.FileRecord
	mode := "new"

	if lastSnap != nil {
		if lastSnap.Status == models.StatusInProgress {
			// Interrupted
			doResume := false
			if cfg.ForceResume {
				doResume = true
			} else if cfg.ForceNew {
				doResume = false
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

			if doResume {
				mode = "resume"
				snapshotID = lastSnap.ID
				fmt.Println("Resuming snapshot...")
				// Get total files for progress bar
				totalFiles, err := dbStore.GetFileCount(snapshotID)
				if err != nil {
					fmt.Printf("Warning: failed to get file count: %v\n", err)
					totalFiles = -1
				}

				bar := progressbar.Default(totalFiles, "Loading existing")
				
				if totalFiles > 0 {
					fmt.Printf("Loading %d existing files...\n", totalFiles)
				} else {
					fmt.Println("Loading existing map...")
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
				fmt.Printf("Already processed %d files. Skipping them.\n", len(resumeMap))
			} else {
				// Start new (abandon old)
				// We don't delete the old one, just start new.
			}
		} else {
			// Last snapshot completed. Start a fresh scan.
			if !cfg.ForceNew {
				fmt.Printf("found previous completed snapshot from %v. Starting new scan.\n", lastSnap.FinishedAt)
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
		var err error
		// We need to access the helper function. It is currently in main.go.
		// We should move getUniqueSnapshotName to here or a helper file in app.
		// For now, I'll inline/copy logical equivalent or create a helper in app package.
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

	if mode == "new" {
		snap, err := dbStore.CreateSnapshot(targetDir, finalName, hostname)
		if err != nil {
			return fmt.Errorf("error creating snapshot: %w", err)
		}
		snapshotID = snap.ID
		fmt.Printf("Created new snapshot ID %d (Name: %s) [Host: %s]\n", snapshotID, finalName, hostname)
	}

	// Run Scan
	results := make(chan scanner.ScanResult, cfg.Threads)
	
	// Progress Reporting
	var foundCount atomic.Int64
	var foundBytes atomic.Int64
	var processedBytes atomic.Int64
	var processedCount atomic.Int64
	
	walkDone := make(chan bool)
	done := make(chan bool)
	
	scanConfig := scanner.ScannerConfig{
		RootPath:   targetDir,
		SnapshotID: snapshotID,
		NumWorkers: cfg.Threads,
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

	// Prepare Bar
	// We want 2 modes: Indeterminate (while walking), then Determinate (when walk done)
	bar := progressbar.NewOptions64(
		-1,
		progressbar.OptionSetDescription("Scanning..."),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(15),
		progressbar.OptionThrottle(65*time.Millisecond),
		progressbar.OptionShowCount(),
		progressbar.OptionOnCompletion(func() {
			fmt.Fprint(os.Stderr, "\n")
		}),
		progressbar.OptionSpinnerType(14),
		progressbar.OptionFullWidth(),
	)

	// UI Loop
	totalBuffer := float64(scanConfig.NumWorkers*consts.ScannerChannelBufferMultiplier)
	uiDone := make(chan bool)
	go func() {
		defer close(uiDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		
		walking := true
		for {
			select {
			case <-walkDone:
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
		for res := range results {
			if res.Error != nil {
				// progressbar handles stderr
				continue
			}
			batch = append(batch, res.File)
			if len(batch) >= consts.DBBatchSize {
				if err := dbStore.BatchAddFiles(batch); err != nil {
					fmt.Printf("Error writing batch: %v\n", err)
				}
				
				var sum int64
				for _, f := range batch {
					sum += f.SizeBytes
				}
				processedBytes.Add(sum)
				processedCount.Add(int64(len(batch)))
				batch = batch[:0]
			}
		}
		// final batch
		if len(batch) > 0 {
			if err := dbStore.BatchAddFiles(batch); err != nil {
				fmt.Printf("Error writing batch: %v\n", err)
			}
			var sum int64
			for _, f := range batch {
				sum += f.SizeBytes
			}
			processedBytes.Add(sum)
			processedCount.Add(int64(len(batch)))
		}
		done <- true
	}()

	start := time.Now()
	scanner.RunScan(scanConfig, results)

	<-done
	
	if err := dbStore.CompleteSnapshot(snapshotID, time.Time{}); err != nil {
		fmt.Printf("Error completing snapshot: %v\n", err)
	}
	
	fmt.Printf("Duration: %v\n", time.Since(start))
	
	return nil
}

