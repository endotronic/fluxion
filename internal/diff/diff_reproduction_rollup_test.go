package diff

import (
	"fluxion/internal/models"
	"testing"
)

func TestReproduction_Rollup_Blocking(t *testing.T) {
	tests := []struct {
		name   string
		filesA map[string]models.FileRecord
		filesB map[string]models.FileRecord
		rootA  string
		rootB  string

		strategy string
		want     []DiffResult
	}{
		{
			name: "Rollup Blocked by Single Copy Child",
			// Scenario:
			// /target/ is new.
			// /target/child_copy_dir/file1 is a copy of /source/file1. (Single copy in this dir)
			// /target/file2 is Added
			// /target/file3 is Added
			// /target/file4 is Added
			//
			// Current Behavior Hypothesis:
			// /target/child_copy_dir/ becomes StatusMixed (Single Move/Copy rule).
			// /target/ becomes StatusMixed (because it contains a Mixed child).
			//
			// Desired Behavior (User Request):
			// /target/ should rollup (StatusAdded/StatusModified) because it is entirely changed (changeCount > 2).
			filesA: map[string]models.FileRecord{
				"/source/file1": {SHA1: "hash1", SizeBytes: 100},
				"/source/noise": {SHA1: "noise", SizeBytes: 100}, // Prevents child_copy_dir from being a Dir Copy of source
			},
			filesB: map[string]models.FileRecord{
				"/source/file1":                {SHA1: "hash1", SizeBytes: 100},
				"/source/noise":                {SHA1: "noise", SizeBytes: 100},
				"/target/child_copy_dir/file1": {SHA1: "hash1", SizeBytes: 100}, // Copy
				"/target/file2":                {SHA1: "hash2", SizeBytes: 100}, // Added
				"/target/file3":                {SHA1: "hash3", SizeBytes: 100}, // Added
				"/target/file4":                {SHA1: "hash4", SizeBytes: 100}, // Added
			},
			want: []DiffResult{
				// Ideally:
				{Path: "/target/", Root: "/", RelPath: "target/", Status: StatusModified, AddedCount: 3, CopyCount: 1},
				// Or StatusModified. But since /target/ is new, StatusAdded is preferred if Pure AddedLike.
				// But "Mixed" child prevents "allAddedLike".
			},
		},
		{
			name: "Mixed Rollup Blocked by Unchanged File",
			// Scenario:
			// /target/ has Changed files AND Unchanged files.
			// It should NOT rollup to StatusModified (collapsed), but StatusMixed (Expanded).
			filesA: map[string]models.FileRecord{
				"/target/unchanged": {SHA1: "hash1", SizeBytes: 100},
			},
			filesB: map[string]models.FileRecord{
				"/target/unchanged": {SHA1: "hash1", SizeBytes: 100},
				"/target/added1":    {SHA1: "hash2", SizeBytes: 100}, // Added
				"/target/added2":    {SHA1: "hash3", SizeBytes: 100}, // Added
				"/target/added3":    {SHA1: "hash4", SizeBytes: 100}, // Added
			},
			want: []DiffResult{
				// Should show children because it didn't collapse.
				// /target/ is Mixed.
				// Children: added1, added2, added3.
				// Since showUnchanged is false (in test call), unchanged is not shown.
				// And parent /target/ (Mixed) is not shown unless it matches specific conditions?
				// collectResults only prints Mixed parent IF showUnchanged is true (line 752).
				// So we expect only children.
				{Path: "/target/added1", Root: "/", RelPath: "target/added1", Status: StatusAdded, AddedCount: 1},
				{Path: "/target/added2", Root: "/", RelPath: "target/added2", Status: StatusAdded, AddedCount: 1},
				{Path: "/target/added3", Root: "/", RelPath: "target/added3", Status: StatusAdded, AddedCount: 1},
			},
		},
		{
			name: "Aggressive Rollup Check (Many Changes + Unchanged)",
			// Scenario:
			// /target/ has > 2 changes (Added/Modified/etc) which triggers the "High Volume" rollup rule.
			// BUT it also has an Unchanged file.
			// It should NOT rollup to StatusModified (collapsed), because that would hide the Unchanged file count context line.
			filesA: map[string]models.FileRecord{
				"/target/unchanged": {SHA1: "hash1", SizeBytes: 100},
				"/target/mod1":      {SHA1: "old1", SizeBytes: 100},
			},
			filesB: map[string]models.FileRecord{
				"/target/unchanged": {SHA1: "hash1", SizeBytes: 100},
				"/target/mod1":      {SHA1: "new1", SizeBytes: 100}, // Modified
				"/target/add1":      {SHA1: "add1", SizeBytes: 100}, // Added
				"/target/add2":      {SHA1: "add2", SizeBytes: 100}, // Added
				"/target/add3":      {SHA1: "add3", SizeBytes: 100}, // Added
			},
			// Total changes = 4 (> 2). Previous logic would force rollup.
			want: []DiffResult{
				// Expecting CHILDREN (Expanded) because Unchanged content exists.
				{Path: "/target/add1", Root: "/", RelPath: "target/add1", Status: StatusAdded, AddedCount: 1},
				{Path: "/target/add2", Root: "/", RelPath: "target/add2", Status: StatusAdded, AddedCount: 1},
				{Path: "/target/add3", Root: "/", RelPath: "target/add3", Status: StatusAdded, AddedCount: 1},
				{Path: "/target/mod1", Root: "/", RelPath: "target/mod1", Status: StatusModified, ModifiedCount: 1},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rootA := "/"
			rootB := "/"
			strategy := "sha1"

			got, err := CompareSnapshots(tt.filesA, tt.filesB, rootA, rootB, strategy, false, false, false, nil)
			if err != nil {
				t.Fatalf("CompareSnapshots() error = %v", err)
			}

			if len(got) != len(tt.want) {
				t.Errorf("Mismatch len. Got %d, Want %d", len(got), len(tt.want))
				for i, g := range got {
					t.Logf("Got [%d]: %s Status=%s Added=%d Copy=%d", i, g.Path, g.Status, g.AddedCount, g.CopyCount)
				}
				return
			}

			// Validate match
			for i := range got {
				if got[i].Path != tt.want[i].Path || got[i].Status != tt.want[i].Status {
					t.Errorf("Mismatch at %d. Got Path=%s Status=%s. Want Path=%s Status=%s", i, got[i].Path, got[i].Status, tt.want[i].Path, tt.want[i].Status)
				}
			}
		})
	}
}
