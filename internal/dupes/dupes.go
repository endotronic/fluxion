package dupes

import (
	"fluxion/internal/models"
	"sort"
	"strings"
)

// DuplicateGroup represents a set of paths that are duplicates
type DuplicateGroup struct {
	Hash      string
	Size      int64
	Paths     []string
	IsDir     bool
	ItemCount int // Number of files/dirs inside if dir
}

// Node for internal tree
type Node struct {
	Name     string
	Path     string
	IsFile   bool
	Size     int64
	Hash     string
	Children map[string]*Node
}

// FindDuplicates finds duplicates in a set of FileRecords
func FindDuplicates(files map[string]models.FileRecord, minSize int64, rootPath string) ([]DuplicateGroup, error) {
	// 1. Build Tree
	root := &Node{
		Children: make(map[string]*Node),
	}

	for path, rec := range files {
		insertNode(root, path, rec)
	}

	// 2. Compute Hashes and Sizes (Post-Order)
	computeMetadata(root)

	// 3. Index Nodes by Hash
	hashIndex := make(map[string][]*Node)
	indexNodes(root, hashIndex)

	// 4. Filter and Group
	var groups []DuplicateGroup
	
	// We need to handle "Collapsing".
	// Logic: If a directory is duplicated, we shouldn't report its children as duplicates.
	// But how do we know if a child is *only* duplicated because the parent is?
	// If Parent A and Parent B are exact dupes, then Child A/x and Child B/x are exact dupes.
	// We want to report {A, B} and suppress {A/x, B/x}.
	// Approach: 
	// Iterate valid duplicate hashes. Check if their parents are also duplicates?
	// Or simpler: If we identify a Dir duplicate, mark its children as "covered".
	
	// Let's sort hashIndex by some metric? No.
	// We can iterate the Tree Top-Down logic again?
	// Or:
	// 1. Identify all duplicate Hashes that meet minSize.
	// 2. For each such Hash, we have a list of Nodes.
	// 3. Check if all Nodes in this group are children of Nodes that form another duplicate group?
	// That's O(N^2) potentially.
	
	// Better: Top-Down traversal to finding "Highest Level Duplicates".
	// But "Finding Duplicates" is inherently global (hashing). 
	// You can't just walk the tree to find them.
	
	// Let's stick to the "Covered" approach.
	// 1. Collect all potential duplicate groups (Count > 1 && Size >= minSize).
	// 2. Sort groups by Path depth?
	// Actually, we can check node ancestry.
	// If for a group {N1, N2, N3}, their parents {P1, P2, P3} are ALSO a duplicate group (Hash(P1)==Hash(P2)==Hash(P3)),
	// Then this group is redundant.
	// Caveat: What if P1 and P2 match, but P3 is different parent?
	// Then {N1, N2} are covered by {P1, P2}, but N3 is loose?
	// Then we might splits.
	
	// Simplified Approach (User Requirement: "report the copy at the highest level common ancestor")
	// If /a/b and /c/d are duplicates. Report them.
	// If /a/b/x and /c/d/x are duplicates. Don't report them IF /a/b + /c/d reported.
	
	
	// We need to iterate "biggest structures" first? Or Top Down?
	// If we process /a/b (Dir) and mark it as Dupe, we can mark all its children as Covered.
	// But /a/b is only a dupe if found elsewhere.
	
	// Let's refine:
	// 1. Collect all Nodes that are part of a duplicate set (Hash count > 1).
	// 2. Filter by MinSize.
	// 3. Iterate these "Candidate Nodes" in Top-Down order (Breadth First or Depth First?).
	//    Breadth First (Shallower paths first).
	//    If we encounter a Candidate Node that is NOT covered:
	//       - Create a Group for its Hash (if not created yet?).
	//       - Wait, Group creation depends on Hash.
	//       - If we process Node A (Hash H), we find all other nodes with Hash H.
	//       - Mark A and its peers as processed.
	//       - Mark all children of A and peers as "Covered" (suppress their future groups).
	
	candidates := make(map[string][]*Node) // Hash -> Nodes
	
	for h, nodes := range hashIndex {
		if len(nodes) < 2 { continue }
		// Check size of one (all same size)
		if nodes[0].Size < minSize { continue }
		candidates[h] = nodes
	}
	
	// Traverse tree Top-Down.
	// If Node is in Candidates:
	//    If Node is Covered -> Skip.
	//    If Node Not Covered ->
	//        Report Group (Hash).
	//        Mark all nodes in this group as Covered.
	//        Mark all descendents of these nodes as Covered.
	
	covered := make(map[*Node]bool)
	reportedHashes := make(map[string]bool)
	
	var traverse func(n *Node)
	traverse = func(n *Node) {
		if covered[n] {
			// Don't skip traversal? If n is covered, its children are covered too.
			// So yes, skip traversal of subtree.
			return
		}
		
		isCandidate := false
		if n.Hash != "" {
			if _, ok := candidates[n.Hash]; ok {
				isCandidate = true
			}
		}
		
		if isCandidate && !reportedHashes[n.Hash] {
			// Found a new highest-level duplicate group!
			groupNodes := candidates[n.Hash]
			
			// Build Group
			var paths []string
			for _, gn := range groupNodes {
				paths = append(paths, gn.Path)
				markCovered(gn, covered)
			}
			sort.Strings(paths)
			
			groups = append(groups, DuplicateGroup{
				Hash:      n.Hash,
				Size:      n.Size,
				Paths:     paths,
				IsDir:     !n.IsFile,
				ItemCount: countItems(n),
			})
			
			reportedHashes[n.Hash] = true
			
			// Since we marked n and its peers/children covered, we don't recurse.
			return
		}
		
		// If not a candidate, or hash already reported (which implies n was not covered when hash reported? impossible if logic sound), recurse
		// If hash already reported, it means 'n' is part of a group we just handled?
		// No, 'reportedHashes' is global.
		// If n.Hash is reported, then n SHOULD be covered.
		// So checking covered first handles it.
		
		for _, child := range n.Children {
			traverse(child)
		}
	}
	
	// Use sorted children traversal for determinism
	// Actually traverse helper needs to order children
	// We can reuse a deterministic walker
	
	walkDeterministic(root, traverse)
	
	return groups, nil
}

