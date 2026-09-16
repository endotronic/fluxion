package app

import "testing"

// The table from knowledge/known-issues.md issue 1.3, which was confirmed by
// reproduction rather than inferred. Each of these once made a diff look clean
// while content was being silently dropped from it.
func TestIsExcluded_MatchesOnlyAtPathBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		root     string
		excludes []string
		want     bool
	}{
		{"the excluded directory itself", "/project/data", "/project", []string{"data"}, true},
		{"a file inside it", "/project/data/file", "/project", []string{"data"}, true},
		{"a sibling sharing the prefix", "/project/data2/file", "/project", []string{"data"}, false},
		{"a longer sibling name", "/project/database/file", "/project", []string{"data"}, false},
		{"a backup of an excluded file", "/project/secrets.txt.bak", "/project", []string{"secrets.txt"}, false},
		{"the excluded file itself", "/project/secrets.txt", "/project", []string{"secrets.txt"}, true},
		{"a numbered sibling of a backup dir", "/project/backup2/x", "/project", []string{"backup"}, false},

		{"absolute exclude, sibling sharing the prefix", "/mnt/data2/file", "", []string{"/mnt/data"}, false},
		{"absolute exclude, genuinely underneath", "/mnt/data/file", "", []string{"/mnt/data"}, true},

		{"relative path input, sibling sharing the prefix", "data2/file", "", []string{"data"}, false},
		{"relative path input, genuinely underneath", "data/file", "", []string{"data"}, true},

		// A trailing separator is how a shell tab-completion writes a directory,
		// and it used to make the exclude match nothing at all.
		{"exclude written with a trailing slash", "/project/data/file", "/project", []string{"data/"}, true},
		{"exclude written as ./name", "/project/data/file", "/project", []string{"./data"}, true},

		// Excluding everything by accident is the same severity-1 direction as
		// over-excluding, so an empty exclude must match nothing.
		{"empty exclude matches nothing", "/project/anything", "/project", []string{""}, false},

		{"one of several excludes matches", "/project/data/f", "/project", []string{"other", "data"}, true},
		{"no exclude matches", "/project/keep/f", "/project", []string{"other", "data"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isExcluded(tt.path, tt.root, tt.excludes); got != tt.want {
				t.Errorf("isExcluded(%q, %q, %v) = %v, want %v",
					tt.path, tt.root, tt.excludes, got, tt.want)
			}
		})
	}
}

func TestPathHasPrefix(t *testing.T) {
	tests := []struct {
		path, prefix string
		want         bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b/c", "/a/b", true},
		{"/a/bc", "/a/b", false},
		{"/a/b.txt", "/a/b", false},
		{"/a", "/a/b", false},
		{"/anything", "/", true}, // the root's boundary is part of the prefix
		{"/", "/", true},
		{"a/b", "a", true},
		{"ab", "a", false},
		{"/a/b", "", false},
		{"", "/a", false},
		{"/a/b/c", "/a/b/", true},  // cleaned before comparing
		{"/a/b/c", "/a/./b", true}, // ditto
	}

	for _, tt := range tests {
		if got := pathHasPrefix(tt.path, tt.prefix); got != tt.want {
			t.Errorf("pathHasPrefix(%q, %q) = %v, want %v", tt.path, tt.prefix, got, tt.want)
		}
	}
}

func TestRootsDisjoint(t *testing.T) {
	tests := []struct {
		name  string
		roots []string
		want  bool
	}{
		{"independent zfs-scan datasets", []string{"luna/kevin/archives/2016-2020", "luna/mike/archives", "luna/witness/scribe-minio"}, true},
		{"pre-issue-2.8 scan mounts, always distinct temp dirs", []string{"/tmp/fluxion-zfsscan-1", "/tmp/fluxion-zfsscan-2"}, true},
		{"identical roots", []string{"/tmp", "/tmp"}, false},
		{"one root nested under another", []string{"/luna/kevin", "/luna/kevin/archives"}, false},
		{"a sibling that only shares a text prefix is still disjoint", []string{"/luna/kevin", "/luna/kevin2"}, true},
		{"single root is trivially disjoint", []string{"/a"}, true},
		{"empty list is trivially disjoint", nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rootsDisjoint(tt.roots); got != tt.want {
				t.Errorf("rootsDisjoint(%v) = %v, want %v", tt.roots, got, tt.want)
			}
		})
	}
}
