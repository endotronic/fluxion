package diff

import (
	"fluxion/internal/models"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestZeroByteMoveCopy(t *testing.T) {
	// Scenario 1: Zero-byte file "moved" (Removed from A, Added in B, same hash).
	// Should NOT be detected as Move.

	filesA := map[string]models.FileRecord{
		"root/zero_A": {Filename: "zero_A", SizeBytes: 0, ModTime: time.Now(), SHA1: "da39a3ee5e6b4b0d3255bfef95601890afd80709"}, // Empty SHA1
	}

	filesB := map[string]models.FileRecord{
		"root/zero_B": {Filename: "zero_B", SizeBytes: 0, ModTime: time.Now(), SHA1: "da39a3ee5e6b4b0d3255bfef95601890afd80709"},
	}

	results, err := CompareSnapshots(filesA, filesB, "/root", "/root", "sha1", false, false, false, nil)
	assert.NoError(t, err)

	// Expect:
	// Removed: zero_A
	// Added: zero_B
	// NO Move.

	hasMove := false
	hasRemoved := false
	hasAdded := false

	for _, res := range results {
		t.Logf("Result: Path=%s, RelPath=%s, Status=%s", res.Path, res.RelPath, res.Status)
		// If collapsed, check counts
		if res.Status == StatusModified {
			if res.MoveCount > 0 {
				hasMove = true
			}
			if res.RemovedCount == 1 {
				hasRemoved = true
			}
			if res.AddedCount == 1 {
				hasAdded = true
			}
		} else {
			// Fallback for non-collapsed (if logic changes)
			if res.Status == StatusMove {
				hasMove = true
			}
			if res.Status == StatusRemoved && res.RelPath == "root/zero_A" {
				hasRemoved = true
			}
			if res.Status == StatusAdded && res.RelPath == "root/zero_B" {
				hasAdded = true
			}
		}
	}

	assert.False(t, hasMove, "Zero byte files should not be detected as moves")
	assert.True(t, hasRemoved, "Zero byte file A should be removed")
	assert.True(t, hasAdded, "Zero byte file B should be added")

	// Scenario 2: Zero-byte file "copied" (Exists in A, Added in B, same hash).
	// Should NOT be detected as Copy.

	filesA2 := map[string]models.FileRecord{
		"root/zero_A": {Filename: "zero_A", SizeBytes: 0, ModTime: time.Now(), SHA1: "da39a3ee5e6b4b0d3255bfef95601890afd80709"},
	}

	filesB2 := map[string]models.FileRecord{
		"root/zero_A": {Filename: "zero_A", SizeBytes: 0, ModTime: time.Now(), SHA1: "da39a3ee5e6b4b0d3255bfef95601890afd80709"},
		"root/zero_B": {Filename: "zero_B", SizeBytes: 0, ModTime: time.Now(), SHA1: "da39a3ee5e6b4b0d3255bfef95601890afd80709"},
	}

	results2, err := CompareSnapshots(filesA2, filesB2, "/root", "/root", "sha1", false, false, false, nil)
	assert.NoError(t, err)

	hasCopy := false
	for _, res := range results2 {
		if res.Status == StatusCopy {
			hasCopy = true
		}
	}
	assert.False(t, hasCopy, "Zero byte files should not be detected as copies")
}
