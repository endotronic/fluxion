package dupes

import (
	"fluxion/internal/models"
	"slices"
	"testing"
)

func TestFindDuplicates(t *testing.T) {
	tests := []struct {
		name       string
		files      map[string]models.FileRecord
		minSize    int64
		wantHashes []string        // Hashes of groups we expect (simplified check)
		wantPaths  map[string]bool // Paths validation?

		// wantDirPaths checks for a directory-level group with exactly these
		// paths, without depending on the internal hash's exact value (an
		// opaque digest as of the 2.2 fix, not something a test should predict).
		wantDirPaths []string
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
			// Directory hashes are an internal digest (internal/diff's
			// digestEntries, ported here - see knowledge/known-issues.md issue
			// 2.2), not a value any caller should compute or predict, so this
			// case is checked below by structure (IsDir + Paths) instead of by
			// a literal expected hash string. FindDuplicates should report the
			// DIRECTORY group, not separate files, since /root/dirA and
			// /root/dirB have identical structure and content: report {dirA,
			// dirB} at the directory level, and suppress dirA/file1 as covered
			// by its already-reported parent.
			name: "Directory Duplicate (Collapsed)",
			files: map[string]models.FileRecord{
				"/root/dirA/file1": {SHA1: "h1", SizeBytes: 100},
				"/root/dirB/file1": {SHA1: "h1", SizeBytes: 100},
			},
			minSize:      1,
			wantDirPaths: []string{"/root/dirA", "/root/dirB"},
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

			if len(tt.wantHashes) == 0 && len(tt.wantDirPaths) == 0 {
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

			if len(tt.wantDirPaths) > 0 {
				found := false
				for _, g := range groups {
					if g.IsDir && slices.Equal(g.Paths, tt.wantDirPaths) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected a directory group with paths %v not found. Got: %+v", tt.wantDirPaths, groups)
				}
			}
		})
	}
}
