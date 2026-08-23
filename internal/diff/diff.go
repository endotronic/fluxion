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

	// StatusTruncated stands in for the lines a directory did not have the
	// budget to print. It carries their combined counts, so the content is
	// summarised rather than dropped.
	StatusTruncated Status = "Truncated"
)

// DefaultMaxLinesPerDir is the line budget a single directory gets before the
// rest of its changes are summarised as one "... and N more" line. A directory
// that holds unchanged content may not be collapsed - saying "Modified" over a
// file that did not change is the kind of claim goals.md forbids - so without a
// budget a half-changed directory of 40,000 files prints 20,000 lines.
const DefaultMaxLinesPerDir = 25

// Node represents a file or directory in the diff tree
type Node struct {
	Name     string
	Path     string // Full path
	IsFile   bool
	Status   Status
	Children map[string]*Node

	// Presence, tracked explicitly. This is deliberately NOT inferred from
	// "has a non-empty hash": a snapshot can carry records with no hash of the
	// type being compared - a legacy MD5-only import merged into a SHA-1
	// snapshot is the reachable case - and inferring presence from the hash
	// made every one of those files read as absent from A.
	InA bool // recorded as a regular file in A
	InB bool // recorded as a regular file in B

	DirA bool // something at or under this path existed in A
	DirB bool // something at or under this path existed in B

	// Metadata for comparison (Generic, set based on strategy).
	// For a directory, HashA/HashB hold its merkle hash.
	HashA string
	HashB string

	SizeA int64
	SizeB int64

	// FileTwin carries the file aspect of a path that is a file on one side and
	// a directory on the other. No filesystem holds both at once, but two
	// snapshots taken months apart disagree about plenty, and both halves have
	// to reach the user - so they are reported as two separate lines.
	FileTwin *Node

	// For Move/Copy
	SourcePath string

	// matched records that detectMovesCopies assigned this node's Move/Copy
	// status by content, as opposed to it being inferred from children during
	// a rollup. Only a matched status is fixed; a rolled-up one has to be
	// recomputed whenever the children beneath it change.
	matched bool
}

// presentInA reports whether anything at or under this node existed in A.
func (n *Node) presentInA() bool {
	return n.InA || n.DirA || (n.FileTwin != nil && n.FileTwin.InA)
}

// presentInB reports whether anything at or under this node existed in B.
func (n *Node) presentInB() bool {
	return n.InB || n.DirB || (n.FileTwin != nil && n.FileTwin.InB)
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

	// HiddenCount is set only on StatusTruncated: how many lines this one line
	// stands in for.
	HiddenCount int64
}

// FileIterator is a function that calls yield for each file record.
// It stops if yield returns an error.
type FileIterator func(yield func(string, models.FileRecord) error) error

// Options configures a comparison. The zero value is valid except for HashType,
// and means "compare against no roots, detect moves and copies, print every
// line" - note that MaxLinesPerDir 0 is unlimited, so callers wanting the
// default budget must say DefaultMaxLinesPerDir.
type Options struct {
	RootA, RootB string
	HashType     string // "sha1" or "md5"

	NoCopies bool
	NoMoves  bool

	// ShowUnchanged adds context lines carrying the unchanged counts.
	ShowUnchanged bool

	// MaxLinesPerDir caps how many lines any one directory contributes before
	// the remainder is summarised. 0 means no cap.
	MaxLinesPerDir int

	OnProgress func(current int)
}

