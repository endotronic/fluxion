package diff

import (
	"fluxion/internal/models"
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

	got, err := CompareSnapshots(filesA, filesB, "/", "/", "sha1", false, false, true, nil)
	if err != nil {
		t.Fatalf("CompareSnapshots error: %v", err)
	}

	foundMixed := false
	for _, res := range got {
		if res.Status == StatusMixed {
			foundMixed = true
			if res.Path != "/root" && res.Path != "/root/" {
				t.Errorf("Expected mixed path based on context (/root or /root/), got %s", res.Path)
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
