package diff

import (
	"testing"
	"time"

	"fluxion/internal/models"

	"github.com/stretchr/testify/assert"
)

// TestDiffSortOrder verifies that the diff output is sorted alphabetically (A-Z)
// with unchanged items (directories) following their changes (Post-Order).
func TestDiffSortOrder(t *testing.T) {
	// Setup a hypothetical file tree
	// A:
	// root/ (Removed)
	// home/kevin/ztemp.sh (Removed)
	// home/kevin/workarea/ (Original)
	// home/mike/junk (Modified content)
	// docker/dropbox/ (Removed)
	// docker/historian/postgres/ (Removed)
	// docker/loki/ (Removed)

	// B:
	// home/kevin/work area/ (Renamed from workarea)
	// home/kevin/ (Unchanged files exist)
	// home/mike/junk (Modified content)

	// We'll simulate this by creating a tree manually or using CompareSnapshots with map inputs.
	// Using CompareSnapshots is better to test the full pipeline including collectResults.

	// filesA keys should be relative paths as CompareSnapshots expects relative keys (usually).
	// appHelper removes root prefix.
	filesA := map[string]models.FileRecord{
		"root/file1":                  {Filename: "file1", SizeBytes: 100, ModTime: time.Now(), SHA1: "h1"},
		"home/kevin/ztemp.sh":         {Filename: "ztemp.sh", SizeBytes: 10, ModTime: time.Now(), SHA1: "h2"},
		"home/kevin/workarea/file2":   {Filename: "file2", SizeBytes: 200, ModTime: time.Now(), SHA1: "h3"},
		"home/kevin/other.txt":        {Filename: "other.txt", SizeBytes: 50, ModTime: time.Now(), SHA1: "h_same"},
		"home/mike/junk":              {Filename: "junk", SizeBytes: 99, ModTime: time.Now(), SHA1: "h_old"},
		"docker/dropbox/d1":           {Filename: "d1", SizeBytes: 1, ModTime: time.Now(), SHA1: "h_drop"},
		"docker/historian/postgres/p": {Filename: "p", SizeBytes: 1, ModTime: time.Now(), SHA1: "h_pg"},
		"docker/historian/kept.txt":   {Filename: "kept.txt", SizeBytes: 1, ModTime: time.Now(), SHA1: "h_kept"},
		"docker/loki/l1":              {Filename: "l1", SizeBytes: 1, ModTime: time.Now(), SHA1: "h_loki"},
		"docker/readme.txt":           {Filename: "readme.txt", SizeBytes: 1, ModTime: time.Now(), SHA1: "h_keep"},
	}

	filesB := map[string]models.FileRecord{
		"home/kevin/work area/file2": {Filename: "file2", SizeBytes: 200, ModTime: time.Now(), SHA1: "h3"}, // Moved
		"home/kevin/other.txt":       {Filename: "other.txt", SizeBytes: 50, ModTime: time.Now(), SHA1: "h_same"},
		"home/mike/junk":             {Filename: "junk", SizeBytes: 99, ModTime: time.Now(), SHA1: "h_new"}, // Modified
		"docker/readme.txt":          {Filename: "readme.txt", SizeBytes: 1, ModTime: time.Now(), SHA1: "h_keep"},
		"docker/historian/kept.txt":  {Filename: "kept.txt", SizeBytes: 1, ModTime: time.Now(), SHA1: "h_kept"},
	}

	// Root paths
	rootA := "/mnt/zroot"
	rootB := "/mnt/zroot"

	// Expected Order logic:
	// A-Z Sibling Sort + Post-Order Traversal.
	//
	// Top Level Siblings: docker, home, root
	//
	// 1. docker (Mixed)
	//    - Children: dropbox(Removed), historian(Mixed), loki(Removed)
	//    - Sorted: dropbox, historian, loki
	//    - dropbox -> Emit
	//    - historian -> Recurse -> postgres(Removed) -> Emit -> Emit historian(Summary)
	//    - loki -> Emit
	//    - Emit docker(Summary)
	//
	// 2. home (Mixed)
	//    - Children: kevin(Mixed), mike(Mixed/Modified?)
	//    - filesB has home/mike/junk (Modified). So home/mike is distinct.
	//    - Mike has IsFile=false (implicit dir). StatusMixed containing Modified child?
	//      Actually diff logic might collapse if it's a dir with modified files?
	//      Let's see logic: "propagateStatus" uses "canRollup". Modified count > 0 -> StatusModified.
	//      So home/mike will be StatusModified (Collapsed).
	//    - Kevin: Has 'other.txt'(Unchanged), 'ztemp.sh'(Removed), 'workarea'(Moved).
	//      StatusMixed.
	//    - Sorted: kevin, mike.
	//    - kevin -> Recurse
	//      - Children: other.txt(Unchanged), workarea(Move), ztemp.sh(Removed).
	//        Note: 'other.txt' is Unchanged.
	//      - Sorted: other.txt, workarea, ztemp.sh. (Wait, file names? or keys?)
	//        Key in map is component name.
	//      - other.txt (Unchanged) -> Ignored (unless showUnchanged=true).
	//        Let's set showUnchanged=true to match user request "unchanged items following changes".
	//      - workarea (Move) -> Emit.
	//      - ztemp.sh (Removed) -> Emit.
	//      - Emit kevin(Summary) -> "home/kevin/"
	//    - mike (Modified) -> Emit.
	//    - Emit home(Summary) -> "home/"
	//
	// 3. root (Removed) -> Emit.
	//
	// Expected Result List (Paths):
	// docker/dropbox/
	// docker/historian/postgres/
	// docker/historian/
	// docker/loki/
	// docker/
	// home/kevin/workarea/ (Source or Dest? DiffResult.Path usually Dest for Move? No, RelPath)
	// home/kevin/ztemp.sh
	// home/kevin/
	// home/mike/
	// home/
	// root/

	results, err := CompareSnapshots(filesA, filesB, rootA, rootB, "sha1", false, false, true, nil)
	assert.NoError(t, err)

	var paths []string
	for _, res := range results {
		paths = append(paths, res.RelPath)
	}

	// We expect A-Z global if flat sorted currently.
	// Current implementation: Sorts by RelPath Descending (Z-A).
	// root/, home/, docker/...
	// This matches the "Actual" from the user prompt (roughly).

	// We want to verify the NEW behavior (Post-Order A-Z).
	// Since I haven't implemented it yet, this test will FAIL.
	// That is good.

	expected := []string{
		"docker/dropbox/",
		"docker/historian/postgres/",
		"docker/historian/",
		"docker/loki/",
		"docker/",
		"home/kevin/work area/", // Moved destination? CompareSnapshots return ID path. The user example showed: [>] ... workarea/ -> work area/
		// The DiffResult.RelPath is the "B" path for Moves usually (or A if Removed).
		// Code says: StatusMove -> RelPath = B path.
		"home/kevin/ztemp.sh",
		"home/kevin/",
		"home/mike/junk",
		"home/",
		"root/",
		".",
	}

	// Note: 'home/kevin/work area/' is "w" (space). 'home/kevin/ztemp.sh' is "z".
	// "work area" < "ztemp.sh". Correct.

	// Just verify the relative order of checking some key items to avoid exact string fragility if possible,
	// but exact list is better for sort verification.

	// For the sake of the test, let's just log what we got and assert equality.
	// We expect failure first.

	// Adjusting 'home/kevin/work area/' expectation:
	// In the map filesB, the path is ".../home/kevin/work area/file2".
	// The directory "work area" is the Moved node.
	// The RelPath for a directory move should be the directory path.

	// assert.Equal(t, expected, paths)
	assert.Equal(t, expected, paths)
}
