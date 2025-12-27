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
	StatusUnchanged Status = "Unchanged"
	StatusAdded     Status = "Added"
	StatusRemoved   Status = "Removed"
	StatusModified  Status = "Modified"
	StatusMixed     Status = "Mixed" // Only for directories containing varying children
	StatusMove      Status = "Move"
	StatusCopy      Status = "Copy"
	StatusMovedSource Status = "MovedSource" // Internal: Source of a move, should be hidden
)

// Node represents a file or directory in the diff tree
type Node struct {
	Name     string
	Path     string // Full path
	IsFile   bool
	Status   Status
	Children map[string]*Node

	// Metadata for comparison
	SHA1A string
	MD5A  string
	SHA1B string
	MD5B  string
	
	// Merkle Hash (computed for dirs)
	MerkleHash string
	
	// For Move/Copy
	SourcePath string
}

// DiffResult represents a collapsed diff entry for display
type DiffResult struct {
	Path       string
	Status     Status
	SourcePath string
}

// CompareSnapshots computes the diff between two sets of files.
// filesA and filesB keys must be relative paths to their respective roots.
func CompareSnapshots(filesA, filesB map[string]models.FileRecord, rootA, rootB string, onProgress func(current, total int)) ([]DiffResult, error) {
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
		insertNode(root, path, record, true)
		current++
		if onProgress != nil && current%1000 == 0 {
			onProgress(current, total)
		}
	}

	// 2. Insert B (Files present in new snapshot)
	for path, record := range filesB {
		insertNode(root, path, record, false)
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

	// 4. Propagate Status (Pass 1 - Identify Added/Removed Dirs)
	propagateStatus(root)

	// 5. Detect Moves/Copies
	detectMovesCopies(root)

	// 6. Propagate Status (Pass 2 - Handle Suppression)
	propagateStatus(root)

	// 7. Collapse and Collect (Top-Down)
	var results []DiffResult
	collectResults(root, &results)
	
	// 8. Reconstruct Absolute Paths for Result
	// We want to present absolute paths to the user.
	// Logic:
	// - Added: rootB + Path
	// - Removed: rootA + Path
	// - Modified: rootB + Path
	// - Move/Copy: SourcePath (rootA) -> Path (rootB)
	
	finalResults := make([]DiffResult, len(results))
	for i, res := range results {
		// Clean paths (remove leading slash if present from Node path construction)
		relPath := strings.TrimPrefix(res.Path, "/")
		
		var absPath string
		var absSource string
		
		switch res.Status {
		case StatusAdded, StatusModified, StatusMove, StatusCopy:
			absPath = filepath.Join(rootB, relPath)
			// Ensure trailing slash for dirs if original had it?
			// filepath.Join removes trailing slash.
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
			Path: absPath,
			Status: res.Status,
			SourcePath: absSource,
		}
	}

	// Sort results
	sort.Slice(finalResults, func(i, j int) bool {
		return finalResults[i].Path < finalResults[j].Path
	})

	return finalResults, nil
}