func insertNode(root *Node, path string, rec models.FileRecord) {
	cleanPath := strings.TrimPrefix(path, "/")
	parts := strings.Split(cleanPath, "/")

	current := root
	for _, part := range parts {
		if part == "" { continue }
		if current.Children == nil {
			current.Children = make(map[string]*Node)
		}
		child, exists := current.Children[part]
		if !exists {
			child = &Node{
				Name:     part,
				Path:     "", 
				Children: make(map[string]*Node),
			}
			if current.Path == "" {
				child.Path = "/" + part
			} else if current.Path == "/" {
				child.Path = "/" + part
			} else {
				child.Path = current.Path + "/" + part
			}
			current.Children[part] = child
		}
		current = child
	}
	// Leaf
	current.IsFile = true
	current.Size = rec.SizeBytes
	// Hash Priority: SHA1 -> MD5
	if rec.SHA1 != "" {
		current.Hash = rec.SHA1
	} else if rec.MD5 != "" {
		current.Hash = rec.MD5
	}
}

func computeMetadata(n *Node) {
	if n.IsFile {
		return
	}
	
	// Dir: Size = sum of children sizes. Hash = Merkle.
	var size int64 = 0
	var hashes []string
	
	for _, child := range n.Children {
		computeMetadata(child)
		size += child.Size
		if child.Hash != "" {
			// Hash input: Name + Hash (structure sensitive)
			hashes = append(hashes, child.Name+":"+child.Hash)
		}
	}
	n.Size = size
	
	if len(hashes) > 0 {
		sort.Strings(hashes) // Sort for stability
		n.Hash = strings.Join(hashes, ",") // Simple merkle
	}
}

func indexNodes(n *Node, index map[string][]*Node) {
	if n.Hash != "" {
		index[n.Hash] = append(index[n.Hash], n)
	}
	for _, child := range n.Children {
		indexNodes(child, index)
	}
}

func markCovered(n *Node, covered map[*Node]bool) {
	if covered[n] { return }
	covered[n] = true
	for _, child := range n.Children {
		markCovered(child, covered)
	}
}

func countItems(n *Node) int {
	if n.IsFile {
		return 1
	}
	c := 0
	for _, child := range n.Children {
		c += countItems(child)
	}
	return c
}

func walkDeterministic(n *Node, visit func(*Node)) {
	visit(n)
	// Sort children
	children := make([]*Node, 0, len(n.Children))
	for _, c := range n.Children {
		children = append(children, c)
	}
	sort.Slice(children, func(i, j int) bool {
		return children[i].Name < children[j].Name
	})
	
	for _, c := range children {
		walkDeterministic(c, visit)
	}
}
