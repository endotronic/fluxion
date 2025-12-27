package main

import (
	"bufio"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/schollz/progressbar/v3"

	"fluxion/internal/consts"
	"fluxion/internal/diff"
	"fluxion/internal/models"
	"fluxion/internal/scanner"
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"
	"fluxion/internal/table"
)

func main() {
	// Parse subcommand
	if len(os.Args) < 2 {
		fmt.Println("Usage: fluxion <subcommand> [flags]")
		fmt.Println("Subcommands:")
		fmt.Println("  snapshot (s)    Scan a directory")
		fmt.Println("  list (l)        List snapshots")
		fmt.Println("  diff (d)        Compare snapshots")
		fmt.Println("  import (i)      Import from another DB")
		fmt.Println("  import-legacy   Import legacy format")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "snapshot", "s":
		runSnapshot(os.Args[2:])
	case "list", "l":
		runList(os.Args[2:])
	case "diff", "d":
		runDiff(os.Args[2:])
	case "import", "i":
		runImportDB(os.Args[2:])
	case "import-legacy":
		runImportLegacy(os.Args[2:])
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

	if len(snaps) == 0 {
		fmt.Println("No snapshots found.")
		return
	}

	tbl := table.New([]string{"ID", "Name", "Status", "Started At", "Finished At", "Root Path"})

	for _, s := range snaps {
		finished := "N/A"
		if s.FinishedAt != nil {
			finished = s.FinishedAt.Format(time.RFC3339)
		}
		
		tbl.AddRow([]string{
			fmt.Sprintf("%d", s.ID),
			s.Name,
			string(s.Status),
			s.StartedAt.Format(time.RFC3339),
			finished,
			s.RootPath,
		})
	}
	
	tbl.Print(os.Stdout)
}

func runSnapshot(args []string) {
	cmd := flag.NewFlagSet("snapshot", flag.ExitOnError)
	dirPtr := cmd.String("dir", "", "Directory to scan (optional if provided as argument)")
	dbPtr := cmd.String("db", "", "Path to sqlite DB (optional, defaults to <dirname>.db)")
	namePtr := cmd.String("name", "", "Name for the snapshot (optional, defaults to <dirname>_<date>)")
	threadsPtr := cmd.Int("threads", runtime.NumCPU(), "Number of threads")
	forceNewPtr := cmd.Bool("new", false, "Force new scan (ignore previous)")
	forceResumePtr := cmd.Bool("resume", false, "Force resume (if possible)")
	
	crossMountsPtr := cmd.Bool("cross-mounts", true, "Traverse mount points")
	failOnMountPtr := cmd.Bool("fail-on-mount", false, "Fail if mount point encountered")
	md5Ptr := cmd.Bool("md5", false, "Compute MD5 checksums")
	
	cmd.Parse(args)

	// Check for positional argument if flag is empty
	targetArg := *dirPtr
	if targetArg == "" {
		if cmd.NArg() > 0 {
			targetArg = cmd.Arg(0)
		}
	}

	if targetArg == "" {
		fmt.Println("Error: Directory required (via argument or --dir)")
		cmd.Usage()
		os.Exit(1)
	}

	targetDir, err := filepath.Abs(targetArg)
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

	// Determine Base Name and Uniqueness Strategy
	baseName := *namePtr
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
		finalName, err = getUniqueSnapshotName(dbStore, baseName, explicitName)
		if err != nil {
			fmt.Printf("Error resolving snapshot name: %v\n", err)
			os.Exit(1)
		}
	}

	// Open DB (Moved down? No, need DB to check name)
	// Already opened above.

	if mode == "new" {
		snap, err := dbStore.CreateSnapshot(targetDir, finalName)
		if err != nil {
			fmt.Printf("Error creating snapshot: %v\n", err)
			os.Exit(1)
		}
		snapshotID = snap.ID
		fmt.Printf("Created new snapshot ID %d (Name: %s)\n", snapshotID, finalName)
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
		ComputeMD5:  *md5Ptr,
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
				// Update description with found count if walking
				if walking {
					found := foundCount.Load()
					desc := fmt.Sprintf("Scanning (Found %d)...", found)
					
					// Check saturation (if channel is >= 90% full)
					// Only relevant if we have a buffer
					capRes := cap(results)
					if capRes > 0 && float64(len(results)) >= float64(capRes)*0.9 {
						desc = fmt.Sprintf("Scanning (Found %d) [Pipeline Saturated]...", found)
					}
					
					bar.Describe(desc)
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

	oldQuery := tail[0]
	newQuery := tail[1]
	
	// Open DB
	dbStore, err := sqlite.NewSqliteStore(*dbPtr)
	if err != nil {
		fmt.Printf("Error opening DB: %v\n", err)
		os.Exit(1)
	}
	defer dbStore.Close()

	// 1. Find Snapshots
	snapA, err := dbStore.FindSnapshot(oldQuery)
	if err != nil {
		fmt.Printf("Error: could not find 'old' snapshot '%s': %v\n", oldQuery, err)
		os.Exit(1)
	}
	
	snapB, err := dbStore.FindSnapshot(newQuery)
	if err != nil {
		fmt.Printf("Error: could not find 'new' snapshot '%s': %v\n", newQuery, err)
		os.Exit(1)
	}
	
	oldID := snapA.ID
	newID := snapB.ID

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
	
	// Prepare relative maps
	relFilesA := make(map[string]models.FileRecord, len(filesA))
	for k, v := range filesA {
		rel, err := filepath.Rel(snapA.RootPath, k)
		if err == nil {
			relFilesA[rel] = v
		} else {
			relFilesA[k] = v // Fallback
		}
	}
	
	relFilesB := make(map[string]models.FileRecord, len(filesB))
	for k, v := range filesB {
		rel, err := filepath.Rel(snapB.RootPath, k)
		if err == nil {
			relFilesB[rel] = v
		} else {
			relFilesB[k] = v // Fallback
		}
	}

	barDiff := progressbar.Default(int64(len(filesA) + len(filesB)))

	results, err := diff.CompareSnapshots(relFilesA, relFilesB, snapA.RootPath, snapB.RootPath, func(curr, total int) {
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

func runImportLegacy(args []string) {
	cmd := flag.NewFlagSet("import-legacy", flag.ExitOnError)
	sizesPtr := cmd.String("sizes", "", "Path to sizes file (optional, defaults to inferred from hashes)")
	dbPtr := cmd.String("db", "", "Path to sqlite DB (required)")
	namePtr := cmd.String("name", "", "Name for the snapshot (optional, defaults to filename)")
	rootPtr := cmd.String("root", "/", "Root path for the snapshot (defaults to /)")
	
	cmd.Parse(args)

	// Validate Arguments
	if *dbPtr == "" {
		fmt.Println("Error: --db is required.")
		cmd.Usage()
		os.Exit(1)
	}
	
	// Hashes File is Positional
	hashesPath := ""
	if cmd.NArg() > 0 {
		hashesPath = cmd.Arg(0)
	}
	
	if hashesPath == "" {
		fmt.Println("Error: hashes file path is required as positional argument.")
		cmd.Usage()
		os.Exit(1)
	}

	dbPath := *dbPtr
	rootPath := *rootPtr
	snapName := *namePtr

	if snapName == "" {
		snapName = filepath.Base(hashesPath)
		// Clean up common suffixes
		snapName = strings.TrimSuffix(snapName, "_files_hashes.txt")
		snapName = strings.TrimSuffix(snapName, "_hashes.txt")
		snapName = strings.TrimSuffix(snapName, ".txt")
	}
	
	// Infer Sizes File if not provided
	sizesPath := *sizesPtr
	if sizesPath == "" {
		// Infer
		// Try replacing "hashes" with "sizes"
		// Logic: _hashes.txt -> _sizes.txt
		if strings.Contains(hashesPath, "hashes") {
			inferred := strings.Replace(hashesPath, "hashes", "sizes", 1)
			if _, err := os.Stat(inferred); err == nil {
				sizesPath = inferred
				fmt.Printf("Inferred sizes file: %s\n", sizesPath)
			}
		}
	}
	
	if sizesPath == "" {
		fmt.Println("Warning: Sizes file not provided and could not be inferred. Importing with 0 sizes.")
	}

	// Open DB
	dbStore, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		fmt.Printf("Error opening DB: %v\n", err)
		os.Exit(1)
	}
	defer dbStore.Close()

	// 1. Load Sizes into memory (if provided)
	sizeMap := make(map[string]int64)
	if sizesPath != "" {
		fmt.Printf("Loading sizes from %s...\n", sizesPath)
		sf, err := os.Open(sizesPath)
		if err != nil {
			fmt.Printf("Error opening sizes file: %v\n", err)
			os.Exit(1)
		}
		defer sf.Close()
		
		sScanner := bufio.NewScanner(sf)
		for sScanner.Scan() {
			line := sScanner.Text()
			// Format: SIZE  ENCODING  BASE64_PATH
			parts := strings.Split(line, "  ")
			if len(parts) >= 3 {
				sizeStr := strings.TrimSpace(parts[0])
				b64Path := strings.TrimSpace(parts[2])
				
				size, err := strconv.ParseInt(sizeStr, 10, 64)
				if err == nil {
					sizeMap[b64Path] = size
				}
			}
		}
		fmt.Printf("Loaded %d size records.\n", len(sizeMap))
	}

	// 2. Open Hashes File
	hf, err := os.Open(hashesPath)
	if err != nil {
		fmt.Printf("Error opening hashes file: %v\n", err)
		os.Exit(1)
	}
	defer hf.Close()

	// Autodetect Root if not provided (or default "/")
	if *rootPtr == "/" {
		fmt.Println("Autodetecting root path from hashes file...")
		scanner := bufio.NewScanner(hf)
		var commonPrefix string
		first := true
		
		count := 0
		for scanner.Scan() {
			line := scanner.Text()
			parts := strings.Split(line, "  ")
			if len(parts) < 3 { continue }
			
			b64Path := strings.TrimSpace(parts[2])
			decodedBytes, err := base64.StdEncoding.DecodeString(b64Path)
			if err != nil { continue }
			path := string(decodedBytes)
			
			if first {
				commonPrefix = filepath.Dir(path) // Start with dir of first file
				first = false
			} else {
				// Find common prefix
				// Simple approach: shrink commonPrefix until it fits
				for !strings.HasPrefix(path, commonPrefix) {
					// Move up one dir
					if commonPrefix == "" || commonPrefix == "/" || commonPrefix == "." {
						commonPrefix = "/"
						break
					}
					commonPrefix = filepath.Dir(commonPrefix)
				}
			}
			count++
			if count % 10000 == 0 {
			    // Optimization: if commonPrefix is already root, stop?
			    if commonPrefix == "/" { break }
			}
		}
		
		if commonPrefix == "" { commonPrefix = "/" }
		rootPath = commonPrefix
		fmt.Printf("Detected root: %s\n", rootPath)
		
		// Rewind file
		if _, err := hf.Seek(0, 0); err != nil {
			fmt.Printf("Error rewinding file: %v\n", err)
			os.Exit(1)
		}
	}

	// Resolve unique name (Import always increments on collision)
	finalName, err := getUniqueSnapshotName(dbStore, snapName, false)
	if err != nil {
		fmt.Printf("Error resolving name: %v\n", err)
		os.Exit(1)
	}

	// Create Snapshot
	snap, err := dbStore.CreateSnapshot(rootPath, finalName)
	if err != nil {
		fmt.Printf("Error creating snapshot: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Created snapshot ID %d (Name: %s)\n", snap.ID, finalName)

	scanner := bufio.NewScanner(hf)
	batch := make([]*models.FileRecord, 0, consts.DBBatchSize)
	var processedCount int64
	
	bar := progressbar.Default(-1, "Importing lines")
	
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, "  ") // double space separator
		if len(parts) < 3 {
			continue
		}
		
		hashVal := strings.TrimSpace(parts[0])
		b64Path := strings.TrimSpace(parts[2])
		
		decodedPathBytes, err := base64.StdEncoding.DecodeString(b64Path)
		if err != nil {
			continue
		}
		path := string(decodedPathBytes)
		
		// Lookup Size
		var size int64 = 0
		if val, ok := sizeMap[b64Path]; ok {
			size = val
		}

		record := &models.FileRecord{
			SnapshotID: snap.ID,
			Path:       path,
			Filename:   filepath.Base(path),
			SizeBytes:  size,
			ModTime:    time.Time{}, // Zero time
			SHA1:       "",          // Empty SHA1 for legacy import
			MD5:        hashVal,     // MD5 from legacy
		}
		
		batch = append(batch, record)
		if len(batch) >= consts.DBBatchSize {
			if err := dbStore.BatchAddFiles(batch); err != nil {
				fmt.Printf("Error writing batch: %v\n", err)
			}
			processedCount += int64(len(batch))
			bar.Add(len(batch))
			batch = batch[:0]
		}
	}

	if len(batch) > 0 {
		if err := dbStore.BatchAddFiles(batch); err != nil {
			fmt.Printf("Error writing batch: %v\n", err)
		}
		processedCount += int64(len(batch))
		bar.Add(len(batch))
	}
	
	bar.Finish()
	fmt.Println()

	if err := scanner.Err(); err != nil {
		fmt.Printf("Error reading file: %v\n", err)
	}
	
	if err := dbStore.CompleteSnapshot(snap.ID); err != nil {
		fmt.Printf("Error completing snapshot: %v\n", err)
	}
	
	fmt.Printf("Imported %d files.\n", processedCount)
}

func runImportDB(args []string) {
	cmd := flag.NewFlagSet("import", flag.ExitOnError)
	destDBPtr := cmd.String("db", "", "Path to destination sqlite DB (required)")
	sourceDBPtr := cmd.String("source", "", "Path to source sqlite DB (required)")
	
	cmd.Parse(args)

	if *destDBPtr == "" || *sourceDBPtr == "" {
		fmt.Println("Error: --db and --source are required")
		cmd.Usage()
		os.Exit(1)
	}

	// 1. Open Source DB
	sourceStore, err := sqlite.NewSqliteStore(*sourceDBPtr)
	if err != nil {
		fmt.Printf("Error opening Source DB: %v\n", err)
		os.Exit(1)
	}
	defer sourceStore.Close()

	// 2. Open Dest DB
	destStore, err := sqlite.NewSqliteStore(*destDBPtr)
	if err != nil {
		fmt.Printf("Error opening Dest DB: %v\n", err)
		os.Exit(1)
	}
	defer destStore.Close()

	// 3. List Snapshots from Source
	snaps, err := sourceStore.ListSnapshots()
	if err != nil {
		fmt.Printf("Error listing source snapshots: %v\n", err)
		os.Exit(1)
	}

	if len(snaps) == 0 {
		fmt.Println("No snapshots found in source DB.")
		return
	}

	fmt.Printf("Found %d snapshots in source. Importing...\n", len(snaps))

	for _, s := range snaps {
		fmt.Printf("Importing snapshot '%s' (ID: %d)...\n", s.Name, s.ID)

		// Resolve unique name
		finalName, err := getUniqueSnapshotName(destStore, s.Name, false)
		if err != nil {
			fmt.Printf("Error resolving name for '%s': %v\n", s.Name, err)
			continue
		}
		
		// Create new snapshot in Dest
		newSnap, err := destStore.CreateSnapshot(s.RootPath, finalName)
		if err != nil {
			fmt.Printf("Error creating snapshot in dest: %v\n", err)
			continue
		}
		
		// Copy metadata (StartedAt, FinishedAt, Status)
		// Note: CreateSnapshot sets StartedAt=Now, Status=InProgress.
		// Future improvement: Copy exact timestamps if store API allows.
		
		// Retrieve files from Source
		// We use progress bar for files
		count, _ := sourceStore.GetFileCount(s.ID)
		bar := progressbar.Default(count, fmt.Sprintf("Copying %s", s.Name))
		
		files, err := sourceStore.GetFilesForSnapshot(s.ID, func(c int) {
			bar.Set(c)
		})
		if err != nil {
			fmt.Printf("Error getting files from source: %v\n", err)
			continue
		}
		
		// Prepare batch for Dest
		batch := make([]*models.FileRecord, 0, consts.DBBatchSize)
		for _, f := range files {
			// Update SnapshotID to newSnap.ID
			// Create new record copy
			newRec := &models.FileRecord{
				SnapshotID: newSnap.ID,
				Path:       f.Path,
				Filename:   f.Filename,
				SizeBytes:  f.SizeBytes,
				ModTime:    f.ModTime,
				SHA1:       f.SHA1,
				MD5:        f.MD5,
			}
			batch = append(batch, newRec)
			
			if len(batch) >= consts.DBBatchSize {
				if err := destStore.BatchAddFiles(batch); err != nil {
					fmt.Printf("Error writing batch to dest: %v\n", err)
				}
				batch = batch[:0]
			}
		}
		if len(batch) > 0 {
			destStore.BatchAddFiles(batch)
		}
		
		// Complete Dest Snapshot
		destStore.CompleteSnapshot(newSnap.ID)
		
		bar.Finish()
		fmt.Println()
	}
	
	fmt.Println("Import complete.")
}

func getUniqueSnapshotName(s store.Store, baseName string, mustFail bool) (string, error) {
	// Check if exists
	existing, err := s.FindSnapshot(baseName)
	if err != nil && !strings.Contains(err.Error(), "not found") {
		return "", err
	}
	
	if existing == nil {
		return baseName, nil
	}
	
	// Exists
	if mustFail {
		return "", fmt.Errorf("snapshot with name '%s' already exists", baseName)
	}
	
	// Auto-increment
	// Try appending -2, -3, ...
	i := 2
	for {
		newName := fmt.Sprintf("%s-%d", baseName, i)
		existing, err := s.FindSnapshot(newName)
		if err != nil && !strings.Contains(err.Error(), "not found") {
			return "", err
		}
		if existing == nil {
			return newName, nil
		}
		i++
	}
}
