package dupes

import (
	"fluxion/internal/models"
	"testing"
)

func TestFindDuplicates(t *testing.T) {
	tests := []struct {
		name       string
		files      map[string]models.FileRecord
		minSize    int64
		wantHashes []string        // Hashes of groups we expect (simplified check)
		wantPaths  map[string]bool // Paths validation?
	}{
		{
			name: "No Duplicates",
			files: map[string]models.FileRecord{
				"/root/file1": {SHA1: "h1", SizeBytes: 100},
				"/root/file2": {SHA1: "h2", SizeBytes: 100},
			},
			minSize:    1,
			wantHashes: nil,
		},
		{
			name: "Simple Duplicates",
			files: map[string]models.FileRecord{
				"/root/file1": {SHA1: "h1", SizeBytes: 100},
				"/root/file2": {SHA1: "h1", SizeBytes: 100},
			},
			minSize:    1,
			wantHashes: []string{"h1"},
		},
		{
			name: "Size Filter",
			files: map[string]models.FileRecord{
				"/root/small1": {SHA1: "h1", SizeBytes: 10},
				"/root/small2": {SHA1: "h1", SizeBytes: 10},
			},
			minSize:    100,
			wantHashes: nil,
		},
		{
			name: "Directory Duplicate (Collapsed)",
			files: map[string]models.FileRecord{
				"/root/dirA/file1": {SHA1: "h1", SizeBytes: 100},
				"/root/dirB/file1": {SHA1: "h1", SizeBytes: 100},
			},
			minSize: 1,
			// Directory hashes are computed internally as Merkle.
			// Check logic: FindDuplicates should report the DIRECTORY group, not separate files if structure matches.
			// DirA hash: "file1:h1"
			// DirB hash: "file1:h1"
			// Wait, FindDuplicates returns file duplicates too if they aren't totally covered?
			// The logic in dupes.go attempts to report "Highest Level".
			// So it should report /root/dirA and /root/dirB as a group.
			// And suppress /root/dirA/file1.
			wantHashes: []string{"file1:h1"}, // This is the merkle hash of the dir!
		},
		{
			name: "Partial Directory Duplicate",
			files: map[string]models.FileRecord{
				"/root/dirA/file1":  {SHA1: "h1", SizeBytes: 100}, // Shared
				"/root/dirA/unique": {SHA1: "u1", SizeBytes: 100},
				"/root/dirB/file1":  {SHA1: "h1", SizeBytes: 100}, // Shared
				"/root/dirB/unique": {SHA1: "u2", SizeBytes: 100},
			},
			minSize: 1,
			// Structure distinct, so dirs don't match.
			// Should report file duplicates.
			wantHashes: []string{"h1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups, err := FindDuplicates(tt.files, tt.minSize, "/root")
			if err != nil {
				t.Fatalf("FindDuplicates() error = %v", err)
			}

			if len(tt.wantHashes) == 0 {
				if len(groups) != 0 {
					t.Errorf("Expected 0 groups, got %d", len(groups))
				}
				return
			}

			// Verify presence of expected groups
			for _, wantH := range tt.wantHashes {
				found := false
				for _, g := range groups {
					if g.Hash == wantH {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected group with hash %s not found. Got: %+v", wantH, groups)
				}
			}
		})
	}
}
