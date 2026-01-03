package diff

import (
	"fluxion/internal/models"
	"sort"
	"testing"
)

func TestCompareSnapshots_ShowUnchanged(t *testing.T) {
	filesA := map[string]models.FileRecord{
		"/root/svc1/file1": {SHA1: "h1"},
		"/root/svc2/file2": {SHA1: "h2"},
		"/root/common/f3":  {SHA1: "h3"},
		"/root/common/f4":  {SHA1: "h4"},
	}
	filesB := map[string]models.FileRecord{
		// svc1 removed
		// svc2 removed
		// common unchanged
		"/root/common/f3": {SHA1: "h3"},
		"/root/common/f4": {SHA1: "h4"},
	}

	// Expect:
	// - svc1 Removed
	// - svc2 Removed
	// - root/ Mixed (Context) with UnchangedDirCount=1 (common), UnchangedFileCount=2 (f3, f4 recursive)

	got, err := CompareSnapshots(mapToIter(filesA), mapToIter(filesB), "/", "/", "sha1", false, false, true, nil)
	if err != nil {
		t.Fatalf("CompareSnapshots error: %v", err)
	}

	foundMixed := false
	for _, res := range got {
		if res.Status == StatusMixed {
			foundMixed = true
			if res.Path != "/root" && res.Path != "/root/" && res.Path != "/" {
				t.Errorf("Expected mixed path based on context (/root or /root/ or /), got %s", res.Path)
			}
			// Unchanged stats?
			// common is unchanged dir. It contains 2 files.
			// accumulateStats should return 1 UnchangedDir and 2 UnchangedFiles?
			// Let's check logic.
			// accumulateStats on Node(common): Unchanged.
			// logic:
			// if node.Status == Unchanged { total.UnchangedDirCount = 1 }
			// recurses children (f3, f4).
			// f3: UnchangedFileCount=1
			// f4: UnchangedFileCount=1
			// Total: Dir=1, File=2. Correct.

			if res.UnchangedDirCount != 1 {
				t.Errorf("Expected 1 unchanged dir, got %d", res.UnchangedDirCount)
			}
			if res.UnchangedFileCount != 2 {
				t.Errorf("Expected 2 unchanged files, got %d", res.UnchangedFileCount)
			}
		}
	}

	if !foundMixed {
		t.Error("Expected to find StatusMixed result for /root/")
	}
}

