package sqlite

import (
	"os"
	"testing"
	"time"

	"fluxion/internal/models"

	"github.com/stretchr/testify/assert"
)

func setupTestDB(t *testing.T) (*SqliteStore, string) {
	tmpFile, err := os.CreateTemp("", "fluxion_test_*.db")
	if err != nil {
		t.Fatalf("Failed to create temp DB: %v", err)
	}
	tmpFile.Close()

	store, err := NewSqliteStore(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to open DB: %v", err)
	}

	return store, tmpFile.Name()
}

func TestSearchFiles(t *testing.T) {
	store, dbPath := setupTestDB(t)
	defer os.Remove(dbPath)
	defer store.Close()

	snap, err := store.CreateSnapshot("/test/root", "test_snap", "host")
	assert.NoError(t, err)

	// 2. Add Files
	files := []*models.FileRecord{
		{SnapshotID: snap.ID, Path: "/test/root/file1.txt", Filename: "file1.txt", SizeBytes: 100, ModTime: time.Now(), SHA1: "dummy", MD5: "dummy"},
		{SnapshotID: snap.ID, Path: "/test/root/FILE2.TXT", Filename: "FILE2.TXT", SizeBytes: 100, ModTime: time.Now(), SHA1: "dummy", MD5: "dummy"},
		{SnapshotID: snap.ID, Path: "/test/root/other.go", Filename: "other.go", SizeBytes: 100, ModTime: time.Now(), SHA1: "dummy", MD5: "dummy"},
		{SnapshotID: snap.ID, Path: "/test/root/subdir/subfile.txt", Filename: "subfile.txt", SizeBytes: 100, ModTime: time.Now(), SHA1: "dummy", MD5: "dummy"},
	}
	err = store.BatchAddFiles(files)
	if err != nil {
		t.Fatalf("Failed to add files: %v", err)
	}

	type testCase struct {
		name          string
		pattern       string
		caseSensitive bool
		want          []string
	}

	tests := []testCase{
		{
			name:          "Exact match case-insensitive (default)",
			pattern:       "file1.txt",
			caseSensitive: false,
			want:          []string{"/test/root/file1.txt"},
		},
		{
			name:          "Exact match case-sensitive",
			pattern:       "file1.txt",
			caseSensitive: true,
			want:          []string{"/test/root/file1.txt"},
		},
		{
			name:          "Case mismatch insensitive",
			pattern:       "FILE1.TXT",
			caseSensitive: false,
			want:          []string{"/test/root/file1.txt"}, // "FILE1.TXT" lower="file1.txt". file in db="file1.txt". Match.
		},
		{
			name:          "Case mismatch insensitive FILE2",
			pattern:       "file2.txt",
			caseSensitive: false,
			want:          []string{"/test/root/FILE2.TXT"}, // "file2.txt" -> "file2.txt". DB "FILE2.TXT" -> "file2.txt". Match.
		},
		{
			name:          "Case mismatch sensitive",
			pattern:       "FILE1.TXT",
			caseSensitive: true,
			want:          []string{},
		},
		{
			name:          "Wildcard match insensitive",
			pattern:       "*.txt",
			caseSensitive: false,
			want:          []string{"/test/root/file1.txt", "/test/root/FILE2.TXT", "/test/root/subdir/subfile.txt"},
		},
		{
			name:          "Wildcard match sensitive",
			pattern:       "*.txt",
			caseSensitive: true,
			want:          []string{"/test/root/file1.txt", "/test/root/subdir/subfile.txt"},
		},
		{
			name:          "Question mark wildcard",
			pattern:       "file?.txt",
			caseSensitive: false,
			want:          []string{"/test/root/file1.txt", "/test/root/FILE2.TXT"},
		},
		{
			name:          "Wildcard match insensitive upper pattern",
			pattern:       "*.TXT",
			caseSensitive: false,
			want:          []string{"/test/root/file1.txt", "/test/root/FILE2.TXT", "/test/root/subdir/subfile.txt"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			err := store.SearchFiles(snap.ID, tc.pattern, tc.caseSensitive, func(f models.FileRecord) error {
				got = append(got, f.Path)
				return nil
			})
			assert.NoError(t, err)
			assert.ElementsMatch(t, tc.want, got)
		})
	}
}
