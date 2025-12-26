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
	"file-hasher/internal/diff"
	"file-hasher/internal/models"
	"file-hasher/internal/scanner"
	"file-hasher/internal/store"
	"file-hasher/internal/store/sqlite"
)

func main() {
	// Parse subcommand
	if len(os.Args) < 2 {
		fmt.Println("Usage: file-hasher <subcommand> [flags]")
		fmt.Println("Subcommands: snapshot, list, diff")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "snapshot":
		runSnapshot(os.Args[2:])
	case "list":
		runList(os.Args[2:])
	case "diff":
		runDiff(os.Args[2:])
	default:
		fmt.Printf("Unknown subcommand: %s\n", os.Args[1])
		os.Exit(1)
	}
}

func runList(args []string) {
	cmd := flag.NewFlagSet("list", flag.ExitOnError)
	dbPtr := cmd.String("db", "", "Path to sqlite DB (required)")
	cmd.Parse(args)

	if *dbPtr == "" {
		fmt.Println("Error: --db is required for list")
		cmd.Usage()
		os.Exit(1)
	}

	dbStore, err := sqlite.NewSqliteStore(*dbPtr)
	if err != nil {
		fmt.Printf("Error opening DB: %v\n", err)
		os.Exit(1)
	}
	defer dbStore.Close()

	snaps, err := dbStore.ListSnapshots()
	if err != nil {
		fmt.Printf("Error listing snapshots: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("ID\tStatus\tStarted At\t\t\tFinished At\t\t\tRoot Path")
	fmt.Println("--\t------\t----------\t\t\t-----------\t\t\t---------")
	for _, s := range snaps {
		finished := "N/A"
		if s.FinishedAt != nil {
			finished = s.FinishedAt.Format(time.RFC3339)
		}
		fmt.Printf("%d\t%s\t%s\t%s\t%s\n", s.ID, s.Status, s.StartedAt.Format(time.RFC3339), finished, s.RootPath)
	}
}

func runSnapshot(args []string) {
	cmd := flag.NewFlagSet("snapshot", flag.ExitOnError)
	dirPtr := cmd.String("dir", "", "Directory to scan (required)")
	dbPtr := cmd.String("db", "", "Path to sqlite DB (optional, defaults to <dirname>.db)")
	threadsPtr := cmd.Int("threads", runtime.NumCPU(), "Number of threads")
	forceNewPtr := cmd.Bool("new", false, "Force new scan (ignore previous)")
	forceResumePtr := cmd.Bool("resume", false, "Force resume (if possible)")
	
	crossMountsPtr := cmd.Bool("cross-mounts", true, "Traverse mount points")
	failOnMountPtr := cmd.Bool("fail-on-mount", false, "Fail if mount point encountered")
	
	cmd.Parse(args)

	if *dirPtr == "" {
		fmt.Println("Error: --dir is required")
		cmd.Usage()
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
				// progressbar handles stderr
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

func runDiff(args []string) {
	cmd := flag.NewFlagSet("diff", flag.ExitOnError)
	dbPtr := cmd.String("db", "", "Path to sqlite DB (required)")
	cmd.Parse(args)

	if *dbPtr == "" {
		fmt.Println("Error: --db is required for diff")
		cmd.Usage()
		os.Exit(1)
	}
	
	tail := cmd.Args()
	if len(tail) != 2 {
		fmt.Println("Usage: file-hasher diff --db <db> <old_snapshot_id> <new_snapshot_id>")
		os.Exit(1)
	}

	oldIDStr := tail[0]
	newIDStr := tail[1]
	
	// Parse IDs... (assuming strconv needed, wait I forgot import)
	// I'll add strconv import in a separate replace or use fmt.Sscanf
	var oldID, newID int64
	fmt.Sscanf(oldIDStr, "%d", &oldID)
	fmt.Sscanf(newIDStr, "%d", &newID)

	// Open DB
	dbStore, err := sqlite.NewSqliteStore(*dbPtr)
	if err != nil {
		fmt.Printf("Error opening DB: %v\n", err)
		os.Exit(1)
	}
	defer dbStore.Close()

	// 1. Get Metadata
	snaps, err := dbStore.ListSnapshots()
	if err != nil {
		fmt.Printf("Error listing snapshots: %v\n", err)
		os.Exit(1)
	}
	// Logic to find specific snapshots? ListSnapshots returns all. 
	// We might need GetSnapshot(id). But Store doesn't have it.
	// We can iterate the list.
	
	var snapA, snapB *models.Snapshot
	for _, s := range snaps {
		if s.ID == oldID {
			snapA = s
		}
		if s.ID == newID {
			snapB = s
		}
	}
	
	if snapA == nil || snapB == nil {
		fmt.Println("Error: could not find one or both snapshots.")
		os.Exit(1)
	}
	
	// Validate Root Path
	if snapA.RootPath != snapB.RootPath {
		fmt.Printf("Error: Root paths do not match.\nSnapshot %d: %s\nSnapshot %d: %s\n", snapA.ID, snapA.RootPath, snapB.ID, snapB.RootPath)
		os.Exit(1)
	}

	// 2. Load Maps with Progress
	fmt.Printf("Loading Snapshot %d...\n", oldID)
	countA, _ := dbStore.GetFileCount(oldID)
	barA := progressbar.Default(countA)
	filesA, err := dbStore.GetFilesForSnapshot(oldID, func(c int) { barA.Set(c) })
	if err != nil { panic(err) }
	
	fmt.Printf("\nLoading Snapshot %d...\n", newID)
	countB, _ := dbStore.GetFileCount(newID)
	barB := progressbar.Default(countB)
	filesB, err := dbStore.GetFilesForSnapshot(newID, func(c int) { barB.Set(c) })
	if err != nil { panic(err) }
	fmt.Println()

	// 3. Compare with Progress
	// Total ops = len(A) + len(B)
	fmt.Println("Computing Diff...")
	barDiff := progressbar.Default(int64(len(filesA) + len(filesB)))
	
	results, err := diff.CompareSnapshots(filesA, filesB, func(curr, total int) {
		barDiff.Set(curr)
	})
	if err != nil {
		fmt.Printf("Error during diff: %v\n", err)
		os.Exit(1)
	}
	fmt.Println()

	// 4. Print Results
	if len(results) == 0 {
		fmt.Println("No differences found.")
		return
	}

	for _, res := range results {
		symbol := "?"
		path := res.Path
		switch res.Status {
		case diff.StatusAdded:
			symbol = "[+]"
		case diff.StatusRemoved:
			symbol = "[-]"
		case diff.StatusModified:
			symbol = "[M]"
		case diff.StatusMove:
			symbol = "[>]"
			path = fmt.Sprintf("%s -> %s", res.SourcePath, res.Path)
		case diff.StatusCopy:
			symbol = "[C]"
			path = fmt.Sprintf("%s -> %s", res.SourcePath, res.Path)
		}
		fmt.Printf("%s %s\n", symbol, path)
	}
}
