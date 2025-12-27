package scanner

import (
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"fluxion/internal/consts"
	"fluxion/internal/models"
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

	// New: MD5 Computation
	ComputeMD5 bool

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
			results <- ScanResult{Error: err}
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
			// Skip non-regular files (symlinks, devices, pipes, sockets)
			if !d.Type().IsRegular() {
				return nil
			}

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

	close(paths)
	wg.Wait()

	if cfg.OnWalkComplete != nil {
		cfg.OnWalkComplete()
	}
}

func processFile(path string, cfg ScannerConfig, results chan<- ScanResult) {
	info, err := os.Stat(path)
	if err != nil {
		results <- ScanResult{Error: err}
		return
	}

	// Hash the file
	sha1Hash, md5Hash, err := hashFile(path, cfg.ComputeMD5)
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
		SHA1:       sha1Hash,
		MD5:        md5Hash,
	}

	if cfg.OnFileFound != nil {
		cfg.OnFileFound(path, info.Size())
	}

	results <- ScanResult{File: record}
}

func hashFile(path string, computeMD5 bool) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	// SHA1 as requested
	hSha1 := sha1.New()
	var writers io.Writer = hSha1
	
	var md5Res interface{ Sum([]byte) []byte }
	
	if computeMD5 {
		md5Hasher := md5.New()
		md5Res = md5Hasher
		writers = io.MultiWriter(hSha1, md5Hasher)
	}

	if _, err := io.Copy(writers, f); err != nil {
		return "", "", err
	}

	sha1Str := hex.EncodeToString(hSha1.Sum(nil))
	md5Str := ""
	if computeMD5 {
		md5Str = hex.EncodeToString(md5Res.Sum(nil))
	}

	return sha1Str, md5Str, nil
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
