package util

import (
	"strings"
	"testing"
)

func TestGetMountPoints(t *testing.T) {
	mounts, err := GetMountPoints()
	if err != nil {
		t.Fatalf("GetMountPoints failed: %v", err)
	}

	if len(mounts) == 0 {
		t.Errorf("Expected at least one mount point, got 0")
	}

	foundRoot := false
	for _, m := range mounts {
		if m == "/" {
			foundRoot = true
		}
		if !strings.HasPrefix(m, "/") {
			t.Errorf("Mount point %s is not absolute", m)
		}
	}
	
	if !foundRoot {
		t.Logf("Warning: Root mount point '/' not found in list: %v", mounts)
	} else {
		t.Logf("Found %d mount points, including '/'", len(mounts))
	}
}
