package app

import (
	"fmt"
	"time"

	"fluxion/internal/store"
)

func getUniqueSnapshotName(s store.Store, baseName string, exact bool) (string, error) {
	if exact {
		snap, _ := s.FindSnapshot(baseName)
		if snap != nil {
			return "", fmt.Errorf("snapshot with name '%s' already exists", baseName)
		}
		return baseName, nil
	}

	// Try baseName
	snap, _ := s.FindSnapshot(baseName)
	if snap == nil {
		return baseName, nil
	}

	// Try adding time suffix
	name := fmt.Sprintf("%s_%s", baseName, time.Now().Format("150405"))
	snap, _ = s.FindSnapshot(name)
	if snap == nil {
		return name, nil
	}

	// Try incrementing
	for i := 1; i < 100; i++ {
		name = fmt.Sprintf("%s_%d", baseName, i)
		snap, _ = s.FindSnapshot(name)
		if snap == nil {
			return name, nil
		}
	}

	return "", fmt.Errorf("could not generate unique name for %s", baseName)
}

