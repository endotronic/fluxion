# The diff algorithm

`internal/diff/diff.go` (~880 lines) is the most complex and highest-risk code in the
project. This document explains what it does, why, and where it is currently wrong.

Read this before touching `internal/diff`, `internal/app/diff.go`, or anything that
changes what `diff` reports.

## What the diff is trying to do

Not "which paths differ" — **"what happened to my files"**. A reorganised tree must not
read as 40,000 deletions plus 40,000 additions. So the algorithm is:

1. content-addressed (a file is identified by its hash, not its path),
2. tree-structured (so whole directories can collapse to one line),
3. relocation-aware (moves and copies are first-class outcomes).

Everything below serves those three properties, in that order of importance.

## The data model

```go
type Node struct {
    Name     string            // basename
    Path     string            // "/a/b/c", relative to the snapshot root, leading slash
    IsFile   bool
    Status   Status
    Children map[string]*Node

    HashA, HashB string        // leaf: file hash. dir: synthetic merkle string (below)
    SizeA, SizeB int64
    SourcePath   string        // set only for Move/Copy
}
```

**One tree holds both snapshots.** There is no "tree A" and "tree B" — `insertNode` is
called for every file of A and then every file of B into the *same* root. A node whose
`HashA != "" && HashB != ""` existed in both; `HashA == ""` means new in B; `HashB == ""`
means gone in B. This is what makes move detection cheap: both sides are already in one
addressable structure.

**Statuses** (`Status` is a plain string type):

| Status | Meaning |
|---|---|
| `Unchanged` | present in both, same hash |
| `Added` | in B only |
| `Removed` | in A only |
| `Modified` | in both, different hash |
| `Mixed` | directory only: children disagree, do not collapse — recurse |
| `Move` | in B, content matched a `Removed` node in A; `SourcePath` set |
| `Copy` | in B, content matched a node still present in A; `SourcePath` set |
| `MovedSource` | internal: the A-side of a `Move`. Never printed. |

`Mixed` is not a user-visible outcome so much as a *control-flow* signal to
`collectResults`: "this directory cannot be summarised in one line."

## The pipeline

`CompareSnapshots(iterA, iterB, rootA, rootB, hashType, noCopies, noMoves, showUnchanged, onProgress)`

| # | Stage | Function |
|---|---|---|
| 1 | insert all of A (every leaf starts as `Removed`) | `insertNode` |
| 2 | insert all of B (leaf becomes `Unchanged`/`Modified`/`Added`) | `insertNode` |
| 3 | compute directory hashes bottom-up | `computeMerkleHashes` |
| 4 | roll child statuses up into directories (**pass 1**) | `propagateStatus` |
| 5 | match added/modified content against removed/existing | `detectMovesCopies` |
| 6 | roll up again, now that moves/copies exist (**pass 2**) | `propagateStatus` |
| 7 | walk the tree emitting collapsed results | `collectResults` |
| 8 | turn relative node paths back into absolute paths | inline in `CompareSnapshots` |

Two `propagateStatus` passes are required: `detectMovesCopies` needs directory hashes and
per-node statuses to exist (so it can index `Removed` nodes and match whole directories),
but it also *changes* statuses, which then have to re-roll into the parents. This is the
single most important structural fact about the algorithm.

### Why paths are relative

`app/diff.go` strips each snapshot's `root_path` before yielding to the iterators, and
step 8 re-attaches `rootA`/`rootB`. That is what lets the same tree scanned at
`/mnt/backup1` and at `/mnt/backup2` diff clean — the property `scripts/verify.sh` guards
with its "Relative Path Move" check. Step 8's choice of which root to re-attach is
status-dependent: `Removed`/`Modified` resolve against `rootA`, everything else against
`rootB`, and `SourcePath` always resolves against `rootA`.

## Stage 3: the "merkle" hashes — read this carefully

Directory hashes are **not cryptographic digests**. `computeMerkleHashes` builds a
concatenated string:

```go
for _, child := range node.Children {
    computeMerkleHashes(child)                                  // post-order
    if child.HashA != "" { hashesA = append(hashesA, child.Name+":"+child.HashA) }
    if child.HashB != "" { hashesB = append(hashesB, child.Name+":"+child.HashB) }
}
sort.Strings(hashesA)
node.HashA = strings.Join(hashesA, ",")
```

