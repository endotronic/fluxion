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
	Path               string // Absolute (Legacy/Sort Key)
	Root               string
	RelPath            string
	Status             Status
	SourcePath         string // Absolute
	SourceRoot         string
	SourceRelPath      string
	AddedCount         int64
	RemovedCount       int64
	ModifiedCount      int64
	CopyCount          int64
	MoveCount          int64
	UnchangedFileCount int64
	UnchangedDirCount  int64
}

// CompareSnapshots computes the diff between two sets of files using a specific hash strategy.
func CompareSnapshots(filesA, filesB map[string]models.FileRecord, rootA, rootB string, hashType string, noCopies, noMoves, showUnchanged bool, onProgress func(current, total int)) ([]DiffResult, error) {
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
	collectResults(root, &results, showUnchanged)

	// 8. Reconstruct Absolute Paths
	finalResults := make([]DiffResult, len(results))
	for i, res := range results {
		// Clean paths (remove leading slash if present from Node path construction)
		relPath := strings.TrimPrefix(res.Path, "/")

		var absPath string
		var rootUsed string

		switch res.Status {
		case StatusAdded, StatusMove, StatusCopy:
			absPath = filepath.Join(rootB, relPath)
			rootUsed = rootB
			if strings.HasSuffix(res.Path, "/") {
				absPath += string(filepath.Separator)
			}
		case StatusModified, StatusRemoved:
			absPath = filepath.Join(rootA, relPath)
			rootUsed = rootA
			if strings.HasSuffix(res.Path, "/") {
				absPath += string(filepath.Separator)
			}
		default:
			absPath = filepath.Join(rootB, relPath)
			rootUsed = rootB
			if strings.HasSuffix(res.Path, "/") {
				absPath += string(filepath.Separator)
			}
		}

		var absSource string
		var sourceRootUsed string
		var sourceRel string

		if res.SourcePath != "" {
			sourceRel = strings.TrimPrefix(res.SourcePath, "/")
			absSource = filepath.Join(rootA, sourceRel)
			sourceRootUsed = rootA
			if strings.HasSuffix(res.SourcePath, "/") {
				absSource += string(filepath.Separator)
			}
		}

		finalResults[i] = DiffResult{
			Path:               absPath,
			Root:               rootUsed,
			RelPath:            relPath,
			Status:             res.Status,
			SourcePath:         absSource,
			SourceRoot:         sourceRootUsed,
			SourceRelPath:      sourceRel,
			AddedCount:         res.AddedCount,
			RemovedCount:       res.RemovedCount,
			ModifiedCount:      res.ModifiedCount,
			CopyCount:          res.CopyCount,
			MoveCount:          res.MoveCount,
			UnchangedFileCount: res.UnchangedFileCount,
			UnchangedDirCount:  res.UnchangedDirCount,
		}
	}

	// Sort results - Removed (Logic is now in traversal order)
	// Traversal is Post-Order (Depth-First), Sibling A-Z.
	// This matches user requirement:
	// 1. Children before Parent (Post-Order)
	// 2. Siblings A-Z

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
		return StatusUnchanged
	}

	// Check children
	allUnchanged := true
	allMovedSource := true
	allRemovedOrMovedSource := true
	allAddedLike := true // Added, Copy, Move

	hasUnchangedOrMixed := false
	hasMove := false
	hasCopy := false
	hasAdded := false
	hasModified := false

	// Track first sources for potential rollup
	var firstMoveSource string
	var firstCopySource string

	changeCount := 0

	// Helper to count changes
	isChange := func(s Status) bool {
		return s == StatusAdded || s == StatusRemoved || s == StatusModified || s == StatusCopy || s == StatusMove
	}

	for _, child := range node.Children {
		s := propagateStatus(child)

		if s != StatusUnchanged {
			allUnchanged = false
		} else {
			hasUnchangedOrMixed = true
		}

		if s != StatusMovedSource {
			allMovedSource = false
		}

		if s != StatusRemoved && s != StatusMovedSource {
			allRemovedOrMovedSource = false
		}

		if s != StatusAdded && s != StatusCopy && s != StatusMove && s != StatusMovedSource {
			allAddedLike = false
		}

		if s == StatusAdded {
			hasAdded = true
		}
		if s == StatusModified {
			hasModified = true
		}
		if s == StatusMove {
			hasMove = true
			if firstMoveSource == "" {
				firstMoveSource = child.SourcePath
			}
		}
		if s == StatusCopy {
			hasCopy = true
			if firstCopySource == "" {
				firstCopySource = child.SourcePath
			}
		}

		if s == StatusMixed {
			hasUnchangedOrMixed = true
			allUnchanged = false
			allMovedSource = false
			allRemovedOrMovedSource = false
			allAddedLike = false
		}

		if isChange(s) {
			changeCount++
		}
	}

	if allUnchanged {
		node.Status = StatusUnchanged
		return StatusUnchanged
	}

	if allMovedSource {
		node.Status = StatusMovedSource
		return StatusMovedSource
	}

	if allRemovedOrMovedSource {
		node.Status = StatusRemoved
		return StatusRemoved
	}

	// New Rule: If directory is new (HashA empty) and contains only Added-like things (Added, Move, Copy).
	if node.HashA == "" && allAddedLike {
		// 1. Pure additions -> Always added
		if !hasMove && !hasCopy {
			node.Status = StatusAdded
			return StatusAdded
		}
		// 2. Mixed/Multiple changes -> Rollup to Added
		if changeCount > 1 {
			node.Status = StatusAdded
			return StatusAdded
		}
		// 3. Single Move/Copy -> Fall through to show detail (StatusMixed)
	}

	// Determine if we should attempt a rollup
	canRollup := allAddedLike || !hasUnchangedOrMixed || changeCount > 2

	// Refined Prioritization for canRollup:
	if canRollup {
		if hasModified {
			node.Status = StatusModified
			return StatusModified
		}
		if allAddedLike {
			// Rule: If we have mixed types of "AddedLike" operations (e.g. Move + Copy),
			// we should rollup as Modified (Mixed operations on a new/moved set),
			// rather than picking one winner and confusing the user.
			typesFound := 0
			if hasMove {
				typesFound++
			}
			if hasCopy {
				typesFound++
			}
			if hasAdded {
				typesFound++
			}

			if typesFound > 1 {
				node.Status = StatusModified
				return StatusModified
			}

			// Otherwise, pure type (or single dominating type)
			// Preference: Move > Added > Copy
			if hasMove {
				if changeCount == 1 {
					node.Status = StatusMixed
					return StatusMixed
				}
				node.Status = StatusMove
				if firstMoveSource != "" {
					node.SourcePath = firstMoveSource
				} else {
					node.SourcePath = "" // Ensure SourcePath is cleared if not a single source
				}
				return StatusMove
			}
			// Added > Copy (per Rollup_Added test)
			if hasAdded {
				node.Status = StatusAdded
				node.SourcePath = "" // Added items don't have a SourcePath
				return StatusAdded
			}
			// Copy last
			if hasCopy {
				if changeCount == 1 {
					node.Status = StatusMixed
					return StatusMixed
				}
				node.Status = StatusCopy
				if firstCopySource != "" {
					node.SourcePath = firstCopySource
				} else {
					node.SourcePath = "" // Ensure SourcePath is cleared if not a single source
				}
				return StatusCopy
			}
		}
		// If canRollup, and we haven't returned yet, it means me have mixed changes (e.g. Added + Removed)
		// but no Unchanged items preventing rollup. Summary: Modified.
		if !hasUnchangedOrMixed {
			node.Status = StatusModified
			return StatusModified
		}
	}

	node.Status = StatusMixed
	return StatusMixed
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