// CompareSnapshots computes the diff between two sets of files.
func CompareSnapshots(iterA, iterB FileIterator, opts Options) ([]DiffResult, error) {
	rootA, rootB := opts.RootA, opts.RootB
	hashType := opts.HashType
	onProgress := opts.OnProgress

	root := &Node{
		Name:     "",
		Path:     "",
		Children: make(map[string]*Node),
		Status:   StatusUnchanged,
	}

	current := 0

	// 1. Insert A
	err := iterA(func(path string, record models.FileRecord) error {
		insertNode(root, path, record, true, hashType)
		current++
		if onProgress != nil && current%1000 == 0 {
			onProgress(current)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 2. Insert B
	err = iterB(func(path string, record models.FileRecord) error {
		insertNode(root, path, record, false, hashType)
		current++
		if onProgress != nil && current%1000 == 0 {
			onProgress(current)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if onProgress != nil {
		onProgress(current)
	}

	// 3. Separate the file and directory aspects of any colliding path
	splitFileDirCollisions(root)

	// 4. Compute Merkle Hashes (Post-Order)
	computeMerkleHashes(root)

	// 5. Propagate Status (Pass 1)
	propagateStatus(root)

	// 6. Detect Moves/Copies
	detectMovesCopies(root, opts.NoCopies, opts.NoMoves)

	// 7. Propagate Status (Pass 2)
	propagateStatus(root)

	// 8. Make sure no move source is left unmentioned because the line that was
	// supposed to name it got collapsed - or truncated - away. The only reliable
	// way to know what the output says is to collect it, so this collects a
	// trial run, demotes any source that run failed to account for, and repeats:
	// reinstating a source changes what its ancestors say, which changes the
	// output again. Each pass only ever turns MovedSource into Removed, so it
	// converges; the bound is a guard, not an expected limit.
	newCollector := func() *collector {
		return &collector{showUnchanged: opts.ShowUnchanged, maxLinesPerDir: opts.MaxLinesPerDir}
	}

	c := newCollector()
	c.collect(root)

	for i := 0; i < 32; i++ {
		if !reinstateHiddenMoveSources(root, accountedPaths(c.results)) {
			break
		}
		propagateStatus(root)
		c = newCollector()
		c.collect(root)
	}

	// 9. The trial run above is the final output.
	results := c.results

	// 10. Reconstruct Absolute Paths
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
			HiddenCount:        res.HiddenCount,
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

	if isA {
		current.InA = true
		current.HashA = hash
		current.SizeA = record.SizeBytes
	} else {
		current.InB = true
		current.HashB = hash
		current.SizeB = record.SizeBytes
	}

	current.Status = fileStatus(current)
}

// fileStatus classifies the file aspect of a node from what each side recorded.
func fileStatus(n *Node) Status {
	switch {
	case n.InA && !n.InB:
		return StatusRemoved
	case n.InB && !n.InA:
		return StatusAdded
	case n.HashA == "" || n.HashB == "":
		// Present on both sides, but at least one side carries no hash of the
		// type being compared, so we cannot claim the contents match. Report
		// Modified: over-reporting costs the user reading time, and a false
		// "unchanged" can cost them the file.
		return StatusModified
	case n.HashA == n.HashB:
		return StatusUnchanged
	default:
		return StatusModified
	}
}

// splitFileDirCollisions moves the file aspect of any path that is a file on one
// side and a directory on the other into a FileTwin, leaving the original node
// as a pure directory.
//
// Without this, such a node was both IsFile and the owner of Children, and every
// consumer checked IsFile first - so the directory half was never looked at and
// its files disappeared from the diff without a trace. That is the exact failure
// the severity rule in knowledge/goals.md ranks worst.
func splitFileDirCollisions(node *Node) {
	for _, child := range node.Children {
		splitFileDirCollisions(child)
	}

	if !node.IsFile || len(node.Children) == 0 {
		return
	}

	twin := &Node{
		Name:   node.Name,
		Path:   node.Path,
		IsFile: true,
		InA:    node.InA,
		InB:    node.InB,
		HashA:  node.HashA,
		HashB:  node.HashB,
		SizeA:  node.SizeA,
		SizeB:  node.SizeB,
	}
	twin.Status = fileStatus(twin)
	node.FileTwin = twin

	// What remains is a directory and nothing else.
	node.IsFile = false
	node.InA, node.InB = false, false
	node.HashA, node.HashB = "", ""
	node.SizeA, node.SizeB = 0, 0
	node.Status = StatusUnchanged
}

// propagateStatus updates directory statuses based on children.
// Returns the status of the node.
// propagateStatus updates directory statuses based on children.
// Returns the status of the node.
// propagateStatus updates directory statuses based on children.
// Returns the status of the node and a boolean indicating if the node contains any unchanged content (recursively).
// propagateStatus resolves a node's status, combining its directory aspect with
// its file aspect when the path is both.
func propagateStatus(node *Node) (Status, bool) {
	status, hasUnchanged := propagateNodeStatus(node)

	twin := node.FileTwin
	if twin == nil {
		return status, hasUnchanged
	}

	if twin.Status == StatusUnchanged {
		return status, true
	}
	if twin.Status != status {
		// The path is two different things with two different fates. Report
		// Mixed so no ancestor collapses over it: a single summary status here
		// would necessarily hide one of the two halves.
		node.Status = StatusMixed
		return StatusMixed, hasUnchanged
	}
	return status, hasUnchanged
}

func propagateNodeStatus(node *Node) (Status, bool) {
	if node.IsFile {
		return node.Status, node.Status == StatusUnchanged
	}

	// A node whose Move/Copy was established by content matching keeps it; the
	// match is a fact about the data, not an inference from children. A rolled-up
	// Move/Copy is only an inference, so it must not be preserved here - doing so
	// froze directories at a stale summary that later passes could not correct.
	if node.matched {
		return node.Status, false
	}

	if len(node.Children) == 0 {
		return StatusUnchanged, true
	}

	// Recomputing from scratch: drop any source attribution a previous pass
	// inferred, so it cannot outlive the reasoning that produced it.
	node.SourcePath = ""

	// Check children
	allUnchanged := true
	allMovedSource := true
	allRemovedOrMovedSource := true
	allAddedLike := true // Added, Copy, Move

	hasMove := false
	hasCopy := false
	hasAdded := false
	hasModified := false
	hasMovedSource := false

	hasUnchangedContent := false

	// Track first sources for potential rollup
	var firstMoveSource string
	var firstCopySource string

	changeCount := 0

	// Helper to count changes
	isChange := func(s Status) bool {
		return s == StatusAdded || s == StatusRemoved || s == StatusModified || s == StatusCopy || s == StatusMove
	}

	for _, child := range node.Children {
		s, childHasUnchanged := propagateStatus(child)

		if childHasUnchanged {
			hasUnchangedContent = true
		}

		if s != StatusUnchanged {
			allUnchanged = false
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
		if s == StatusMovedSource {
			hasMovedSource = true
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
		return StatusUnchanged, true
	}

	if allMovedSource {
		node.Status = StatusMovedSource
		return StatusMovedSource, false
	}

	if allRemovedOrMovedSource {
		node.Status = StatusRemoved
		return StatusRemoved, false
	}

	// If the directory is new in B and contains only Added-like things (Added,
	// Move, Copy). Presence is checked directly - a directory whose A-side
	// children all lack the compared hash has an empty merkle string but is
	// very much present.
	if !node.DirA && allAddedLike {
		// 1. Pure additions -> Always added
		if !hasMove && !hasCopy {
			node.Status = StatusAdded
			return StatusAdded, false
		}
		// 2. Mixed/Multiple changes -> Rollup to Added
		if changeCount > 1 {
			node.Status = StatusAdded
			return StatusAdded, false
		}
		// 3. Single Move/Copy -> Fall through to show detail (StatusMixed)
	}

	// Determine if we should attempt a rollup
	canRollup := !hasUnchangedContent && (allAddedLike || changeCount >= 2)

	// Refined Prioritization for canRollup:
	if canRollup {
		if hasModified {
			node.Status = StatusModified
			return StatusModified, hasUnchangedContent
		}
		// allAddedLike counts MovedSource as "added-like" so that a directory
		// emptied by a move still rolls up. But content that *left* this
		// directory is not something arriving in it: summarising as Added, Move
		// or Copy would describe only what came and silently drop what went. Let
		// it fall through to Modified (or to Mixed, if detail is available) so
		// the loss stays on screen.
		if allAddedLike && !hasMovedSource {
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
				return StatusModified, hasUnchangedContent
			}

			// Otherwise, pure type (or single dominating type)
			// Preference: Move > Added > Copy
			if hasMove {
				if changeCount == 1 {
					node.Status = StatusMixed
					return StatusMixed, hasUnchangedContent
				}
				node.Status = StatusMove
				if firstMoveSource != "" {
					node.SourcePath = firstMoveSource
				} else {
					node.SourcePath = "" // Ensure SourcePath is cleared if not a single source
				}
				return StatusMove, false // Move implies pure change
			}
			// Added > Copy (per Rollup_Added test)
			if hasAdded {
				node.Status = StatusAdded
				node.SourcePath = "" // Added items don't have a SourcePath
				return StatusAdded, false
			}
			// Copy last
			if hasCopy {
				if changeCount == 1 {
					node.Status = StatusMixed
					return StatusMixed, hasUnchangedContent
				}
				node.Status = StatusCopy
				if firstCopySource != "" {
					node.SourcePath = firstCopySource
				} else {
					node.SourcePath = "" // Ensure SourcePath is cleared if not a single source
				}
				return StatusCopy, false
			}
		}
		// If canRollup, and we haven't returned yet, it means we have mixed changes (e.g. Added + Removed)
		// but no Unchanged items preventing rollup. Summary: Modified.
		if !hasUnchangedContent {
			// If this directory didn't exist in A at all, it is Added, even when
			// it contains Mixed things (Moves/Copies) that confuse allAddedLike.
			if !node.DirA {
				node.Status = StatusAdded
				return StatusAdded, false
			}

			node.Status = StatusModified
			return StatusModified, false
		}
	}

	node.Status = StatusMixed
	return StatusMixed, hasUnchangedContent
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

		if child.presentInA() {
			node.DirA = true
		}
		if child.presentInB() {
			node.DirB = true
		}

		if child.HashA != "" {
			hashesA = append(hashesA, child.Name+":"+child.HashA)
		}
		if child.HashB != "" {
			hashesB = append(hashesB, child.Name+":"+child.HashB)
		}

		// A split path contributes both halves, tagged so that a directory and
		// a file of the same name cannot produce the same entry.
		if twin := child.FileTwin; twin != nil {
			if twin.HashA != "" {
				hashesA = append(hashesA, child.Name+":file:"+twin.HashA)
			}
			if twin.HashB != "" {
				hashesB = append(hashesB, child.Name+":file:"+twin.HashB)
			}
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
	// 1. Index Removed, Modified, and Existing nodes.
	//
	// The maps hold nodes rather than paths. A path that is a file on one side
	// and a directory on the other exists twice in the tree, so resolving a
	// source by path alone could mark the wrong half as the origin of a move.
	removedMap := make(map[string][]*Node)  // Hash -> nodes (For Moves)
	modifiedMap := make(map[string][]*Node) // Hash -> nodes (For Swap Moves / Copies)
	existingMap := make(map[string][]*Node) // Hash -> nodes (For Copies)

	add := func(m map[string][]*Node, n *Node, hash string, size int64) {
		if hash == "" {
			return
		}
		// Every empty file has the same hash, so matching on it would attribute
		// arbitrary moves between unrelated empty files.
		if n.IsFile && size == 0 {
			return
		}
		m[hash] = append(m[hash], n)
	}

	indexOne := func(n *Node) {
		// Only Removed nodes are primary sources for Moves (overwrite/rename)
		if n.Status == StatusRemoved {
			add(removedMap, n, n.HashA, n.SizeA)
		} else if n.Status == StatusModified {
			add(modifiedMap, n, n.HashA, n.SizeA)
		}

		// Any node that existed in A (Removed, Modified, Unchanged) can be the
		// source of a copy.
		if n.Status != StatusAdded {
			add(existingMap, n, n.HashA, n.SizeA)
		}
	}

	var index func(*Node)
	index = func(n *Node) {
		if !n.presentInA() {
			return
		}

		indexOne(n)
		if n.FileTwin != nil {
			indexOne(n.FileTwin)
		}

		// Sorted, because which source a move is attributed to depends on the
		// order candidates were indexed. Map order made that vary run to run,
		// so the same two snapshots could report different origins.
		for _, child := range sortedChildren(n) {
			index(child)
		}
	}
	index(root)

	// 2. Scan for Added/Modified nodes and match
	matchOne := func(n *Node) {
		if n.Status != StatusAdded && n.Status != StatusModified {
			return
		}

		hash := n.HashB
		if hash == "" {
			return
		}
		if n.IsFile && n.SizeB == 0 {
			return
		}

		// A directory that already held something in A cannot be summarised as
		// a whole move or copy. That label describes only what the directory
		// holds now; propagateStatus stops at a node with a SourcePath, so
		// whatever the directory used to hold and has since lost would never be
		// looked at, let alone reported.
		if !n.IsFile && n.presentInA() {
			return
		}

		// take pulls the first candidate for hash, consuming it.
		take := func(m map[string][]*Node) *Node {
			nodes := m[hash]
			if len(nodes) == 0 {
				return nil
			}
			m[hash] = nodes[1:]
			return nodes[0]
		}

		sourcePath := func(src *Node) string {
			p := src.Path
			if !n.IsFile && !strings.HasSuffix(p, "/") {
				p += "/"
			}
			return p
		}

		matched := false

		// Check Removed (Move) first - strongest match.
		if !noMoves {
			if src := take(removedMap); src != nil {
				n.Status = StatusMove
				n.SourcePath = sourcePath(src)
				n.matched = true

				if src.Status == StatusRemoved {
					src.Status = StatusMovedSource
				}
				matched = true
			} else if n.Status == StatusModified {
				// File swap / rewrite: the target is Modified and a Modified
				// node elsewhere holds the content that used to be here.
				// Swaps are 1:1, so the source is consumed.
				if src := take(modifiedMap); src != nil {
					n.Status = StatusMove
					n.SourcePath = sourcePath(src)
					n.matched = true
					matched = true
				}
			}
		}

		if !matched && !noCopies {
			// Prefer a modified source over an unchanged one (rollup case).
			src := take(modifiedMap)
			if src == nil {
				src = take(existingMap)
			}
			if src != nil {
				n.Status = StatusCopy
				n.SourcePath = sourcePath(src)
				n.matched = true
			}
		}
	}

	var match func(*Node)
	match = func(n *Node) {
		matchOne(n)
		if n.FileTwin != nil {
			matchOne(n.FileTwin)
		}

		for _, child := range sortedChildren(n) {
			match(child)
		}
	}
	match(root)
}

// sortedChildren returns a node's children in name order, so that every
// traversal that can affect the output is deterministic.
func sortedChildren(n *Node) []*Node {
	children := make([]*Node, 0, len(n.Children))
	for _, child := range n.Children {
		children = append(children, child)
	}
	sort.Slice(children, func(i, j int) bool {
		return children[i].Name < children[j].Name
	})
	return children
}

// accountedPaths reads a trial run of the output and returns the paths it tells
// the reader something about - either as the origin of a move ("<- old/x"), or
// as a line reporting loss, whose counts cover everything beneath it.
//
// Deriving this from real output rather than predicting it is what keeps the
// check honest: collection collapses subtrees *and* enforces the line budget,
// and both can make a line the reader was supposed to see disappear.
//
// Truncation summaries are deliberately not included. A suppressed move source
// emits no line, so it contributes nothing to a summary's counts - being inside
// a truncated block is not being mentioned.
func accountedPaths(results []DiffResult) map[string]bool {
	out := make(map[string]bool)
	for _, r := range results {
		switch r.Status {
		case StatusMove:
			if r.SourcePath != "" {
				out[strings.TrimSuffix(r.SourcePath, "/")] = true
			}
		case StatusRemoved, StatusModified:
			out[strings.TrimSuffix(r.Path, "/")] = true
		}
	}
	return out
}

// sourceNamed reports whether path, or a directory containing it, is accounted
// for in the output. An ancestor is enough: "Move new/ <- old/" accounts for
// everything that was under old/, and so does "Removed old/".
func sourceNamed(named map[string]bool, path string) bool {
	p := strings.TrimSuffix(path, "/")
	for p != "" && p != "/" {
		if named[p] {
			return true
		}
		i := strings.LastIndex(p, "/")
		if i < 0 {
			break
		}
		p = p[:i]
	}
	return false
}

// reinstateHiddenMoveSources turns a suppressed move source back into a plain
// removal when nothing in the output would have mentioned it.
//
// It reports whether it changed anything, because the change has to be fed back
// through propagateStatus: an ancestor that rolled up to "Added" while this node
// was still a suppressed move source is wrong once the node is a removal again.
func reinstateHiddenMoveSources(node *Node, named map[string]bool) bool {
	if node.Status == StatusMovedSource {
		if sourceNamed(named, node.Path) {
			// Something in the output names this path or a directory above it,
			// so everything under it is accounted for and there is nothing to
			// recurse into.
			return false
		}
		// Convert the whole subtree, not just this node. Leaving MovedSource
		// descendants behind would let the next propagateStatus roll this node
		// straight back to MovedSource and undo the fix.
		demoteMovedSources(node)
		return true
	}

	changed := false
	if twin := node.FileTwin; twin != nil && twin.Status == StatusMovedSource &&
		!sourceNamed(named, twin.Path) {
		twin.Status = StatusRemoved
		changed = true
	}

	for _, child := range node.Children {
		if reinstateHiddenMoveSources(child, named) {
			changed = true
		}
	}
	return changed
}

func demoteMovedSources(node *Node) {
	if node.Status == StatusMovedSource {
		node.Status = StatusRemoved
	}
	if twin := node.FileTwin; twin != nil && twin.Status == StatusMovedSource {
		twin.Status = StatusRemoved
	}
	for _, child := range node.Children {
		demoteMovedSources(child)
	}
}

// collectResults walks the tree and appends one entry per reportable node,
// collapsing whole subtrees where a single line says everything.
// collector walks the finished tree and turns it into the lines the user sees.
type collector struct {
	results        []DiffResult
	showUnchanged  bool
	maxLinesPerDir int
}

func (c *collector) collect(node *Node) {
	if node.Name != "" && node.FileTwin != nil {
		twin := node.FileTwin
		// Two lines for one path. Emit whichever half existed in A first, so
		// the pair reads in the order things happened: the old thing goes, the
		// new thing arrives.
		if twin.Status == StatusRemoved || twin.Status == StatusMovedSource {
			c.collectNode(twin)
			c.collectNode(node)
		} else {
			c.collectNode(node)
			c.collectNode(twin)
		}
		return
	}

	c.collectNode(node)
}

// collectChildren emits the children of a directory that could not be collapsed,
// then applies the line budget to whatever they produced.
//
// The virtual root is exempt. The budget exists to stop one directory drowning
// the output, not to cap the diff as a whole - and since a truncated directory
// always emits budget+1 lines, a capped root would immediately re-truncate the
// summary it had just produced, leaving one "... N more" line for the entire
// run.
func (c *collector) collectChildren(node *Node) {
	start := len(c.results)
	for _, child := range sortedChildren(node) {
		c.collect(child)
	}
	if node.Name != "" {
		c.applyBudget(node, start)
	}
}

// applyBudget folds everything this directory emitted past maxLinesPerDir into a
// single summary line. The summary carries the combined counts of the lines it
// replaces, so nothing is silently dropped - the reader is told how much more is
// there and what kind it is, and can re-run with a larger budget to see it.
func (c *collector) applyBudget(node *Node, start int) {
	if c.maxLinesPerDir <= 0 {
		return
	}
	emitted := c.results[start:]
	if len(emitted) <= c.maxLinesPerDir {
		return
	}

	var sum DiffResult
	for _, r := range emitted[c.maxLinesPerDir:] {
		sum.AddedCount += r.AddedCount
		sum.RemovedCount += r.RemovedCount
		sum.ModifiedCount += r.ModifiedCount
		sum.CopyCount += r.CopyCount
		sum.MoveCount += r.MoveCount
		sum.UnchangedFileCount += r.UnchangedFileCount
		sum.UnchangedDirCount += r.UnchangedDirCount
		// A nested summary already stands in for lines of its own; this one
		// stands in for those too.
		sum.HiddenCount += r.HiddenCount
	}
	sum.HiddenCount += int64(len(emitted) - c.maxLinesPerDir)

	sum.Path = node.Path + "/"
	sum.Status = StatusTruncated

	c.results = append(c.results[:start+c.maxLinesPerDir], sum)
}

func (c *collector) collectNode(node *Node) {
	if node.Name == "" { // Root
		c.collectChildren(node)

		if c.showUnchanged {
			stats := accumulateStats(node)
			if stats.UnchangedFileCount > 0 || stats.UnchangedDirCount > 0 {
				c.results = append(c.results, DiffResult{
					Path:               ".", // Root representation
					Status:             StatusMixed,
					UnchangedFileCount: stats.UnchangedFileCount,
					UnchangedDirCount:  stats.UnchangedDirCount,
				})
			}
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
			if c.showUnchanged {
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

		c.results = append(c.results, DiffResult{
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
		c.collectChildren(node)

		// Post-Order Emission for Mixed/Unchanged Parent
		if c.showUnchanged {
			// Check if this mixed node has unchanged content
			stats := accumulateStats(node)
			if stats.UnchangedFileCount > 0 || stats.UnchangedDirCount > 0 {
				path := node.Path
				if !node.IsFile {
					path += "/"
				}
				c.results = append(c.results, DiffResult{
					Path:       path,
					Status:     StatusMixed,
					SourcePath: node.SourcePath,
					// For Mixed nodes (context), we only report Unchanged counts.
					// Recursive changes (Added/Removed/etc) are reported by children traversal.
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

	if node.FileTwin != nil {
		s := accumulateStats(node.FileTwin)
		total.AddedCount += s.AddedCount
		total.RemovedCount += s.RemovedCount
		total.ModifiedCount += s.ModifiedCount
		total.CopyCount += s.CopyCount
		total.MoveCount += s.MoveCount
		total.UnchangedFileCount += s.UnchangedFileCount
		total.UnchangedDirCount += s.UnchangedDirCount
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