So a directory containing `a`(hash `AA`) and `b`(hash `BB`) gets the *literal string*
`"a:AA,b:BB"`. Sorting makes it order-independent, which is correct and necessary.

It works — equal content produces equal strings — but it has two real defects:

**1. It is not injective (collision).** `:` and `,` are legal filename characters and are
not escaped. A directory containing one file literally named `a:AA,b` with hash `BB`
produces `"a:AA,b:BB"` — byte-identical to the two-file directory above. Two structurally
different directories then compare equal, and one can be reported as a move/copy of the
other. Verified experimentally. Exotic, but it is a *false-unchanged* class bug, which
[goals.md](goals.md) ranks as top severity.

**2. It is O(total subtree bytes) per node.** The root's hash string contains every file's
hash. Measured on a synthetic depth-5 / 4096-file tree: **1,336,663 bytes** of `HashA`
across the tree, largest single directory string **196,603 bytes** — roughly 326 B/file,
growing with depth. On a real multi-million-file tree the hash strings alone dominate
memory, and every map insert and comparison in `detectMovesCopies` hashes those long
strings.

Both are fixed by the same one-line-ish change: hash the sorted, length-delimited child
list with SHA-1 and store the digest. That makes directory hashes fixed-width (20 bytes)
and injective. It changes no semantics — nothing outside this package interprets a
directory hash. **This is the single highest-value change available in this package.**

## Stage 4/6: `propagateStatus` — the rollup rules

Signature: `func propagateStatus(node *Node) (Status, bool)`. The bool is
`hasUnchangedContent` — whether the subtree contains anything unchanged. It is the brake
on collapsing: **if a directory contains unchanged content, it must not be collapsed into
a single status line**, because doing so would claim things about files that did not
change.

Order of decisions (first match wins):

1. `IsFile` → return the leaf's own status. *(Also the source of the file↔dir bug below.)*
2. `SourcePath != ""` → already decided as Move/Copy by stage 5; keep it, report no
   unchanged content.
3. no children → `Unchanged`.
4. all children `Unchanged` → `Unchanged`.
5. all children `MovedSource` → `MovedSource` (the whole directory moved away).
6. all children `Removed` or `MovedSource` → `Removed`.
7. **new directory** (`HashA == ""`) with only added-like children → `Added` if pure
   additions, or `Added` if `changeCount > 1`; a *single* move/copy falls through so the
   detail is shown.
8. Otherwise compute:
   ```go
   canRollup := !hasUnchangedContent && (allAddedLike || changeCount >= 2)
   ```
   and if it holds, pick a summary status by preference:
   - any `Modified` child → `Modified`
   - all added-like, but **more than one kind** of add-like (Move + Copy, etc.) →
     `Modified` (deliberately refuses to pick a winner)
   - else `Move` > `Added` > `Copy`, where a lone `Move` or lone `Copy`
     (`changeCount == 1`) degrades to `Mixed` so the child is shown individually
   - rolled-up `Move`/`Copy` inherit `SourcePath` from the *first* such child
   - fallthrough: `HashA == ""` → `Added`, else `Modified`
9. anything else → `Mixed` (recurse and show children).

`changeCount >= 2` as a rollup trigger is the rule most likely to surprise: a directory
with a single change and no unchanged content still reports that one change directly
(good), but two unrelated changes summarise as one `Modified` line for the parent.

**These rules read as reverse-engineered from individual test cases, not derived from a
stated principle** — several branches carry comments naming the test that motivated them
(`"per Rollup_Added test"`, `"CRITICAL FIX"`). Before refactoring, the intended semantics
need to be restated from scratch and the tests rewritten against them, not the other way
around.

## Stage 5: `detectMovesCopies`

Two passes over the tree.

**Index pass** — for every node with `HashA != ""`:
- `Status == Removed` → `removedMap[hash]` (move sources)
- `Status == Modified` → `modifiedMap[hash]` (swap/rewrite sources)
- `Status != Added` → `existingMap[hash]` (copy sources; includes unchanged nodes)

Zero-byte **files** are excluded from indexing — every empty file shares a hash, so
without this every empty file would "move" to every other. Directories are not excluded.

**Match pass** — for every node with `Status == Added` or `Modified`, keyed on `HashB`
(zero-byte files skipped again):
1. `removedMap` hit → `Move`, mark the source node `MovedSource`, and **consume** the
   entry (`paths[1:]`), so N removals can absorb at most N moves.
