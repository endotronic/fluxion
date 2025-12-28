package diff

import (
	"fluxion/internal/models"
	"path/filepath"
	"sort"
	"strings"
)

// Status represents the diff status of a file or directory
type Status string

const (
	StatusUnchanged   Status = "Unchanged"
	StatusAdded       Status = "Added"
	StatusRemoved     Status = "Removed"
	StatusModified    Status = "Modified"
	StatusMixed       Status = "Mixed" // Only for directories containing varying children
	StatusMove        Status = "Move"
	StatusCopy        Status = "Copy"
	StatusMovedSource Status = "MovedSource" // Internal: Source of a move, should be hidden
)

// Node represents a file or directory in the diff tree
type Node struct {
	Name     string
	Path     string // Full path
	IsFile   bool
	Status   Status
	Children map[string]*Node

	// Metadata for comparison (Generic, set based on strategy)
	HashA string
	HashB string

	// Merkle Hash (computed for dirs) -> reusing HashA/HashB for dirs?
	// Yes, HashA for Dir is its Merkle Hash in A.

	// For Move/Copy
	SourcePath string
}

// DiffResult represents a collapsed diff entry for display
type DiffResult struct {
	Path       string
	Status     Status
	SourcePath string
	FileCount  int64
}

// CompareSnapshots computes the diff between two sets of files using a specific hash strategy.
func CompareSnapshots(filesA, filesB map[string]models.FileRecord, rootA, rootB string, hashType string, noCopies, noMoves bool, onProgress func(current, total int)) ([]DiffResult, error) {
	root := &Node{
		Name:     "",
		Path:     "",
		Children: make(map[string]*Node),
		Status:   StatusUnchanged,
	}

	total := len(filesA) + len(filesB)
	current := 0

	// 1. Insert A
	for path, record := range filesA {
		insertNode(root, path, record, true, hashType)
		current++
		if onProgress != nil && current%1000 == 0 {
			onProgress(current, total)
		}
	}

	// 2. Insert B
	for path, record := range filesB {
		insertNode(root, path, record, false, hashType)
		current++
		if onProgress != nil && current%1000 == 0 {
			onProgress(current, total)
		}
	}

	if onProgress != nil {
		onProgress(total, total)
	}

	// 3. Compute Merkle Hashes (Post-Order)
	computeMerkleHashes(root)

	// 4. Propagate Status (Pass 1)
	propagateStatus(root)

	// 5. Detect Moves/Copies
	detectMovesCopies(root, noCopies, noMoves)

	// 6. Propagate Status (Pass 2)
	propagateStatus(root)

	// 7. Collapse and Collect
	var results []DiffResult
	collectResults(root, &results)

	// 8. Reconstruct Absolute Paths
	finalResults := make([]DiffResult, len(results))
	for i, res := range results {
		// Clean paths (remove leading slash if present from Node path construction)
		relPath := strings.TrimPrefix(res.Path, "/")

		var absPath string
		var absSource string

		switch res.Status {
		case StatusAdded, StatusModified, StatusMove, StatusCopy:
			absPath = filepath.Join(rootB, relPath)
			if strings.HasSuffix(res.Path, "/") {
				absPath += string(filepath.Separator)
			}
		case StatusRemoved:
			absPath = filepath.Join(rootA, relPath)
			if strings.HasSuffix(res.Path, "/") {
				absPath += string(filepath.Separator)
			}
		default:
			absPath = filepath.Join(rootB, relPath)
		}

		if res.SourcePath != "" {
			relSource := strings.TrimPrefix(res.SourcePath, "/")
			absSource = filepath.Join(rootA, relSource)
			if strings.HasSuffix(res.SourcePath, "/") {
				absSource += string(filepath.Separator)
			}
		}

		finalResults[i] = DiffResult{
			Path:       absPath,
			Status:     res.Status,
			SourcePath: absSource,
			FileCount:  res.FileCount,
		}
	}

	// Sort results
	sort.Slice(finalResults, func(i, j int) bool {
		return finalResults[i].Path < finalResults[j].Path
	})

	return finalResults, nil
}

func insertNode(root *Node, path string, record models.FileRecord, isA bool, hashType string) {
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
				Path:     "",
				Children: make(map[string]*Node),
				Status:   StatusUnchanged,
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

	// Extract correct hash
	hash := ""
	if hashType == "sha1" {
		hash = record.SHA1
	} else if hashType == "md5" {
		hash = record.MD5
	}
	// Fallback? If requested hash missing, use empty string (treated as changed/missing)

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

	// If this node was explicitly detected as Move/Copy (has SourcePath), preserve that status
	// instead of recalculating from children.
	if node.SourcePath != "" {
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
		// If all children are MovedSource, this node is also MovedSource (suppressed)
		if firstStatus == StatusMovedSource {
			node.Status = StatusMovedSource
		} else if firstStatus == StatusMove || firstStatus == StatusCopy {
			// Cannot inherit Move/Copy implicitly. Must be Mixed to show specific moved/copied children.
			node.Status = StatusMixed
		} else {
			node.Status = firstStatus
		}
	}

	return node.Status
}