// TestReproduction_Rollup_And_ShowUnchanged aims to reproduce user reports:
// 1. Rollup broken (Modified dir with no unchanged files should rollup)
// 2. Show Unchanged missing items (Modified dir with some unchanged files should show context)
func TestReproduction_Rollup_And_ShowUnchanged(t *testing.T) {
	tests := []struct {
		name          string
		filesA        map[string]models.FileRecord
		filesB        map[string]models.FileRecord
		showUnchanged bool
		want          []DiffResult
	}{
		{
			// Invariant 2: If no items match, it should rollup (IF High Volume).
			// Directory "rollup/" contains file1 (modified). No other files.
			// New Logic: Low volume (1 change) -> Expanded.
			name: "Invariant_2_Rollup_Pure_Modified",
			filesA: map[string]models.FileRecord{
				"/rollup/file1": {SHA1: "hash1"},
			},
			filesB: map[string]models.FileRecord{
				"/rollup/file1": {SHA1: "hash2"}, // Modified
			},
			showUnchanged: false,
			want: []DiffResult{
				{
					Path:          "/rollup/file1",
					Status:        StatusModified,
					ModifiedCount: 1,
					RelPath:       "rollup/file1",
				},
			},
		},
		{
			// Invariant 1: If there is a change, report containing directory context.
			// Directory "mixed/" contains file1 (unchanged) and file2 (modified).
			// showUnchanged = true.
			// Should result in:
			// - mixed/ (StatusMixed, Unchanged counts)
			// - mixed/file2 (StatusModified)
			// file1 is NOT listed individually, but counted.
			name: "Invariant_1_ShowUnchanged_Mixed_Context",
			filesA: map[string]models.FileRecord{
				"/mixed/file1": {SHA1: "static"},
				"/mixed/file2": {SHA1: "hashA"},
			},
			filesB: map[string]models.FileRecord{
				"/mixed/file1": {SHA1: "static"}, // Unchanged
				"/mixed/file2": {SHA1: "hashB"},  // Modified
			},
			showUnchanged: true,
			want: []DiffResult{
				{
					Path:               "/mixed/",
					Status:             StatusMixed,
					RelPath:            "mixed/",
					UnchangedFileCount: 1, // file1
					UnchangedDirCount:  0,
				},
				{
					Path:          "/mixed/file2",
					Status:        StatusModified,
					RelPath:       "mixed/file2",
					ModifiedCount: 1,
				},
				{
					Path:               "/",
					Root:               "/",
					Status:             StatusMixed,
					RelPath:            ".",
					UnchangedFileCount: 1,
				},
			},
		},
		{
			// Invariant 2 (Extended): If no files match (Empty Intersection), rollup (IF High Volume).
			// Directory "swap/" has "a" removed and "b" added.
			// New Logic: 2 Changes >= 2. Rollup.
			name: "Invariant_2_Rollup_Add_Remove_Only",
			filesA: map[string]models.FileRecord{
				"/swap/a": {SHA1: "hashA"},
			},
			filesB: map[string]models.FileRecord{
				"/swap/b": {SHA1: "hashB"},
			},
			showUnchanged: false,
			want: []DiffResult{
				{
					Path:         "/swap/",
					Status:       StatusModified, // Rolled up!
					RelPath:      "swap/",
					RemovedCount: 1,
					AddedCount:   1,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CompareSnapshots(mapToIter(tt.filesA), mapToIter(tt.filesB), "/", "/", "sha1", false, false, tt.showUnchanged, nil)
			if err != nil {
				t.Fatalf("CompareSnapshots error: %v", err)
			}

			// Sort expectation dynamically (RelPath Descending)
			sortedWant := make([]DiffResult, len(tt.want))
			copy(sortedWant, tt.want)
			sort.Slice(sortedWant, func(i, j int) bool {
				return sortedWant[i].RelPath < sortedWant[j].RelPath
			})

			// Sort got similarly to ensure order-independent verification
			sort.Slice(got, func(i, j int) bool {
				return got[i].RelPath < got[j].RelPath
			})

			if len(got) != len(sortedWant) {
				t.Errorf("Length mismatch. Got %d, Want %d\nGot: %+v\nWant: %+v", len(got), len(sortedWant), got, sortedWant)
				return
			}

			for i := range got {
				w := sortedWant[i]
				g := got[i]

				if g.Path != w.Path || g.Status != w.Status ||
					g.ModifiedCount != w.ModifiedCount ||
					(tt.showUnchanged && g.UnchangedFileCount != w.UnchangedFileCount) {
					t.Errorf("Mismatch at index %d:\nGot:  %+v\nWant: %+v", i, g, w)
				}
			}
		})
	}
}
func TestReproduction_Large_Mixed_Directory(t *testing.T) {
	// User Scenario:
	// /docker/ contains:
	// - Unchanged file (preventing full rollup)
	// - 5 Removed directories (High change count)
	// Current Bug: Collapses to [M] /docker/ (StatusModified)
	// Expected: StatusMixed (Shows children)

	filesA := map[string]models.FileRecord{
		"/docker/unchanged": {SHA1: "h1"},
		"/docker/r1/f":      {SHA1: "a"},
		"/docker/r2/f":      {SHA1: "b"},
		"/docker/r3/f":      {SHA1: "c"},
		"/docker/r4/f":      {SHA1: "d"},
		"/docker/r5/f":      {SHA1: "e"},
	}
	filesB := map[string]models.FileRecord{
		"/docker/unchanged": {SHA1: "h1"},
		// r1-r5 removed
	}

	got, err := CompareSnapshots(mapToIter(filesA), mapToIter(filesB), "/", "/", "sha1", false, false, false, nil)
	if err != nil {
		t.Fatalf("CompareSnapshots error: %v", err)
	}

	// We expect multiple results:
	// - /docker/r1/ (Removed)
	// ...
	// - /docker/r5/ (Removed)
	// Because /docker/ (relPath "docker/") should be StatusMixed and recurse.
	// If it is StatusModified, we will only see ONE result: /docker/

	// However, note that "unchanged" is not shown (showUnchanged=false).

	foundDockerRoot := false
	foundRemoved := 0

	for _, res := range got {
		if res.RelPath == "docker/" {
			foundDockerRoot = true
			if res.Status == StatusModified {
				t.Errorf("FAIL: /docker/ was collapsed to StatusModified. Expected it to be StatusMixed (hidden from results, but children shown).")
			}
		}
		if res.Status == StatusRemoved {
			foundRemoved++
		}
	}

	if foundRemoved < 5 {
		t.Errorf("Expected at least 5 removed items (r1..r5), got %d. Result dump: %+v", foundRemoved, got)
	}

	if foundDockerRoot && foundRemoved == 0 {
		t.Error("Critical: Saw /docker/ root but no children. Over-aggressive rollup occurred.")
	}
}
