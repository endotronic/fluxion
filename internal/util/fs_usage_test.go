package util

import (
	"os"
	"testing"
)

func TestGetFSUsage(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get working dir: %v", err)
	}

	used, total, err := GetFSUsage(wd)
	if err != nil {
		t.Fatalf("GetFSUsage failed for %s: %v", wd, err)
	}

	if total == 0 {
		t.Errorf("Expected total bytes > 0, got 0")
	}

	// Used can theoretically be 0 on a fresh FS, but unlikely.
	// We just ensure no panic/error and meaningful Total.
	t.Logf("FS Usage for %s: Used %d, Total %d", wd, used, total)
}
