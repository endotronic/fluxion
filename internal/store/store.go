package store

import "file-hasher/internal/models"

type Store interface {
	CreateSnapshot(rootPath string) (*models.Snapshot, error)
	GetLastSnapshot(rootPath string) (*models.Snapshot, error)
	CompleteSnapshot(id int64) error
	AddFile(f *models.FileRecord) error
	BatchAddFiles(files []*models.FileRecord) error
	GetFileCount(snapshotID int64) (int64, error)
	GetFilesForSnapshot(snapshotID int64, onProgress func(int)) (map[string]models.FileRecord, error)
	Close() error
}
