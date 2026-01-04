package store

import (
	"fluxion/internal/models"
	"time"
)

type Store interface {
	CreateSnapshot(rootPath, name, computerName string) (*models.Snapshot, error)
	GetLastSnapshot(rootPath string) (*models.Snapshot, error)
	FindSnapshot(query string) (*models.Snapshot, error)
	ListSnapshots() ([]*models.Snapshot, error)
	CompleteSnapshot(id int64, finishedAt time.Time) error
	AddFile(f *models.FileRecord) error
	BatchAddFiles(files []*models.FileRecord) error
	GetFileCount(snapshotID int64) (int64, error)
	GetFilesForSnapshot(snapshotID int64, onProgress func(int)) (map[string]models.FileRecord, error)
	IterateFiles(snapshotID int64, onFile func(models.FileRecord) error) error
	SearchFiles(snapshotID int64, pattern string, caseSensitive bool, onFile func(models.FileRecord) error) error
	GetFileList(snapshotID int64, onProgress func(int)) ([]*models.FileRecord, error)
	HasSizes(snapshotID int64) (bool, error)
	GetSnapshotBytes(snapshotID int64) (int64, error)
	DeleteSnapshot(id int64) error
	Close() error
}
