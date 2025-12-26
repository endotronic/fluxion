package diff

import (
	"file-hasher/internal/models"
	"sort"
	"strings"
)

// Status represents the diff status of a file or directory
type Status string

const (
	StatusUnchanged Status = "Unchanged"
	StatusAdded     Status = "Added"
	StatusRemoved   Status = "Removed"
	StatusModified  Status = "Modified"
	StatusMixed     Status = "Mixed" // Only for directories containing varying children
)

// Node represents a file or directory in the diff tree
type Node struct {
	Name     string
	Path     string // Full path
	IsFile   bool
	Status   Status
	Children map[string]*Node

	// Metadata for comparison
	HashA string
	HashB string
}

// DiffResult represents a collapsed diff entry for display
type DiffResult struct {
	Path   string
	Status Status
}

// CompareSnapshots computes the diff between two sets of files.
func CompareSnapshots(filesA, filesB map[string]models.FileRecord, onProgress func(current, total int)) ([]DiffResult, error) {
	root := &Node{
		Name:     "",
		Path:     "",
		Children: make(map[string]*Node),
		Status:   StatusUnchanged, 
	}

	total := len(filesA) + len(filesB)
	current := 0

	// 1. Insert A (Files present in old snapshot) -> Mark as Removed (default)
	for path, record := range filesA {
		insertNode(root, path, record.SHA1, true)
		current++
		if onProgress != nil && current%1000 == 0 {
			onProgress(current, total)
		}
	}

	// 2. Insert B (Files present in new snapshot)
	for path, record := range filesB {
		insertNode(root, path, record.SHA1, false)
		current++
		if onProgress != nil && current%1000 == 0 {
			onProgress(current, total)
		}
	}

	if onProgress != nil {
		onProgress(total, total)
	}

	// 3. Propagate Status (Bottom-Up)
	propagateStatus(root)

	// 4. Collapse and Collect (Top-Down)
	var results []DiffResult
	collectResults(root, &results)

	// Sort results
	sort.Slice(results, func(i, j int) bool {
		return results[i].Path < results[j].Path
	})

	return results, nil
}

func insertNode(root *Node, path string, hash string, isA bool) {
	// normalize path separator
	// Assuming unix / for memory representation, but should split by os.PathSeparator in real usage?
	// The paths come from DB (created by filepath.Abs).
	// Let's assume standard separators for the platform are used in the string.
	// For splitting, we just use a simple splitter.
	
	// Remove volume name if any (Windows)? `filepath.SplitList`?
	// For simplicity, strict split on '/'. If Mac/Linux, this works.
	// Since user is on Mac, '/' is reliable.
	
	cleanPath := strings.TrimPrefix(path, "/")
	parts := strings.Split(cleanPath, "/")

	current := root
	for _, part := range parts {
		if part == "" {
			continue
		}
		if current.Children == nil {
			current.Children = make(map[string]*Node)
		}
		child, exists := current.Children[part]
		if !exists {
			child = &Node{
				Name:     part,
				Path:     "", // Will construct path? Or assume we only care at leaves?
				// Actually we need path for directories too for output.
				// Construction: current.Path + "/" + part
				Children: make(map[string]*Node),
				Status:   StatusUnchanged, // Default
			}
			if current.Path == "" {
				child.Path = "/" + part
			} else {
				if current.Path == "/" {
					child.Path = "/" + part
				} else {
					child.Path = current.Path + "/" + part
				}
			}
			current.Children[part] = child
		}
		current = child
	}

	// At leaf
	current.IsFile = true
	// Logic:
	// If isA (Old):
	//   Found in Old. Set HashA. Set Status = Removed. (Later if B visits, it checks HashA).
	// If !isA (New):
	//   Found in New. Set HashB.
	//   If HashA is set (means existed in A):
	//      Compare Hashes. Same -> Unchanged. Diff -> Modified.
	//   If HashA empty (didn't exist in A):
	//      Status -> Added.

	if isA {
		current.HashA = hash
		current.Status = StatusRemoved
	} else {
		current.HashB = hash
		if current.HashA != "" {
			// Existed in A
			if current.HashA == current.HashB {
				current.Status = StatusUnchanged
			} else {
				current.Status = StatusModified
			}
		} else {
			// New in B
			current.Status = StatusAdded
		}
	}
}

// propagateStatus updates directory statuses based on children.
// Returns the status of the node.
func propagateStatus(node *Node) Status {
	if node.IsFile {
		return node.Status
	}

	if len(node.Children) == 0 {
		// Empty directory?
		// If it existed in A but not B? -> Removed.
		// If B but not A? -> Added.
		// But our Insert logic only creates nodes for Files.
		// So purely empty directories might not be tracked explicitly unless we track Dirs in DB too.
		// (The tool currently only tracks Files).
		// So a "Directory" node only exists if it has files (current or past).
		// If it has no children in the Tree, it shouldn't really happen unless we inserted explicit Dir records (which we don't).
		// Wait, if all children are Removed, the Dir is Removed? Yes.
		return StatusUnchanged 
	}

	// Check children
	var firstStatus Status
	initialized := false
	isMixed := false

	for _, child := range node.Children {
		s := propagateStatus(child)
		if !initialized {
			firstStatus = s
			initialized = true
		} else {
			if s != firstStatus {
				isMixed = true
			}
		}
	}

	if isMixed {
		node.Status = StatusMixed
	} else {
		node.Status = firstStatus
	}

	return node.Status
}

// collectResults traverses logic to collapse nodes
func collectResults(node *Node, results *[]DiffResult) {
	// Skip root virtual node (unless root itself changed? e.g. everything removed?)
	// Root has no name/path usually in this implementation ("").
	// But its children are top level.
	
	if node.Name == "" { // Root
		for _, child := range node.Children {
			collectResults(child, results)
		}
		return
	}

	// Collapsing Logic
	if node.Status == StatusUnchanged {
		// Don't report unchanged
		return
	}

	if node.Status == StatusAdded || node.Status == StatusRemoved || node.Status == StatusModified {
		// Collapse: Report this node and stop recursion.
		// e.g. "Added /dir/" implies all children added.
		path := node.Path
		if !node.IsFile {
			path += "/"
		}
		*results = append(*results, DiffResult{
			Path:   path,
			Status: node.Status,
		})
		return
	}

	// If Mixed, recurse
	if node.Status == StatusMixed {
		for _, child := range node.Children {
			collectResults(child, results)
		}
	}
}
