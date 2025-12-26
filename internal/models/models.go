package models

import (
	"time"
)

type SnapshotStatus string

const (
	StatusInProgress SnapshotStatus = "in_progress"
	StatusCompleted  SnapshotStatus = "completed"
	StatusFailed     SnapshotStatus = "failed"
)

type Snapshot struct {
	ID         int64
	RootPath   string
	StartedAt  time.Time
	FinishedAt *time.Time
	Status     SnapshotStatus
}

type FileRecord struct {
	ID         int64
	SnapshotID int64
	Path       string
	Filename   string
	SizeBytes  int64
	ModTime    time.Time
	SHA1       string
}
