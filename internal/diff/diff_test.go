package diff

import (
	"fluxion/internal/models"
	"sort"
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
				{Path: "/root/file2", Root: "/", RelPath: "root/file2", Status: StatusAdded, AddedCount: 1},
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
				{Path: "/root/file2", Root: "/", RelPath: "root/file2", Status: StatusRemoved, RemovedCount: 1},
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
				{Path: "/root/file1", Root: "/", RelPath: "root/file1", Status: StatusModified, ModifiedCount: 1},
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
				{Path: "/root/dir/", Root: "/", RelPath: "root/dir/", Status: StatusAdded, AddedCount: 2},
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
				{Path: "/root/dir/", Root: "/", RelPath: "root/dir/", Status: StatusRemoved, RemovedCount: 2},
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
				{Path: "/root/new/file1", Root: "/", RelPath: "root/new/file1", Status: StatusMove, SourcePath: "/root/old/file1", SourceRoot: "/", SourceRelPath: "root/old/file1", MoveCount: 1},
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
				{Path: "/root/new/", Root: "/", RelPath: "root/new/", Status: StatusMove, SourcePath: "/root/old/", SourceRoot: "/", SourceRelPath: "root/old/", MoveCount: 2},
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
				{Path: "/root/file2", Root: "/", RelPath: "root/file2", Status: StatusCopy, SourcePath: "/root/file1", SourceRoot: "/", SourceRelPath: "root/file1", CopyCount: 1},
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
				{Path: "/root/d2/", Root: "/", RelPath: "root/d2/", Status: StatusCopy, SourcePath: "/root/d1/", SourceRoot: "/", SourceRelPath: "root/d1/", CopyCount: 1},
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
				{Path: "/root/file1", Root: "/", RelPath: "root/file1", Status: StatusModified, ModifiedCount: 1},
				{Path: "/root/file2", Root: "/", RelPath: "root/file2", Status: StatusAdded, AddedCount: 1},
			},
		},
		{
			name: "Mixed Operation (Mod + Add) Collapsed",
			filesA: map[string]models.FileRecord{
				"/root/common":       {SHA1: "common"},
				"/root/parent/file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common":       {SHA1: "common"},
				"/root/parent/file1": {SHA1: "hash2"}, // Modified
				"/root/parent/file2": {SHA1: "hash3"}, // Added
			},
			want: []DiffResult{
				{Path: "/root/parent/", Root: "/", RelPath: "root/parent/", Status: StatusModified, ModifiedCount: 1, AddedCount: 1},
			},
		},
		{
			name: "Move vs Copy Preference",
			// Ideally a Move is consumed first.
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1":  {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file2":  {SHA1: "hash1"}, // Move
				"/root/file3":  {SHA1: "hash1"}, // Copy
			},
			want: []DiffResult{
				{Path: "/root/file2", Root: "/", RelPath: "root/file2", Status: StatusMove, SourcePath: "/root/file1", SourceRoot: "/", SourceRelPath: "root/file1", MoveCount: 1},
				{Path: "/root/file3", Root: "/", RelPath: "root/file3", Status: StatusCopy, SourcePath: "/root/file1", SourceRoot: "/", SourceRelPath: "root/file1", CopyCount: 1},
			},
		},
		{
			name: "Move vs Copy Preference (Nested)",
			filesA: map[string]models.FileRecord{
				"/root/common":       {SHA1: "common"},
				"/root/parent/file1": {SHA1: "hash1"},
				"/root/parent/file2": {SHA1: "hash2"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common":       {SHA1: "common"},
				"/root/parent/file1": {SHA1: "hash1"}, // Same
				"/root/parent/file3": {SHA1: "hash2"}, // Move
				"/root/parent/file4": {SHA1: "hash2"}, // Copy
			},
			want: []DiffResult{
				{Path: "/root/parent/file3", Root: "/", RelPath: "root/parent/file3", Status: StatusMove, SourcePath: "/root/parent/file2", SourceRoot: "/", SourceRelPath: "root/parent/file2", MoveCount: 1},
				{Path: "/root/parent/file4", Root: "/", RelPath: "root/parent/file4", Status: StatusCopy, SourcePath: "/root/parent/file2", SourceRoot: "/", SourceRelPath: "root/parent/file2", CopyCount: 1},
			},
		},
		{
			name: "Nested Directory Move",
			filesA: map[string]models.FileRecord{
				"/root/common":        {SHA1: "common"},
				"/root/src/sub/file1": {SHA1: "hash1"},
				"/root/src/sub/file2": {SHA1: "hash2"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common":        {SHA1: "common"},
				"/root/dst/sub/file1": {SHA1: "hash1"},
				"/root/dst/sub/file2": {SHA1: "hash2"},
			},
			want: []DiffResult{
				{Path: "/root/dst/", Root: "/", RelPath: "root/dst/", Status: StatusMove, SourcePath: "/root/src/", SourceRoot: "/", SourceRelPath: "root/src/", MoveCount: 2},
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
				{Path: "/root/fileA", Root: "/", RelPath: "root/fileA", Status: StatusMove, SourcePath: "/root/fileB", SourceRoot: "/", SourceRelPath: "root/fileB", MoveCount: 1},
				{Path: "/root/fileB", Root: "/", RelPath: "root/fileB", Status: StatusMove, SourcePath: "/root/fileA", SourceRoot: "/", SourceRelPath: "root/fileA", MoveCount: 1},
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
				{Path: "/root/dst/file1", Root: "/", RelPath: "root/dst/file1", Status: StatusMove, SourcePath: "/root/src/file1", SourceRoot: "/", SourceRelPath: "root/src/file1", MoveCount: 1},
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
				{Path: "/root/subdir/file1", Root: "/", RelPath: "root/subdir/file1", Status: StatusMove, SourcePath: "/root/file1", SourceRoot: "/", SourceRelPath: "root/file1", MoveCount: 1},
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
				{Path: "/root/file1", Root: "/", RelPath: "root/file1", Status: StatusModified, ModifiedCount: 1},
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
				{Path: "/root/file1", Root: "/", RelPath: "root/file1", Status: StatusModified, ModifiedCount: 1},
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
				{Path: "/mnt/A/file1", Root: "/mnt/A", RelPath: "file1", Status: StatusModified, ModifiedCount: 1},
			},
		},
		{
			name: "Relocated Root Move",
			filesA: map[string]models.FileRecord{
				"file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"file2": {SHA1: "hash1"},
			},
			rootA: "/mnt/A",
			rootB: "/mnt/B",
			want: []DiffResult{
				{Path: "/mnt/B/file2", Root: "/mnt/B", RelPath: "file2", Status: StatusMove, SourcePath: "/mnt/A/file1", SourceRoot: "/mnt/A", SourceRelPath: "file1", MoveCount: 1},
			},
		},
		{
			name: "No Moves Option",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1":  {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file2":  {SHA1: "hash1"},
			},
			noMoves: true,
			want: []DiffResult{
				{Path: "/root/file1", Root: "/", RelPath: "root/file1", Status: StatusRemoved, RemovedCount: 1},
				{Path: "/root/file2", Root: "/", RelPath: "root/file2", Status: StatusCopy, SourcePath: "/root/file1", SourceRoot: "/", SourceRelPath: "root/file1", CopyCount: 1},
			},
		},
		{
			name: "No Moves Option (Explicitly)",
			filesA: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file1":  {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common": {SHA1: "common"},
				"/root/file2":  {SHA1: "hash1"},
			},
			noMoves:  true,
			noCopies: true, // Also disable Copies to force Add/Remove
			want: []DiffResult{
				{Path: "/root/file1", Root: "/", RelPath: "root/file1", Status: StatusRemoved, RemovedCount: 1},
				{Path: "/root/file2", Root: "/", RelPath: "root/file2", Status: StatusAdded, AddedCount: 1},
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
				{Path: "/root/file2", Root: "/", RelPath: "root/file2", Status: StatusAdded, AddedCount: 1},
			},
		},
		{
			name: "Rollup Modified (Mod + Add)",
			filesA: map[string]models.FileRecord{
				"/root/file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/file1": {SHA1: "hash2"}, // Modified
				"/root/file2": {SHA1: "hash3"}, // Added
			},
			want: []DiffResult{
				{Path: "/root/", Root: "/", RelPath: "root/", Status: StatusModified, ModifiedCount: 1, AddedCount: 1},
			},
		},
		{
			name: "Rollup Modified (Mod + Add + Copy)",
			filesA: map[string]models.FileRecord{
				"/root/file1": {SHA1: "hash1"},
				"/root/file2": {SHA1: "hash2"},
			},
			filesB: map[string]models.FileRecord{
				"/root/file1": {SHA1: "hash1"}, // Same
				"/root/file2": {SHA1: "hash3"}, // Modified
				"/root/file3": {SHA1: "hash2"}, // Copy
				"/root/file4": {SHA1: "hash4"}, // Added
			},
			want: []DiffResult{
				{Path: "/root/", Root: "/", RelPath: "root/", Status: StatusModified, ModifiedCount: 1, AddedCount: 1, CopyCount: 1},
			},
		},
		{
			name: "Rollup Added (Add + Copy)",
			filesA: map[string]models.FileRecord{
				"/other/file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/other/file1": {SHA1: "hash1"},
				"/root/file1":  {SHA1: "hash1"}, // Copy
				"/root/file2":  {SHA1: "hash2"}, // Added
			},
			want: []DiffResult{
				{Path: "/root/", Root: "/", RelPath: "root/", Status: StatusAdded, CopyCount: 1, AddedCount: 1},
			},
		},
		{
			name: "Rollup Modified (Mod + Add + Copy + Removed)",
			filesA: map[string]models.FileRecord{
				"/root/file1": {SHA1: "hash1"},
				"/root/file2": {SHA1: "hash2"},
				"/root/file3": {SHA1: "hash3"}, // Removed
			},
			filesB: map[string]models.FileRecord{
				"/root/file1": {SHA1: "hash1"}, // Same
				"/root/file2": {SHA1: "hash4"}, // Modified
				"/root/file4": {SHA1: "hash2"}, // Copy
				"/root/file5": {SHA1: "hash5"}, // Added
			},
			want: []DiffResult{
				{Path: "/root/", Root: "/", RelPath: "root/", Status: StatusModified, ModifiedCount: 1, AddedCount: 1, CopyCount: 1, RemovedCount: 1},
			},
		},
		{
			name: "Rollup Modified (Move + Copy)",
			filesA: map[string]models.FileRecord{
				"/root/common":       {SHA1: "common"},
				"/root/parent/file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/root/common":       {SHA1: "common"},
				"/root/parent/file2": {SHA1: "hash1"}, // Move
				"/root/parent/file3": {SHA1: "hash1"}, // Copy
			},
			want: []DiffResult{
				{Path: "/root/parent/", Root: "/", RelPath: "root/parent/", Status: StatusModified, MoveCount: 1, CopyCount: 1},
			},
		},
		{
			name: "Rollup Modified Different Root (Mod + Add + Copy + Removed)",
			filesA: map[string]models.FileRecord{
				"foo/file1": {SHA1: "hash1"},
				"foo/file2": {SHA1: "hash2"},
				"foo/file3": {SHA1: "hash3"}, // Removed
			},
			filesB: map[string]models.FileRecord{
				"foo/file1": {SHA1: "hash1"}, // Same
				"foo/file2": {SHA1: "hash4"}, // Modified
				"foo/file4": {SHA1: "hash2"}, // Copy
				"foo/file5": {SHA1: "hash5"}, // Added
			},
			rootA: "/mnt/A",
			rootB: "/mnt/B",
			want: []DiffResult{
				{Path: "/mnt/A/foo/", Root: "/mnt/A", RelPath: "foo/", Status: StatusModified, ModifiedCount: 1, AddedCount: 1, CopyCount: 1, RemovedCount: 1},
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

			got, err := CompareSnapshots(tt.filesA, tt.filesB, rootA, rootB, strategy, tt.noCopies, tt.noMoves, false, nil)
			if err != nil {
				t.Errorf("CompareSnapshots() error = %v", err)
				return
			}

			// Sort got by Path to match expectation logic usually
			// CompareSnapshots already sorts by path.

			// Sort expectation dynamically to avoid rewriting all test cases
			sortedWant := make([]DiffResult, len(tt.want))
			copy(sortedWant, tt.want)
			sort.Slice(sortedWant, func(i, j int) bool {
				return sortedWant[i].RelPath > sortedWant[j].RelPath
			})

			// Check len
			if len(got) != len(sortedWant) {
				t.Errorf("CompareSnapshots() got len %d, want %d.\nGot: %+v\nWant: %+v", len(got), len(sortedWant), got, sortedWant)
				return
			}

			// Check content
			for i := range got {
				// Normalize SourcePaths for directory matches (ensure trailing slash consistency if needed)
				// My implementaton might add trailing slash to dirs.
				// Let's rely on strict check for now and fix impl if needed.
				if got[i].Path != sortedWant[i].Path || got[i].Status != sortedWant[i].Status || got[i].SourcePath != sortedWant[i].SourcePath ||
					got[i].Root != sortedWant[i].Root || got[i].RelPath != sortedWant[i].RelPath ||
					got[i].SourceRoot != sortedWant[i].SourceRoot || got[i].SourceRelPath != sortedWant[i].SourceRelPath ||
					got[i].AddedCount != sortedWant[i].AddedCount || got[i].RemovedCount != sortedWant[i].RemovedCount ||
					got[i].ModifiedCount != sortedWant[i].ModifiedCount || got[i].CopyCount != sortedWant[i].CopyCount ||
					got[i].MoveCount != sortedWant[i].MoveCount || got[i].UnchangedFileCount != sortedWant[i].UnchangedFileCount ||
					got[i].UnchangedDirCount != sortedWant[i].UnchangedDirCount {
					t.Errorf("CompareSnapshots() mismatch at index %d.\nGot:  %+v\nWant: %+v", i, got[i], sortedWant[i])
				}
			}
		})
	}
}
