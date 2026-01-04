package app

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"os"

	"fluxion/internal/models"
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"

	"github.com/schollz/progressbar/v3"
	"github.com/sirupsen/logrus"
)

type ExportLegacyConfig struct {
	DBPath     string
	SnapQuery  string
	ExportName string
}

func RunExportLegacy(cfg ExportLegacyConfig) error {
	if cfg.DBPath == "" {
		return fmt.Errorf("DB path is required")
	}
	if cfg.SnapQuery == "" {
		return fmt.Errorf("snapshot name or ID is required")
	}

	// Open DB
	var dbStore store.Store
	var err error
	dbStore, err = sqlite.NewSqliteStore(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("error opening DB: %w", err)
	}
	defer dbStore.Close()

	// Find Snapshot
	snap, err := dbStore.FindSnapshot(cfg.SnapQuery)
	if err != nil {
		return fmt.Errorf("error finding snapshot: %w", err)
	}

	// Check for MD5
	hasMD5 := false
	for _, h := range snap.Hashes {
		if h == "md5" {
			hasMD5 = true
			break
		}
	}
	if !hasMD5 {
		return fmt.Errorf("snapshot '%s' (ID: %d) does not contain MD5 hashes, cannot export to legacy format", snap.Name, snap.ID)
	}

	logrus.Infof("Exporting snapshot '%s' (ID: %d)...", snap.Name, snap.ID)

	exportName := cfg.ExportName
	if exportName == "" {
		exportName = snap.Name
	}

	hashesFileName := fmt.Sprintf("%s_files_hashes.txt", exportName)
	sizesFileName := fmt.Sprintf("%s_files_sizes.txt", exportName)

	// Check sizes
	hasSizes, err := dbStore.HasSizes(snap.ID)
	if err != nil {
		return fmt.Errorf("error checking sizes: %w", err)
	}

	if hasSizes {
		logrus.Infof("Output files: %s, %s", hashesFileName, sizesFileName)
	} else {
		logrus.Infof("Output file: %s (Skipping sizes file as no sizes detected)", hashesFileName)
	}

	// Create output files
	hashesFile, err := os.Create(hashesFileName)
	if err != nil {
		return fmt.Errorf("error creating hashes file: %w", err)
	}
	defer hashesFile.Close()

	hashesWriter := bufio.NewWriter(hashesFile)
	defer hashesWriter.Flush()

	var sizesWriter *bufio.Writer
	if hasSizes {
		sizesFile, err := os.Create(sizesFileName)
		if err != nil {
			return fmt.Errorf("error creating sizes file: %w", err)
		}
		defer sizesFile.Close()

		sizesWriter = bufio.NewWriter(sizesFile)
		defer sizesWriter.Flush()
	}

	// Setup Progress Bar
	count, _ := dbStore.GetFileCount(snap.ID)
	bar := progressbar.Default(count, "Exporting files")

	err = dbStore.IterateFiles(snap.ID, func(f models.FileRecord) error {
		// Encode Path
		b64Path := base64.StdEncoding.EncodeToString([]byte(f.Path))

		// Hashes File: MD5  utf-8  BASE64_PATH
		hash := f.MD5
		if hash == "" {
			hash = "-" // Should not happen if hasMD5 check passed, unless specific files missed it
		}

		if _, err := fmt.Fprintf(hashesWriter, "%s  utf-8  %s\n", hash, b64Path); err != nil {
			return err
		}

		// Sizes File: SIZE  utf-8  BASE64_PATH
		if hasSizes && sizesWriter != nil {
			if _, err := fmt.Fprintf(sizesWriter, "%d  utf-8  %s\n", f.SizeBytes, b64Path); err != nil {
				return err
			}
		}

		bar.Add(1)
		return nil
	})

	if err != nil {
		return fmt.Errorf("error exporting files: %w", err)
	}

	bar.Finish()
	fmt.Println()
	logrus.Info("Export completed successfully.")

	return nil
}