func insertNode(root *Node, path string, record models.FileRecord, isA bool) {
	// Parse path (assuming unix-style from DB/filepath)
	
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
		current.SHA1A = record.SHA1
		current.MD5A = record.MD5
		current.Status = StatusRemoved
	} else {
		current.SHA1B = record.SHA1
		current.MD5B = record.MD5
		
		if current.SHA1A != "" || current.MD5A != "" {
			// Existed in A
			match := false
			if current.SHA1A != "" && current.SHA1B != "" {
				match = (current.SHA1A == current.SHA1B)
			} else if current.MD5A != "" && current.MD5B != "" {
				match = (current.MD5A == current.MD5B)
			}
			// If neither pair present, we assume Modified (couldn't verify equality)
			
			if match {
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


// computeMerkleHashes computes SHA1 of directory content (sorted children hashes)
func computeMerkleHashes(node *Node) {
	if node.IsFile {
		// Use SHA1 if available, else MD5.
		// Prioritize SHA1 for stability.
		
		hash := ""
		if node.SHA1B != "" { hash = node.SHA1B }
		if hash == "" && node.MD5B != "" { hash = node.MD5B }
		if hash == "" && node.SHA1A != "" { hash = node.SHA1A }
		if hash == "" && node.MD5A != "" { hash = node.MD5A }
		
		node.MerkleHash = hash
		return
	}

	// Directory
	if len(node.Children) == 0 {
		return
	}

	var hashes []string
	for _, child := range node.Children {
		computeMerkleHashes(child)
		if child.MerkleHash != "" {
			hashes = append(hashes, child.Name+":"+child.MerkleHash) // Include name to avoid struct collision? Or just content?
			// User said: "If multiple copies happened...".
			// Directory hash should rely on content.
			// Ideally: SHA1(sort(child_merkle_hashes)).
			// Using name: specific to path structure? No, user said "collapse... moved together".
			// If I rename a directory, the content (children) names match, but parent name differs.
			// So inclusion of child name IS correct for directory Merkle.
		}
	}
	sort.Strings(hashes)
	
	// Create hash of strings by joining sorted child hashes.
	// Simple string concatenation is sufficient for our internal map keys.
	
	node.MerkleHash = strings.Join(hashes, ",")
}

func detectMovesCopies(root *Node) {
	// 1. Index Removed and Existing nodes
	removedMap := make(map[string][]string) // Hash -> [Paths]
	existingMap := make(map[string][]string) // Hash -> [Paths] (Only from A)

	var index func(*Node)
	index = func(n *Node) {
		if n.MerkleHash == "" && n.SHA1A == "" && n.MD5A == "" {
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
			if n.IsFile {
				add(removedMap, n.SHA1A, n.Path)
				add(removedMap, n.MD5A, n.Path)
			} else {
				add(removedMap, n.MerkleHash, n.Path)
			}
		} else if n.Status == StatusModified && n.IsFile {
			add(removedMap, n.SHA1A, n.Path)
			add(removedMap, n.MD5A, n.Path)
		}
		
		// If existed in A (Removed OR Modified OR Unchanged OR Mixed), add to ExistingMap
		// (Used for Copy detection)
		if n.Status != StatusAdded {
			if n.IsFile {
				add(existingMap, n.SHA1A, n.Path)
				add(existingMap, n.MD5A, n.Path)
			} else {
				add(existingMap, n.MerkleHash, n.Path)
			}
		}
		
		for _, child := range n.Children {
			index(child)
		}
	}
	index(root)
	
	// 2. Scan for Added/Modified nodes and match
	var match func(*Node)
	match = func(n *Node) {
		if (n.Status == StatusAdded || n.Status == StatusModified) {
			// Try matching with available hashes.
			// Priority: SHA1, then MD5, then Merkle (if dir)
			
			hashesToCheck := []string{}
			if n.IsFile {
				if n.SHA1B != "" { hashesToCheck = append(hashesToCheck, n.SHA1B) }
				if n.MD5B != "" { hashesToCheck = append(hashesToCheck, n.MD5B) }
			} else {
				if n.MerkleHash != "" { hashesToCheck = append(hashesToCheck, n.MerkleHash) }
			}
			
			foundMatch := false
			
			for _, h := range hashesToCheck {
				if foundMatch { break }
				
				// Check Removed (Move) first
				if paths, ok := removedMap[h]; ok && len(paths) > 0 {
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
					removedMap[h] = paths[1:]
					foundMatch = true
				} else if paths, ok := existingMap[h]; ok && len(paths) > 0 {
					// Copy
					src := paths[0]
					n.Status = StatusCopy
					if !n.IsFile {
						if !strings.HasSuffix(src, "/") {
							src += "/"
						}
					}
					n.SourcePath = src
					foundMatch = true
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
		if part == "" { continue }
		if current.Children == nil { return nil }
		child, ok := current.Children[part]
		if !ok { return nil }
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
		if !node.IsFile {
			path += "/"
		}
		*results = append(*results, DiffResult{
			Path:       path,
			Status:     node.Status,
			SourcePath: node.SourcePath,
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
