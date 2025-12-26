package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/schollz/progressbar/v3"

	"file-hasher/internal/consts"
	"file-hasher/internal/models"
	"file-hasher/internal/scanner"
	"file-hasher/internal/store"
	"file-hasher/internal/store/sqlite"
)

func main() {
	dirPtr := flag.String("dir", "", "Directory to scan (required)")
	dbPtr := flag.String("db", "", "Path to sqlite DB (optional, defaults to <dirname>.db)")
	threadsPtr := flag.Int("threads", runtime.NumCPU(), "Number of threads")
	forceNewPtr := flag.Bool("new", false, "Force new scan (ignore previous)")
	forceResumePtr := flag.Bool("resume", false, "Force resume (if possible)")
	
	crossMountsPtr := flag.Bool("cross-mounts", true, "Traverse mount points")
	failOnMountPtr := flag.Bool("fail-on-mount", false, "Fail if mount point encountered")
	
	flag.Parse()

	if *dirPtr == "" {
		fmt.Println("Error: --dir is required")
		flag.Usage()
		os.Exit(1)
	}

	targetDir, err := filepath.Abs(*dirPtr)
	if err != nil {
		fmt.Printf("Error getting abs path: %v\n", err)
		os.Exit(1)
	}

	dbPath := *dbPtr
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
		fmt.Printf("Error opening DB: %v\n", err)
		os.Exit(1)
	}
	defer dbStore.Close()

	// Check last snapshot
	lastSnap, err := dbStore.GetLastSnapshot(targetDir)
	if err != nil {
		fmt.Printf("Error checking snapshots: %v\n", err)
		os.Exit(1)
	}

	var snapshotID int64
	var resumeMap map[string]models.FileRecord
	mode := "new"

	if lastSnap != nil {
		if lastSnap.Status == models.StatusInProgress {
			// Interrupted
			doResume := false
			if *forceResumePtr {
				doResume = true
			} else if *forceNewPtr {
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
					fmt.Printf("Error loading resume data: %v\n", err)
					os.Exit(1)
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
			if !*forceNewPtr {
				fmt.Printf("found previous completed snapshot from %v. Starting new scan.\n", lastSnap.FinishedAt)
			}
		}
	}

	if mode == "new" {
		snap, err := dbStore.CreateSnapshot(targetDir)
		if err != nil {
			fmt.Printf("Error creating snapshot: %v\n", err)
			os.Exit(1)
		}
		snapshotID = snap.ID
		fmt.Printf("Created new snapshot ID %d\n", snapshotID)
	}

	// Run Scan
	results := make(chan scanner.ScanResult, *threadsPtr)
	
	// Progress Reporting
	var foundCount atomic.Int64
	var foundBytes atomic.Int64
	var processedCount atomic.Int64
	
	walkDone := make(chan bool)
	done := make(chan bool)
	
	scanConfig := scanner.ScannerConfig{
		RootPath:   targetDir,
		SnapshotID: snapshotID,
		NumWorkers: *threadsPtr,
		ResumeMap:   resumeMap,
		CrossMounts: *crossMountsPtr,
		FailOnMount: *failOnMountPtr,
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
				total := foundCount.Load()
				bar.ChangeMax64(total)
			case <-ticker.C:
				current := processedCount.Load()
				// Update description with found count if walking
				if walking {
					found := foundCount.Load()
					bar.Describe(fmt.Sprintf("Scanning (Found %d)...", found))
					bar.ChangeMax64(int64(found) + 100) // Keep it moving / indeterminate illusion?
					// actually progressbar v3 indeterminate is Max=-1.
					// If we keep changing Max, it might look weird.
					// Let's just update description.
				} else {
					bar.Describe("Hashing...")
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
				// Log to separate line so bar isn't messed up?
				// progressbar handles stderr.
				// fmt.Fprintf(os.Stderr, "Error: %v\n", res.Error)
				continue
			}
			batch = append(batch, res.File)
			if len(batch) >= consts.DBBatchSize {
				if err := dbStore.BatchAddFiles(batch); err != nil {
					fmt.Printf("Error writing batch: %v\n", err)
				}
				processedCount.Add(int64(len(batch)))
				batch = batch[:0]
			}
		}
		// final batch
		if len(batch) > 0 {
			if err := dbStore.BatchAddFiles(batch); err != nil {
				fmt.Printf("Error writing batch: %v\n", err)
			}
			processedCount.Add(int64(len(batch)))
		}
		done <- true
	}()

	start := time.Now()
	scanner.RunScan(scanConfig, results)

	<-done
	
	if err := dbStore.CompleteSnapshot(snapshotID); err != nil {
		fmt.Printf("Error completing snapshot: %v\n", err)
	}
	
	fmt.Printf("Duration: %v\n", time.Since(start))
}
