package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

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
				// Load existing files to skip
				resumeMap, err = dbStore.GetFilesForSnapshot(snapshotID)
				if err != nil {
					fmt.Printf("Error loading resume data: %v\n", err)
					os.Exit(1)
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
	
	// Start collector
	done := make(chan bool)
	go func() {
		batch := make([]*models.FileRecord, 0, consts.DBBatchSize)
		count := 0
		for res := range results {
			if res.Error != nil {
				fmt.Printf("Error: %v\n", res.Error)
				continue
			}
			batch = append(batch, res.File)
			if len(batch) >= consts.DBBatchSize {
				if err := dbStore.BatchAddFiles(batch); err != nil {
					fmt.Printf("Error writing batch: %v\n", err)
				}
				count += len(batch)
				batch = batch[:0]
				fmt.Printf("\rProcessed %d files...", count)
			}
		}
		// final batch
		if len(batch) > 0 {
			if err := dbStore.BatchAddFiles(batch); err != nil {
				fmt.Printf("Error writing batch: %v\n", err)
			}
			count += len(batch)
		}
		fmt.Printf("\nFinished. Total files: %d\n", count)
		done <- true
	}()

	start := time.Now()
	scanner.RunScan(scanner.ScannerConfig{
		RootPath:   targetDir,
		SnapshotID: snapshotID,
		NumWorkers: *threadsPtr,
		ResumeMap:   resumeMap,
		CrossMounts: *crossMountsPtr,
		FailOnMount: *failOnMountPtr,
	}, results)

	<-done
	
	if err := dbStore.CompleteSnapshot(snapshotID); err != nil {
		fmt.Printf("Error completing snapshot: %v\n", err)
	}
	
	fmt.Printf("Duration: %v\n", time.Since(start))
}