2. else, if the node itself is `Modified`, a `modifiedMap` hit → `Move` (the file-swap
   case), also consumed.
3. else `modifiedMap`, then `existingMap` → `Copy`. Copies are **not consumed** — one
   source can father many copies, which is correct.

Because directories carry hashes too, a whole directory can match as a single `Move`, and
a directory `Move` carries a trailing `/` on its `SourcePath`.

`--no-moves` / `--no-copies` gate the two halves; if both are set the whole stage returns
immediately.

### Confirmed defect: nondeterministic source attribution

The index closure iterates `n.Children` — **a Go map, in randomised order** — while the
match closure explicitly sorts children before recursing. So when several candidate
sources share a hash, `paths[0]` is whichever the randomised walk happened to append
first. Measured: 200 runs over identical input reported four different source paths
(133 / 27 / 18 / 22). Two runs of `fluxion diff` on the same database can print different
`(from ...)` sources.

Fix: sort children in `index` exactly as `match` does, or sort each `map[string][]string`
bucket after indexing. The latter is cheaper and also gives a stable, explainable rule
("lexicographically first source wins").

## Stage 7: `collectResults`

A pre-order walk with sorted sibling names, emitting at most one `DiffResult` per subtree:

- `Unchanged` or `MovedSource` → emit nothing, stop.
- `Added`/`Removed`/`Modified`/`Move`/`Copy` → **emit and stop recursing.** The line
  represents the entire subtree; `accumulateStats` walks the subtree to fill in the
  per-kind counts shown in parentheses. Directory paths get a trailing `/`.
- `Mixed` → emit nothing for the node itself, recurse into sorted children. With
  `--show-unchanged`, a trailing context row carrying only the unchanged counts is emitted
  *after* the children (post-order), which is why unchanged-context lines appear below the
  changes they contextualise.

`accumulateStats` counts **files** for changed statuses but counts **directories** for
`UnchangedDirCount` — an asymmetry that is intentional (so output can say "3 directories,
8 files unchanged") but is not obvious from the code.

## Confirmed bugs (with reproductions)

All of the following were reproduced with tests in `internal/diff` — no database and no
cgo needed, see [build.md](build.md). Full details in
[known-issues.md](known-issues.md).

### 1. File → directory transition silently loses the added files

`x` is a file in A; in B, `x` is a directory containing `y` and `z`.
Result: exactly one line, `Removed /r/x`. **`y` and `z` never appear.**

Cause: `insertNode` sets `current.IsFile = true` on whichever node is the leaf, so A's
insert marks `x` as a file. Both `computeMerkleHashes` and `propagateStatus` return
immediately for `IsFile` nodes, so B's children — which do exist in the tree, hanging off
`x` — are never visited.

### 2. Directory → file transition silently loses the removed files (worse)

`x/y` and `x/z` in A; `x` is a plain file in B.
Result: exactly one line, `Added /r/x`. **The two removals vanish** — including from
`--update` mode, which exists precisely to answer "is it safe to delete?". This is the
most severe known bug in the project by the severity rule in [goals.md](goals.md).

`IsFile` is a single flag that cannot express "file in A, directory in B". Any real fix
needs per-side kind tracking (`IsFileA` / `IsFileB`) and an explicit type-change status,
plus a decision about how such a node should be *reported*.

### 3. Nondeterministic move/copy sources

See stage 5 above.

### 4. Merkle collisions and merkle bloat

See stage 3 above.

### 5. Memory

Two identical 200,000-file snapshots: **200 MiB retained heap**, 398 MiB total allocated —
about 1 KiB per file, most of it merkle strings and `Node` overhead. ROADMAP 0.8.11 claims
"memory use optimization for diff"; that optimisation was on the *store* side (streaming
iterators), and the tree itself is still fully materialised. Fixing the merkle scheme
(stage 3) is also the largest single win here.

## Testing notes

`internal/diff` is pure and needs no database:

```go
// internal/diff/test_helper.go
mapToIter(map[string]models.FileRecord{...})
```

Existing tests: `diff_test.go` (main matrix), `diff_extra_test.go`,
`diff_zero_byte_test.go`, `diff_sort_test.go`. They are thorough about the *cases that
were designed*, which is exactly why they miss the file↔directory transitions — nobody
wrote that case. When adding tests here, prefer property-style checks over golden output:
the invariant worth asserting is **"every file in A that is absent from B appears in the
output under some status"**, which is the invariant all three data-loss bugs violate.
