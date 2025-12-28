package diff

import (
	"fluxion/internal/models"
	"testing"
)

func TestCompareSnapshots(t *testing.T) {
	tests := []struct {
		name   string
		filesA map[string]models.FileRecord
		filesB map[string]models.FileRecord
		rootA  string
		rootB  string

		strategy string // "sha1" or "md5", defaults to "sha1"
		noCopies bool
		noMoves  bool
		want     []DiffResult
	}{
		{
			name: "Simple File Addition",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1":  {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1":  {SHA1: "hash1"},
				"/root/file2":  {SHA1: "hash2"},
			},
			want: []DiffResult{
				{Path: "/root/file2", Status: StatusAdded},
			},
		},
		{
			name: "Simple File Removal",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1":  {SHA1: "hash1"},
				"/root/file2":  {SHA1: "hash2"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1":  {SHA1: "hash1"},
			},
			want: []DiffResult{
				{Path: "/root/file2", Status: StatusRemoved},
			},
		},
		{
			name: "Simple File Modification",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1":  {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1":  {SHA1: "hash2"},
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
				"/root/common":    {SHA1: "common"},
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
				"/root/common":    {SHA1: "common"},
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
				"/root/common":    {SHA1: "common"},
				"/root/old/file1": {SHA1: "hash1"},
				"/root/old/noise": {SHA1: "noise"}, // Prevents /root/old/ from matching /root/new/
			},
			filesB: map[string]models.FileRecord{
				"/root/common":    {SHA1: "common"},
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
				"/root/common":    {SHA1: "common"},
				"/root/old/file1": {SHA1: "hash1"},
				"/root/old/file2": {SHA1: "hash2"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common":    {SHA1: "common"},
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
				"/root/file1":  {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1":  {SHA1: "hash1"}, // Exists
				"/root/file2":  {SHA1: "hash1"}, // Copied
			},
			want: []DiffResult{
				{Path: "/root/file2", Status: StatusCopy, SourcePath: "/root/file1"},
			},
		},
		{
			name: "Directory Copy (Collapsed)",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/d1/f1":  {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/d1/f1":  {SHA1: "hash1"},
				"/root/d2/f1":  {SHA1: "hash1"},
			},
			want: []DiffResult{
				{Path: "/root/d2/", Status: StatusCopy, SourcePath: "/root/d1/"},
			},
		},
		{
			name: "Mixed Operation (Mod + Add)",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1":  {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1":  {SHA1: "hash2"}, // Modified
				"/root/file2":  {SHA1: "hash3"}, // Added
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
				"/root/common":        {SHA1: "common"},
				"/root/src/sub/file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common":        {SHA1: "common"},
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
				"/root/fileA":  {SHA1: "hashA"},
				"/root/fileB":  {SHA1: "hashB"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/fileA":  {SHA1: "hashB"}, // Became hashB (Move from fileB)
				"/root/fileB":  {SHA1: "hashA"}, // Became hashA (Move from fileA)
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
				"/root/common":    {SHA1: "common"},
				"/root/src/file1": {SHA1: "hash1"},
				"/root/src/file2": {SHA1: "hash2"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common":    {SHA1: "common"},
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
				"/root/file1":  {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common":       {SHA1: "common"},
				"/root/subdir/file1": {SHA1: "hash1"}, // Moved
			},
			want: []DiffResult{
				{Path: "/root/subdir/file1", Status: StatusMove, SourcePath: "/root/file1"},
			},
		},
		{
			name: "MD5 Fallback Match",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common", MD5: "c_md5"},
				"/root/file1":  {MD5: "m1"}, // Only MD5 available (e.g. legacy import)
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common", MD5: "c_md5"},
				"/root/file1":  {MD5: "m1", SHA1: "s1"}, // New scan has both
			},
			strategy: "md5",
			want:     []DiffResult{
				// file1 should be Unchanged because MD5 matches
			},
		},
		{
			name: "MD5 Fallback Diff",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common", MD5: "c_md5"},
				"/root/file1":  {MD5: "m1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common", MD5: "c_md5"},
				"/root/file1":  {MD5: "m2", SHA1: "s2"}, // MD5 mismatch
			},
			strategy: "md5",
			want: []DiffResult{
				{Path: "/root/file1", Status: StatusModified},
			},
		},
		{
			name: "Mixed Algo Mismatch",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1":  {SHA1: "s1"}, // Only SHA1
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1":  {MD5: "m1"}, // Only MD5 (e.g. forced MD5 scan only?)
			},
			want: []DiffResult{
				{Path: "/root/file1", Status: StatusModified},
			},
		},
		{
			name: "Relocated Root Unchanged",
			filesA: map[string]models.FileRecord{
				"file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"file1": {SHA1: "hash1"},
			},
			rootA: "/mnt/A",
			rootB: "/mnt/B",
			want:  []DiffResult{}, // Should be unchanged despite root diff
		},
		{
			name: "Relocated Root Modified",
			filesA: map[string]models.FileRecord{
				"file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"file1": {SHA1: "hash2"},
			},
			rootA: "/mnt/A",
			rootB: "/mnt/B",
			want: []DiffResult{
				{Path: "/mnt/B/file1", Status: StatusModified},
			},
		},
		{
			name: "No Moves Option",
			filesA: map[string]models.FileRecord{
				"/root/file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/file2": {SHA1: "hash1"},
			},
			noMoves: true,
			// If No Moves, it should fall back to Copy because it exists in A.
			// Wait, file1 is Removed in B. file2 is Added in B.
			// If we skip Move, file2 logic sees hash1 is in existingMap (from file1).
			// So it becomes Copy.
			// And file1 remains Removed.
			want: []DiffResult{
				{Path: "/root/file1", Status: StatusRemoved},
				{Path: "/root/file2", Status: StatusCopy, SourcePath: "/root/file1"},
			},
		},
		{
			name: "No Moves Option (Explicitly)",
			filesA: map[string]models.FileRecord{
				"/root/file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/file2": {SHA1: "hash1"},
			},
			noMoves:  true,
			noCopies: true, // Also disable Copies to force Add/Remove
			want: []DiffResult{
				{Path: "/root/file1", Status: StatusRemoved},
				{Path: "/root/file2", Status: StatusAdded},
			},
		},
		{
			name: "No Copies Option",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1":  {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1":  {SHA1: "hash1"},
				"/root/file2":  {SHA1: "hash1"}, // Would be copy
			},
			noCopies: true,
			want: []DiffResult{
				{Path: "/root/file2", Status: StatusAdded},
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

			rootA := tt.rootA
			rootB := tt.rootB

			if rootA == "" {
				rootA = "/"
			}
			if rootB == "" {
				rootB = "/"
			}

			// For specific tests, override roots to test reconstruction logic if needed

			strategy := tt.strategy
			if strategy == "" {
				strategy = "sha1"
			}

			got, err := CompareSnapshots(tt.filesA, tt.filesB, rootA, rootB, strategy, tt.noCopies, tt.noMoves, nil)
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
