package app

import (
	"path/filepath"
	"testing"
)

func TestIsExcluded(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		root     string
		excludes []string
		want     bool
	}{
		{
			name:     "No excludes",
			path:     "/foo/bar",
			root:     "/foo",
			excludes: nil,
			want:     false,
		},
		{
			name:     "Absolute exclude match",
			path:     "/foo/bar/file",
			root:     "/foo",
			excludes: []string{"/foo/bar"},
			want:     true,
		},
		{
			name:     "Absolute exclude mismatch",
			path:     "/foo/baz/file",
			root:     "/foo",
			excludes: []string{"/foo/bar"},
			want:     false,
		},
		{
			name:     "Relative exclude match (simple)",
			path:     "node_modules/pkg",
			root:     "", // no root context
			excludes: []string{"node_modules"},
			want:     true,
		},
		{
			name:     "Relative exclude match (with root)",
			path:     "/project/node_modules/pkg",
			root:     "/project",
			excludes: []string{"node_modules"},
			want:     true,
		},
		{
			name: "Relative exclude mismatch (nested deep but not anchored?)",
			// implementation: if root is present, we check root+exclude
			// and we also check if path starts with exclude (which might fail if path is absolute)
			path:     "/project/src/node_modules/pkg",
			root:     "/project",
			excludes: []string{"node_modules"},
			// /project/src/node_modules/pkg does NOT have prefix /project/node_modules
			// and it does NOT have prefix node_modules
			want: false,
		},
		{
			name:     "Specific file exclude",
			path:     "/project/secret.txt",
			root:     "/project",
			excludes: []string{"secret.txt"},
			want:     true,
		},
		// Case where path is relative (e.g. from filepath.Rel)
		{
			name:     "Relative path input match",
			path:     "build/output",
			root:     "",
			excludes: []string{"build"},
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Ensure paths are clean for the test logic to work as expected
			// The implementation also calls Clean.
			got := isExcluded(tt.path, filepath.Clean(tt.root), tt.excludes)
			if got != tt.want {
				t.Errorf("isExcluded(%q, %q, %v) = %v, want %v", tt.path, tt.root, tt.excludes, got, tt.want)
			}
		})
	}
}
