package diff

import (
	"fluxion/internal/models"
	"testing"
)

func TestCompareSnapshots(t *testing.T) {
	tests := []struct {
		name     string
		filesA   map[string]models.FileRecord
		filesB   map[string]models.FileRecord
		want     []DiffResult
	}{
		{
			name: "Simple File Addition",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1": {SHA1: "hash1"},
				"/root/file2": {SHA1: "hash2"},
			},
			want: []DiffResult{
				{Path: "/root/file2", Status: StatusAdded},
			},
		},
		{
			name: "Simple File Removal",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1": {SHA1: "hash1"},
				"/root/file2": {SHA1: "hash2"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1": {SHA1: "hash1"},
			},
			want: []DiffResult{
				{Path: "/root/file2", Status: StatusRemoved},
			},
		},
		{
			name: "Simple File Modification",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1": {SHA1: "hash2"},
			},
			want: []DiffResult{
				{Path: "/root/file1", Status: StatusModified},
			},
		},
		{
			name: "Directory Addition (Collapsed)",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/dir/file1": {SHA1: "hash1"},
				"/root/dir/file2": {SHA1: "hash2"}, // Same dir
			},
			want: []DiffResult{
				{Path: "/root/dir/", Status: StatusAdded},
			},
		},
		{
			name: "Directory Removal (Collapsed)",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/dir/file1": {SHA1: "hash1"},
				"/root/dir/file2": {SHA1: "hash2"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
			},
			want: []DiffResult{
				{Path: "/root/dir/", Status: StatusRemoved},
			},
		},
		{
			name: "Simple File Move",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/old/file1": {SHA1: "hash1"},
				"/root/old/noise": {SHA1: "noise"}, // Prevents /root/old/ from matching /root/new/
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/new/file1": {SHA1: "hash1"}, // Moved
				"/root/old/noise": {SHA1: "noise"}, // Stayed (so /root/old isn't fully removed)
			},
			want: []DiffResult{
				{Path: "/root/new/file1", Status: StatusMove, SourcePath: "/root/old/file1"},
			},
		},
		{
			name: "Directory Move (Collapsed)",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/old/file1": {SHA1: "hash1"},
				"/root/old/file2": {SHA1: "hash2"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/new/file1": {SHA1: "hash1"},
				"/root/new/file2": {SHA1: "hash2"},
			},
			want: []DiffResult{
				{Path: "/root/new/", Status: StatusMove, SourcePath: "/root/old/"},
			},
		},
		{
			name: "Simple File Copy",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1": {SHA1: "hash1"}, // Exists
				"/root/file2": {SHA1: "hash1"}, // Copied
			},
			want: []DiffResult{
				{Path: "/root/file2", Status: StatusCopy, SourcePath: "/root/file1"},
			},
		},
		{
			name: "Directory Copy (Collapsed)",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/d1/f1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/d1/f1": {SHA1: "hash1"},
				"/root/d2/f1": {SHA1: "hash1"},
			},
			want: []DiffResult{
				{Path: "/root/d2/", Status: StatusCopy, SourcePath: "/root/d1/"},
			},
		},
		{
			name: "Mixed Operation (Mod + Add)",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1": {SHA1: "hash2"}, // Modified
				"/root/file2": {SHA1: "hash3"}, // Added
			},
			want: []DiffResult{
				{Path: "/root/file1", Status: StatusModified},
				{Path: "/root/file2", Status: StatusAdded},
			},
		},
		{
			name: "Move vs Copy Preference",
			// Ideally a Move is consumed first.
			filesA: map[string]models.FileRecord{
				"/root/file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/file2": {SHA1: "hash1"}, // Move
				"/root/file3": {SHA1: "hash1"}, // Copy
			},
			want: []DiffResult{
				{Path: "/root/file2", Status: StatusMove, SourcePath: "/root/file1"},
				{Path: "/root/file3", Status: StatusCopy, SourcePath: "/root/file1"},
			},
		},
		{
			name: "Nested Directory Move",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/src/sub/file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/dst/sub/file1": {SHA1: "hash1"},
			},
			want: []DiffResult{
				{Path: "/root/dst/", Status: StatusMove, SourcePath: "/root/src/"},
			},
		},
		{
			name: "File Swap",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/fileA": {SHA1: "hashA"},
				"/root/fileB": {SHA1: "hashB"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/fileA": {SHA1: "hashB"}, // Became hashB (Move from fileB)
				"/root/fileB": {SHA1: "hashA"}, // Became hashA (Move from fileA)
			},
			// Expecting specific moves if logic allows swap detection
			want: []DiffResult{
				{Path: "/root/fileA", Status: StatusMove, SourcePath: "/root/fileB"},
				{Path: "/root/fileB", Status: StatusMove, SourcePath: "/root/fileA"},
			},
		},
		{
			name: "Partial Directory Move",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/src/file1": {SHA1: "hash1"},
				"/root/src/file2": {SHA1: "hash2"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/src/file2": {SHA1: "hash2"}, // Stayed
				"/root/dst/file1": {SHA1: "hash1"}, // Moved
			},
			want: []DiffResult{
				{Path: "/root/dst/file1", Status: StatusMove, SourcePath: "/root/src/file1"},
			},
		},
		{
			name: "Root Level File Move into Subdir",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/subdir/file1": {SHA1: "hash1"}, // Moved
			},
			want: []DiffResult{
				{Path: "/root/subdir/file1", Status: StatusMove, SourcePath: "/root/file1"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Tests mostly used /root as prefix. We can treat /root as the base.
			// But the test data has inputs like "/root/file1".
			// If we pass empty roots, the reconstructor might just use the relative paths (which are absolute strings here).
			// Ideally, we should update test data to be relative?
			// Or we pass root="/"?
			
			// Let's assume input maps are ALREADY relative for the test?
			// The validation in main.go converts absolute to relative.
			// CompareSnapshots EXPECTS relative keys.
			// Currently test data uses keys like "/root/file1".
			// If we pass root="/", then Join("/", "/root/file1") -> "/root/file1".
			// That works.
			
			rootA := "/"
			rootB := "/"
			
			got, err := CompareSnapshots(tt.filesA, tt.filesB, rootA, rootB, nil)
			if err != nil {
				t.Errorf("CompareSnapshots() error = %v", err)
				return
			}
			
			// Sort got by Path to match expectation logic usually
			// CompareSnapshots already sorts by path.
			
			// Check len
			if len(got) != len(tt.want) {
				t.Errorf("CompareSnapshots() got len %d, want %d.\nGot: %+v\nWant: %+v", len(got), len(tt.want), got, tt.want)
				return
			}

			// Check content
			for i := range got {
				// Normalize SourcePaths for directory matches (ensure trailing slash consistency if needed)
				// My implementaton might add trailing slash to dirs.
				// Let's rely on strict check for now and fix impl if needed.
				if got[i].Path != tt.want[i].Path || got[i].Status != tt.want[i].Status || got[i].SourcePath != tt.want[i].SourcePath {
					t.Errorf("CompareSnapshots() mismatch at index %d.\nGot:  %+v\nWant: %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}
