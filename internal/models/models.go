package models

import (
	"time"
)

type SnapshotStatus string

const (
	StatusInProgress SnapshotStatus = "in_progress"
	StatusCompleted  SnapshotStatus = "completed"
	StatusFailed     SnapshotStatus = "failed"
	StatusDeleted    SnapshotStatus = "deleted"
)

type Snapshot struct {
	ID           int64
	Name         string
	RootPath     string
	ComputerName string
	StartedAt    time.Time
	FinishedAt   *time.Time
	Status       SnapshotStatus

	// ErrorCount is how many files the scan could not read. A snapshot with a
	// non-zero count is incomplete: anything it is missing will look deleted in a
	// later diff. Always 0 for snapshots taken before schema version 3.
	ErrorCount int64
	
	// Available Hashes (e.g. "sha1", "md5")
	Hashes []string
}

type FileRecord struct {
	ID         int64
	SnapshotID int64
	Path       string
	Filename   string
	SizeBytes  int64
	ModTime    time.Time
	SHA1       string
	MD5        string
}
