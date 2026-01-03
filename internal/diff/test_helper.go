package diff

import (
	"fluxion/internal/models"
	"sort"
)

// Helper to convert map to iterator for testing
func mapToIter(m map[string]models.FileRecord) FileIterator {
	return func(yield func(string, models.FileRecord) error) error {
		// Iterate in sorted order for determinism in testing
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys) // Not strictly necessary for correct diff but nice for consistency

		for _, k := range keys {
			if err := yield(k, m[k]); err != nil {
				return err
			}
		}
		return nil
	}
}
