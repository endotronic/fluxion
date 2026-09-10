package diff

import (
	"fmt"
	"runtime"
	"testing"

	"fluxion/internal/models"
)

// TestMemory_UnifiedTree measures what a diff of 200,000 files per side costs
// in retained heap, the figure knowledge/diff-memory.md tracks and the reason
// `diff` is unusable at fleet scale.
//
// It measures the **tree with the tree still alive**, not the heap after
// CompareSnapshots returns. Two reasons. The tree is the entire cost - a
// stage-by-stage probe showed split/merkle/propagate/detectMovesCopies/collect
// each add 0.0 MiB on top of it, their indexes being transient - so the tree is
// the honest proxy for the peak. And reading HeapAlloc straight after a run
// without collecting first measures uncollected garbage rather than retained
// memory, which is noisy enough to report a real improvement as a regression
// (it did, during the change that added the deferred-Children optimisation).
func TestMemory_UnifiedTree(t *testing.T) {
	const n = 200_000
	files := make(map[string]models.FileRecord, n)
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("/dir%d/subdir%d/file%d.txt", i%50, i%500, i)
		files[path] = models.FileRecord{
			Path: path,
			SHA1: fmt.Sprintf("%040x", i), // distinct, well-formed hex, like a real SHA-1
		}
	}

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	root := &Node{Name: "", Status: StatusUnchanged}
	if err := mergeJoinInsert(root, mapToIter(files), mapToIter(files), "sha1", nil); err != nil {
		t.Fatalf("mergeJoinInsert: %v", err)
	}

	var after runtime.MemStats
	runtime.GC() // collect the build's garbage; keep only what the tree retains
	runtime.ReadMemStats(&after)

	nodes := int64(countTreeNodes(root))
	retained := int64(after.HeapAlloc - before.HeapAlloc)
	retainedMiB := float64(retained) / (1 << 20)
	perNode := float64(retained) / float64(nodes)

	t.Logf("unified tree for %d files/side: %.1f MiB retained across %d nodes = %.0f B/node",
		n, retainedMiB, nodes, perNode)

	// History, so a regression is legible rather than just a number going up:
	//   ~1 KiB/node   before the fixed-width digest (diff-memory.md Phase 0)
	//   322 B/node    after it
	//   274 B/node    after Children stopped being pre-allocated on leaves
	//   226 B/node    after Path was replaced by a parent pointer + path()
	//   210 B/node    after Status became a uint8 instead of a string
	const ceilingPerNode = 230.0
	if perNode > ceilingPerNode {
		t.Errorf("tree costs %.0f B/node, over the %.0f B ceiling - a memory optimisation in "+
			"knowledge/diff-memory.md has regressed", perNode, ceilingPerNode)
	}

	runtime.KeepAlive(root)
	runtime.KeepAlive(files)
}

func countTreeNodes(n *Node) int {
	c := 1
	if n.FileTwin != nil {
		c++
	}
	for _, ch := range n.Children {
		c += countTreeNodes(ch)
	}
	return c
}
