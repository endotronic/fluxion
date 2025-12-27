package app

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"fluxion/internal/consts"
	"fluxion/internal/models"
	"fluxion/internal/store/sqlite"

	"github.com/schollz/progressbar/v3"
)

type ImportLegacyConfig struct {
	HashesPath string
	SizesPath  string
	DBPath     string
	Name       string
	RootPath   string
}

func RunImportLegacy(cfg ImportLegacyConfig) error {
	if cfg.DBPath == "" {
		return fmt.Errorf("DB path is required")
	}
	if cfg.HashesPath == "" {
		return fmt.Errorf("hashes file path is required")
	}

	dbPath := cfg.DBPath
	rootPath := cfg.RootPath
	snapName := cfg.Name

	if snapName == "" {
		snapName = filepath.Base(cfg.HashesPath)
		// Clean up common suffixes
		snapName = strings.TrimSuffix(snapName, "_files_hashes.txt")
		snapName = strings.TrimSuffix(snapName, "_hashes.txt")
		snapName = strings.TrimSuffix(snapName, ".txt")
	}
	
	// Infer Sizes File if not provided
	sizesPath := cfg.SizesPath
	if sizesPath == "" {
		// Infer
		// Try replacing "hashes" with "sizes"
		// Logic: _hashes.txt -> _sizes.txt
		if strings.Contains(cfg.HashesPath, "hashes") {
			inferred := strings.Replace(cfg.HashesPath, "hashes", "sizes", 1)
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
		return fmt.Errorf("error opening DB: %w", err)
	}
	defer dbStore.Close()

	// 1. Load Sizes into memory (if provided)
	sizeMap := make(map[string]int64)
	if sizesPath != "" {
		fmt.Printf("Loading sizes from %s...\n", sizesPath)
		sf, err := os.Open(sizesPath)
		if err != nil {
			return fmt.Errorf("error opening sizes file: %w", err)
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
	hf, err := os.Open(cfg.HashesPath)
	if err != nil {
		return fmt.Errorf("error opening hashes file: %w", err)
	}
	defer hf.Close()

	// Autodetect Root if not provided (or default "/")
	if rootPath == "/" {
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
			return fmt.Errorf("error rewinding file: %w", err)
		}
	}

	// Resolve unique name (Import always increments on collision)
	finalName, err := getUniqueSnapshotName(dbStore, snapName, false)
	if err != nil {
		return fmt.Errorf("error resolving name: %w", err)
	}

	// Create Snapshot
	h, _ := os.Hostname()
	snap, err := dbStore.CreateSnapshot(rootPath, finalName, h)
	if err != nil {
		return fmt.Errorf("error creating snapshot: %w", err)
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
	return nil
}

type ImportDBConfig struct {
	SourceDBPath string
	DestDBPath   string
}

func RunImportDB(cfg ImportDBConfig) error {
	if cfg.SourceDBPath == "" {
		return fmt.Errorf("source DB path is required")
	}
	if cfg.DestDBPath == "" {
		return fmt.Errorf("destination DB path is required")
	}

	fmt.Printf("Importing from %s to %s\n", cfg.SourceDBPath, cfg.DestDBPath)

	// Open Source
	sourceStore, err := sqlite.NewSqliteStore(cfg.SourceDBPath)
	if err != nil {
		return fmt.Errorf("error opening source DB: %w", err)
	}
	defer sourceStore.Close()

	// Open Dest
	destStore, err := sqlite.NewSqliteStore(cfg.DestDBPath)
	if err != nil {
		return fmt.Errorf("error opening dest DB: %w", err)
	}
	defer destStore.Close()

	// List Source Snapshots
	snaps, err := sourceStore.ListSnapshots()
	if err != nil {
		return fmt.Errorf("error listing source snapshots: %w", err)
	}

	if len(snaps) == 0 {
		fmt.Println("No snapshots found in source DB.")
		return nil
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
		h, _ := os.Hostname()
		newSnap, err := destStore.CreateSnapshot(s.RootPath, finalName, h)
		if err != nil {
			fmt.Printf("Error creating snapshot in dest: %v\n", err)
			continue
		}
		
		// Retrieve files from Source
		count, _ := sourceStore.GetFileCount(s.ID)
		bar := progressbar.Default(count, fmt.Sprintf("Copying %s", s.Name))
		
		files, err := sourceStore.GetFilesForSnapshot(s.ID, func(c int) {
			// GetFiles callback provides loaded count, not incremental
			// progressbar Set(c) handles absolute
			bar.Set(c)
		})
		if err != nil {
			fmt.Printf("Error reading files from source: %v\n", err)
			continue
		}
		bar.Finish()
		fmt.Println()

		// Insert into Dest
		batch := make([]*models.FileRecord, 0, consts.DBBatchSize)
		
		// We iterate map. Order doesn't matter.
		for _, f := range files {
			// Create new record bound to new snapshot
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
					fmt.Printf("Error writing batch: %v\n", err)
				}
				batch = batch[:0]
			}
		}
		
		if len(batch) > 0 {
			if err := destStore.BatchAddFiles(batch); err != nil {
				fmt.Printf("Error writing batch: %v\n", err)
			}
		}
		
		if err := destStore.CompleteSnapshot(newSnap.ID); err != nil {
			fmt.Printf("Error completing snapshot: %v\n", err)
		}
		fmt.Printf("Successfully imported '%s' as '%s'.\n", s.Name, finalName)
	}
	
	return nil
}
