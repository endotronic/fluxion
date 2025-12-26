package scanner

import (
	"crypto/sha1"
	"encoding/hex"
	"file-hasher/internal/consts"
	"file-hasher/internal/models"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// Scanner config
type ScannerConfig struct {
	RootPath   string
	SnapshotID int64
	NumWorkers int
	// If ResumeMap is provided, we skip paths present in it (assuming valid resume)
	// Or we can verify them. For now, strict resume means "skip if present".
	ResumeMap map[string]models.FileRecord

	CrossMounts bool
	FailOnMount bool

	// Callbacks
	OnFileFound    func(path string, size int64)
	OnWalkComplete func()
}

type ScanResult struct {
	File  *models.FileRecord
	Error error
}

func RunScan(cfg ScannerConfig, results chan<- ScanResult) {
	defer close(results)

	paths := make(chan string, cfg.NumWorkers*consts.ScannerChannelBufferMultiplier)
	var wg sync.WaitGroup

	// Start workers
	for i := 0; i < cfg.NumWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range paths {
				processFile(path, cfg, results)
			}
		}()
	}

	// Get root device ID
	rootDev, err := getDevice(cfg.RootPath)
	if err != nil {
		results <- ScanResult{Error: fmt.Errorf("failed to get root device: %w", err)}
		return
	}

	// Start walker
	err = filepath.WalkDir(cfg.RootPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Report error but continue walking other files if possible?
			// For WalkDir, returning error stops walk.
			// Let's log it to results and skip.
			results <- ScanResult{Error: err}
			return nil // Don't stop walk for individual file permission errors
			return nil // Don't stop walk for individual file permission errors
		}

		// Check for filesystem boundary
		if d.IsDir() {
			dev, err := getDevice(path)
			if err != nil {
				// Warn and continue?
				fmt.Printf("Warning: failed to get device for %s: %v\n", path, err)
			} else if dev != rootDev {
				// Different device
				if !cfg.CrossMounts {
					if cfg.FailOnMount {
						return fmt.Errorf("filesystem boundary detected at %s", path)
					}
					fmt.Printf("Skipping filesystem boundary: %s\n", path)
					return filepath.SkipDir
				}
				// Default: warn but continue
				fmt.Printf("Warning: crossing filesystem boundary at %s\n", path)
			}
		}

		if !d.IsDir() {
			// Check resume map
			if cfg.ResumeMap != nil {
				if _, exists := cfg.ResumeMap[path]; exists {
					// Already processed in this snapshot
					return nil
				}
			}
			paths <- path
		}
		return nil
	})

	if err != nil {
		results <- ScanResult{Error: err}
	}

	if cfg.OnWalkComplete != nil {
		cfg.OnWalkComplete()
	}

	close(paths)
	wg.Wait()
}

func processFile(path string, cfg ScannerConfig, results chan<- ScanResult) {
	info, err := os.Stat(path)
	if err != nil {
		results <- ScanResult{Error: err}
		return
	}

	// Hash the file
	hash, err := hashFile(path)
	if err != nil {
		results <- ScanResult{Error: err}
		return
	}

	record := &models.FileRecord{
		SnapshotID: cfg.SnapshotID,
		Path:       path,
		Filename:   filepath.Base(path),
		SizeBytes:  info.Size(),
		ModTime:    info.ModTime(),
		SHA1:       hash,
	}

	if cfg.OnFileFound != nil {
		cfg.OnFileFound(path, info.Size())
	}

	results <- ScanResult{File: record}
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Use a small buffer if needed, but io.Copy handles it.
	// SHA1 as requested
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func getDevice(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unsupported platform for device check")
	}
	// On Mac (Darwin), Dev is int32, explicitly cast to uint64 for safety/consistency
	return uint64(stat.Dev), nil
}
