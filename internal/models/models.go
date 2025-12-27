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
	ID         int64
	Name       string
	RootPath   string
	StartedAt  time.Time
	FinishedAt *time.Time
	Status     SnapshotStatus
	
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