// detectMovesCopies
func detectMovesCopies(root *Node, noCopies, noMoves bool) {
	if noCopies && noMoves {
		return
	}
	// 1. Index Removed, Modified, and Existing nodes
	removedMap := make(map[string][]string)  // Hash -> [Paths] (For Moves)
	modifiedMap := make(map[string][]string) // Hash -> [Paths] (For Swap Moves / Copies)
	existingMap := make(map[string][]string) // Hash -> [Paths] (For Copies)

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

		// Only Removed nodes are primary sources for Moves (overwrite/rename)
		if n.Status == StatusRemoved {
			add(removedMap, n.HashA, n.Path)
		} else if n.Status == StatusModified {
			add(modifiedMap, n.HashA, n.Path)
		}

		// Any existing node (Removed, Modified, Unchanged) can be source for Copy
		// (Modified is explicitly tracked in modifiedMap too, but keeping it in existingMap logic for broad copy support)
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

			// Check Removed (Move) first - Strongest Match
			if !noMoves {
				if paths, ok := removedMap[hash]; ok && len(paths) > 0 {
					// Move from Removed
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
				} else if n.Status == StatusModified {
					// Special Case: File Swap / Rewrite
					// If target is Modified, and source is Modified, treat as Move (Swap)
					if paths, ok := modifiedMap[hash]; ok && len(paths) > 0 {
						src := paths[0]
						n.Status = StatusMove
						if !n.IsFile {
							if !strings.HasSuffix(src, "/") {
								src += "/"
							}
						}
						n.SourcePath = src
						matched = true
						// Don't consume modifiedMap? Or should we?
						// Swaps are 1:1, so yes consume.
						modifiedMap[hash] = paths[1:]
					}
				}
			}

			if !matched && !noCopies {
				// Prioritize modifiedMap for copies if Added (Rollup case)
				if paths, ok := modifiedMap[hash]; ok && len(paths) > 0 {
					src := paths[0]
					n.Status = StatusCopy
					if !n.IsFile {
						if !strings.HasSuffix(src, "/") {
							src += "/"
						}
					}
					n.SourcePath = src
					matched = true
				} else if paths, ok := existingMap[hash]; ok && len(paths) > 0 {
					// Copy from broad match
					src := paths[0]
					n.Status = StatusCopy
					if !n.IsFile {
						if !strings.HasSuffix(src, "/") {
							src += "/"
						}
					}
					n.SourcePath = src
					matched = true
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
// collectResults traverses logic to collapse nodes
func collectResults(node *Node, results *[]DiffResult, showUnchanged bool) {
	// Skip root virtual node (unless root itself changed? e.g. everything removed?)
	// Root has no name/path usually in this implementation ("").
	// But its children are top level.

	if node.Name == "" { // Root
		// Sort keys for deterministic iteration
		var names []string
		for name := range node.Children {
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			collectResults(node.Children[name], results, showUnchanged)
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

		var added, removed, modified, copied, moved int64
		var unchangedFiles, unchangedDirs int64

		if !node.IsFile {
			path += "/"
			// Count recursive files stats
			stats := accumulateStats(node)
			added = stats.AddedCount
			removed = stats.RemovedCount
			modified = stats.ModifiedCount
			copied = stats.CopyCount
			moved = stats.MoveCount
			if showUnchanged {
				unchangedFiles = stats.UnchangedFileCount
				unchangedDirs = stats.UnchangedDirCount
			}
		} else {
			// Leaf node specific counts (for single file)
			switch node.Status {
			case StatusAdded:
				added = 1
			case StatusRemoved:
				removed = 1
			case StatusModified:
				modified = 1
			case StatusCopy:
				copied = 1
			case StatusMove:
				moved = 1
			}
		}

		*results = append(*results, DiffResult{
			Path:               path,
			Status:             node.Status,
			SourcePath:         node.SourcePath,
			AddedCount:         added,
			RemovedCount:       removed,
			ModifiedCount:      modified,
			CopyCount:          copied,
			MoveCount:          moved,
			UnchangedFileCount: unchangedFiles,
			UnchangedDirCount:  unchangedDirs,
		})
		return
	}

	// If Mixed, recurse
	if node.Status == StatusMixed {
		// Sort keys for deterministic iteration (A-Z Sibling Sort)
		var names []string
		for name := range node.Children {
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			collectResults(node.Children[name], results, showUnchanged)
		}

		// Post-Order Emission for Mixed/Unchanged Parent
		if showUnchanged {
			// Check if this mixed node has unchanged content
			stats := accumulateStats(node)
			if stats.UnchangedFileCount > 0 || stats.UnchangedDirCount > 0 {
				path := node.Path
				if !node.IsFile {
					path += "/"
				}
				*results = append(*results, DiffResult{
					Path:       path,
					Status:     StatusMixed,
					SourcePath: node.SourcePath,
					// For Mixed nodes (context), we only report Unchanged counts.
					// Recursive changes (Added/Removed/etc) are reported by children traversal.
					AddedCount:         0,
					RemovedCount:       0,
					ModifiedCount:      0,
					CopyCount:          0,
					MoveCount:          0,
					UnchangedFileCount: stats.UnchangedFileCount,
					UnchangedDirCount:  stats.UnchangedDirCount,
				})
			}
		}
	}
}

func accumulateStats(node *Node) DiffResult {
	if node.IsFile {
		// Return count for this file
		res := DiffResult{}
		switch node.Status {
		case StatusAdded:
			res.AddedCount = 1
		case StatusRemoved:
			res.RemovedCount = 1
		case StatusModified:
			res.ModifiedCount = 1
		case StatusCopy:
			res.CopyCount = 1
		case StatusMove:
			res.MoveCount = 1
		case StatusUnchanged:
			res.UnchangedFileCount = 1
		}
		return res
	}

	var total DiffResult
	if node.Status == StatusUnchanged {
		// If directory is Unchanged, count itself as 1 UnchangedDir?
		// Or do we count only children?
		// The requirement example: "3 directories, 8 files unchanged" for a parent context.
		// If `node` is `docker/` (Mixed), and it has `svc1` (Removed), `src2` (Removed), and `backup/` (Unchanged Dir).
		// We want to count `backup/` as 1 Unchanged Dir.
		// If `backup/` has 100 files, do we count them?
		// "3 directories, 8 files". Usually this means immediate children or recursive leaf files?
		// Given other stats (AddedCount) are typically recursive files, we should probably be consistent.
		// But "3 directories" implies structure count.
		// Let's count recursive files.
		// For directories, do we count them? AddedCount usually doesn't count added directories explicitly in existing logic (it returns 0 for IsFile=false unless we check Children).
		// Wait, accumulateStats sums up children.
		// If child is Directory and Added, accumulateStats(child) returns its file counts.
		// It does NOT seem to count the directory itself in `AddedCount`.
		// Let's verify:
		// case StatusAdded: res.AddedCount = 1 (Only if IsFile).
		// So existing logic counts FILES only.
		// But the user request says "3 directories, 8 files unchanged".
		// This implies we DO want directory counts.
		// I'll add logic to count directories too.
		total.UnchangedDirCount = 1
	}

	for _, child := range node.Children {
		// Recursive
		s := accumulateStats(child)
		total.AddedCount += s.AddedCount
		total.RemovedCount += s.RemovedCount
		total.ModifiedCount += s.ModifiedCount
		total.CopyCount += s.CopyCount
		total.MoveCount += s.MoveCount
		total.UnchangedFileCount += s.UnchangedFileCount
		total.UnchangedDirCount += s.UnchangedDirCount
	}
	return total
}
