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
	DeleteSnapshot(id int64) error
	Close() error
}