// computeMerkleHashes computes Merkle hash of directory content
// Now uses generic HashA/HashB fields.
func computeMerkleHashes(node *Node) {
	if node.IsFile {
		return
	}

	// Directory
	if len(node.Children) == 0 {
		return
	}

	var hashesA []string
	var hashesB []string

	for _, child := range node.Children {
		computeMerkleHashes(child)

		if child.HashA != "" {
			hashesA = append(hashesA, child.Name+":"+child.HashA)
		}
		if child.HashB != "" {
			hashesB = append(hashesB, child.Name+":"+child.HashB)
		}
	}

	if len(hashesA) > 0 {
		sort.Strings(hashesA)
		node.HashA = strings.Join(hashesA, ",")
	}

	if len(hashesB) > 0 {
		sort.Strings(hashesB)
		node.HashB = strings.Join(hashesB, ",")
	}
}

func detectMovesCopies(root *Node, noCopies, noMoves bool) {
	if noCopies && noMoves {
		return
	}
	// 1. Index Removed and Existing nodes
	removedMap := make(map[string][]string)  // Hash -> [Paths]
	existingMap := make(map[string][]string) // Hash -> [Paths] (Only from A)

	var index func(*Node)
	index = func(n *Node) {
		if n.HashA == "" {
			return
		}

		// Helper to index a hash
		add := func(m map[string][]string, hash, path string) {
			if hash != "" {
				m[hash] = append(m[hash], path)
			}
		}

		// If removed (or Modified: Treat old content as removed), add to removedMap
		if n.Status == StatusRemoved {
			add(removedMap, n.HashA, n.Path)
		} else if n.Status == StatusModified {
			add(removedMap, n.HashA, n.Path)
		}

		// If existed in A (Removed OR Modified OR Unchanged OR Mixed), add to ExistingMap
		if n.Status != StatusAdded {
			add(existingMap, n.HashA, n.Path)
		}

		for _, child := range n.Children {
			index(child)
		}
	}
	index(root)

	// 2. Scan for Added/Modified nodes and match
	var match func(*Node)
	match = func(n *Node) {
		if n.Status == StatusAdded || n.Status == StatusModified {
			// Use HashB
			hash := n.HashB
			if hash == "" {
				// Cannot match if no hash
				return
			}

			matched := false

			// Check Removed (Move) first
			if !noMoves {
				if paths, ok := removedMap[hash]; ok && len(paths) > 0 {
					// Match found!
					src := paths[0]
					n.Status = StatusMove
					if !n.IsFile {
						if !strings.HasSuffix(src, "/") {
							src += "/"
						}
					}
					n.SourcePath = src

					// Mark Source as MovedSource
					srcNode := findNode(root, paths[0])
					if srcNode != nil && srcNode.Status == StatusRemoved {
						srcNode.Status = StatusMovedSource
					}

					// Consume
					removedMap[hash] = paths[1:]
					matched = true
				}
			}

			if !matched && !noCopies {
				if paths, ok := existingMap[hash]; ok && len(paths) > 0 {
					// Copy
					src := paths[0]
					n.Status = StatusCopy
					if !n.IsFile {
						if !strings.HasSuffix(src, "/") {
							src += "/"
						}
					}
					n.SourcePath = src
				}
			}
		}

		// Deterministic iteration for children
		var children []*Node
		for _, child := range n.Children {
			children = append(children, child)
		}
		sort.Slice(children, func(i, j int) bool {
			return children[i].Name < children[j].Name
		})

		for _, child := range children {
			match(child)
		}
	}
	match(root)
}

// Helper to find node by path
func findNode(root *Node, path string) *Node {
	cleanPath := strings.TrimPrefix(path, "/")
	parts := strings.Split(cleanPath, "/")

	current := root
	for _, part := range parts {
		if part == "" {
			continue
		}
		if current.Children == nil {
			return nil
		}
		child, ok := current.Children[part]
		if !ok {
			return nil
		}
		current = child
	}
	return current
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
	if node.Status == StatusUnchanged || node.Status == StatusMovedSource {
		// Don't report unchanged or moved sources
		return
	}

	if node.Status == StatusAdded || node.Status == StatusRemoved || node.Status == StatusModified || node.Status == StatusMove || node.Status == StatusCopy {
		// Collapse: Report this node and stop recursion.
		// e.g. "Added /dir/" implies all children added.
		path := node.Path
		files := int64(0)

		if !node.IsFile {
			path += "/"
			// Count recursive files
			files = countSubtreeFiles(node)
		}

		*results = append(*results, DiffResult{
			Path:       path,
			Status:     node.Status,
			SourcePath: node.SourcePath,
			FileCount:  files,
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

func countSubtreeFiles(node *Node) int64 {
	if node.IsFile {
		return 1
	}
	var sum int64 = 0
	for _, child := range node.Children {
		sum += countSubtreeFiles(child)
	}
	return sum
}
