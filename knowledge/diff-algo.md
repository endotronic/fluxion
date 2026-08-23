# The diff algorithm

`internal/diff/diff.go` (~1190 lines) is the most complex and highest-risk code in the
project. This document explains what it does, why, and where it is still wrong.

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

    InA, InB   bool            // recorded as a regular *file* on that side
    DirA, DirB bool            // something at or under this path existed on that side

    HashA, HashB string        // leaf: file hash. dir: synthetic merkle string (below)
    SizeA, SizeB int64

    FileTwin   *Node           // the file half of a path that is a dir on the other side
    SourcePath string          // set only for Move/Copy

    matched  bool              // Move/Copy came from content matching, not a rollup
    moveDest *Node             // on a MovedSource: where the content went
    visible  bool              // collectResults will emit a line for this node
}
```

**One tree holds both snapshots.** There is no "tree A" and "tree B" — `insertNode` is
called for every file of A and then every file of B into the *same* root. That is what
makes move detection cheap: both sides are already in one addressable structure.

**Presence is tracked explicitly, not inferred from the hash.** `InA`/`InB` say the path
was a regular file on that side; `DirA`/`DirB` say something at or under it existed.
Earlier versions asked `HashA == ""` instead, which is a different question: a record can
legitimately carry no hash *of the type being compared* — an MD5-only legacy import merged
into a SHA-1 snapshot is the reachable case — and every one of those files then read as
absent from A. `presentInA()` / `presentInB()` combine the flags with the twin's.

**Statuses** (`Status` is a plain string type):

| Status | Meaning |
|---|---|
| `Unchanged` | present in both, same hash |
| `Added` | in B only |
| `Removed` | in A only |
| `Modified` | in both, different hash — or present in both with a hash missing on one side |
| `Mixed` | directory only: children disagree, do not collapse — recurse |
| `Move` | in B, content matched a `Removed` node in A; `SourcePath` set |
| `Copy` | in B, content matched a node still present in A; `SourcePath` set |
| `MovedSource` | the A-side of a `Move`. Suppressed *if* the output names it (stage 9) |

`Mixed` is not a user-visible outcome so much as a *control-flow* signal to
`collectResults`: "this directory cannot be summarised in one line."

Note the `Modified` rule for missing hashes: two records that both exist but where one
carries no comparable hash are reported `Modified`, never `Unchanged`. Over-reporting
costs reading time; a false "unchanged" can cost the user the file
([goals.md](goals.md)).

### `FileTwin`: a path that is a file on one side and a directory on the other

No filesystem holds both at once, but two snapshots taken months apart disagree about
plenty. `IsFile` is one flag and cannot say "file in A, directory in B", and every
consumer tested it first — so whichever half lost the coin toss vanished from the diff
entirely. That was the worst class of bug in the project.

`splitFileDirCollisions` (stage 3) resolves it structurally: any node that is both
`IsFile` and a parent of children hands its file aspect to a newly allocated `FileTwin`
and keeps only the directory aspect for itself. The twin shares the node's `Path`, carries
its own status, and is matched, propagated and emitted alongside its host — so such a path
produces **two lines**, one for each aspect. If the two aspects disagree, `propagateStatus`
forces the host to `Mixed` so no ancestor can collapse over it and hide one of them.

## The pipeline

`CompareSnapshots(iterA, iterB, rootA, rootB, hashType, noCopies, noMoves, showUnchanged, onProgress)`

| # | Stage | Function |
|---|---|---|
| 1 | insert all of A | `insertNode` |
| 2 | insert all of B (leaf status set by `fileStatus`) | `insertNode` |
| 3 | split file/directory collisions into host + twin | `splitFileDirCollisions` |
| 4 | compute directory hashes and `DirA`/`DirB` bottom-up | `computeMerkleHashes` |
| 5 | roll child statuses up into directories (**pass 1**) | `propagateStatus` |
| 6 | match added/modified content against removed/existing | `detectMovesCopies` |
| 7 | roll up again, now that moves/copies exist (**pass 2**) | `propagateStatus` |
| 8 | reinstate move sources the output would not mention, to a fixed point | `markVisible` → `reinstateHiddenMoveSources` → `propagateStatus` |
| 9 | walk the tree emitting collapsed results | `collectResults` |
| 10 | turn relative node paths back into absolute paths | inline in `CompareSnapshots` |

Two `propagateStatus` passes are required: `detectMovesCopies` needs directory hashes and
per-node statuses to exist (so it can index `Removed` nodes and match whole directories),
but it also *changes* statuses, which then have to re-roll into the parents. This is the
single most important structural fact about the algorithm.

Stage 8 is a loop, described under its own heading below.

### Why paths are relative

`app/diff.go` strips each snapshot's `root_path` before yielding to the iterators, and
step 10 re-attaches `rootA`/`rootB`. That is what lets the same tree scanned at
`/mnt/backup1` and at `/mnt/backup2` diff clean — the property `scripts/verify.sh` guards
with its "Relative Path Move" check. Step 10's choice of which root to re-attach is
status-dependent: `Removed`/`Modified` resolve against `rootA`, everything else against
`rootB`, and `SourcePath` always resolves against `rootA`.

## Stage 4: the "merkle" hashes — read this carefully

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
`"a:AA,b:BB"`. Sorting makes it order-independent, which is correct and necessary. A
`FileTwin` contributes a second entry tagged `name:file:hash`, so a directory and a file
of the same name cannot produce the same entry. The same walk sets `DirA`/`DirB` from the
children's `presentInA()`/`presentInB()`.

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

## Stages 5/7/8: `propagateStatus` — the rollup rules

Signature: `func propagateStatus(node *Node) (Status, bool)`. It resolves the node's
directory aspect via `propagateNodeStatus` and then reconciles it with the `FileTwin`, if
any: an unchanged twin just contributes "has unchanged content", and a twin that disagrees
with the directory forces `Mixed`.

The bool is `hasUnchangedContent` — whether the subtree contains anything unchanged. It is
the brake on collapsing: **if a directory contains unchanged content, it must not be
collapsed into a single status line**, because doing so would claim things about files
that did not change.

Order of decisions in `propagateNodeStatus` (first match wins):

1. `IsFile` → return the leaf's own status.
2. `matched` → a Move/Copy established by *content matching* in stage 6 is a fact about
   the data and is kept as-is. A rolled-up Move/Copy is only an inference and must stay
   recomputable — freezing on `SourcePath != ""` instead (as an earlier version did) left
   directories stuck at a stale summary that later passes could not correct. Recomputing
   clears any inferred `SourcePath` so it cannot outlive its reasoning.
3. no children → `Unchanged`.
4. all children `Unchanged` → `Unchanged`.
5. all children `MovedSource` → `MovedSource` (the whole directory moved away).
6. all children `Removed` or `MovedSource` → `Removed`.
7. **directory absent from A** (`!DirA`) with only added-like children → `Added` if pure
   additions, or `Added` if `changeCount > 1`; a *single* move/copy falls through so the
   detail is shown.
8. Otherwise compute:
   ```go
   canRollup := !hasUnchangedContent && (allAddedLike || changeCount >= 2)
   ```
   and if it holds, pick a summary status by preference:
   - any `Modified` child → `Modified`
   - all added-like **and no `MovedSource` child** — see below — but **more than one kind**
     of add-like (Move + Copy, etc.) → `Modified` (deliberately refuses to pick a winner)
   - else `Move` > `Added` > `Copy`, where a lone `Move` or lone `Copy`
     (`changeCount == 1`) degrades to `Mixed` so the child is shown individually
   - rolled-up `Move`/`Copy` inherit `SourcePath` from the *first* such child
   - fallthrough: `!DirA` → `Added`, else `Modified`
9. anything else → `Mixed` (recurse and show children).

`allAddedLike` counts `MovedSource` as added-like so that a directory emptied by a move
still rolls up (rule 5). But content that *left* a directory is not content arriving in
it: summarising a directory that holds a `MovedSource` as `Added`, `Move` or `Copy` would
describe only what came and silently drop what went. Rule 8 therefore requires
`!hasMovedSource` before taking any of those, and otherwise falls through to `Modified`
(or `Mixed`, when detail is available) so the loss stays on screen. A property-test seed
found exactly this: `Added d/` printed for a directory whose old contents had moved out.

`changeCount >= 2` as a rollup trigger is the rule most likely to surprise: a directory
with a single change and no unchanged content still reports that one change directly
(good), but two unrelated changes summarise as one `Modified` line for the parent.

**These rules read as reverse-engineered from individual test cases, not derived from a
stated principle** — several branches carry comments naming the test that motivated them
(`"per Rollup_Added test"`, `"CRITICAL FIX"`). They are now fenced in by the property test
below, which is what makes restating them from scratch a tractable job rather than a
gamble.

## Stage 6: `detectMovesCopies`

Two passes over the tree. Both walk children through `sortedChildren`, so everything here
is deterministic.

The maps are `map[string][]*Node` — **nodes, not paths**. A path that is a file on one
side and a directory on the other exists twice in the tree (host and twin), so resolving a
source by path alone could mark the wrong half as the origin of a move.

**Index pass** — every node reachable while `presentInA()` holds, plus its twin:
- `Status == Removed` → `removedMap[hash]` (move sources)
- `Status == Modified` → `modifiedMap[hash]` (swap/rewrite sources)
- `Status != Added` → `existingMap[hash]` (copy sources; includes unchanged nodes)

Zero-byte **files** are excluded from indexing — every empty file shares a hash, so
without this every empty file would "move" to every other. Directories are not excluded.

**Match pass** — for every node (and twin) with `Status == Added` or `Modified`, keyed on
`HashB` (zero-byte files skipped again):
1. `removedMap` hit → `Move`, mark the source node `MovedSource`, set its `moveDest`, and
   **consume** the entry, so N removals can absorb at most N moves.
2. else, if the node itself is `Modified`, a `modifiedMap` hit → `Move` (the file-swap
   case), also consumed.
3. else `modifiedMap`, then `existingMap` → `Copy`. Copies are **not consumed** — one
   source can father many copies, which is correct.

A successful match sets `matched = true`, which is what stage 5/7 honours.

Because directories carry hashes too, a whole directory can match as a single `Move`, and
a directory `Move` carries a trailing `/` on its `SourcePath`.

**A directory that already held something in A is never matched.** `matchOne` refuses when
`!n.IsFile && n.presentInA()`. "This directory is a copy of that one" describes only what
the directory holds *now*; because the node then stops being recomputed, whatever it used
to hold and has since lost would never be examined, let alone reported. Two property-test
seeds found real data loss through this path.

`--no-moves` / `--no-copies` gate the two halves; if both are set the whole stage returns
immediately.

## Stage 8: visibility and hidden move sources

A `MovedSource` is normally suppressed, on the assumption that the destination's line
names it (`Move new/x <- old/x`). That assumption breaks when the destination line is
itself collapsed into an ancestor summary: the source is then mentioned **nowhere**, and a
file that is gone from B reads as untouched. This is the false-unchanged failure that
[goals.md](goals.md) ranks worst, and it is what the property test found first.

The stage is a loop, because reinstating a source changes what its ancestors should say:

```go
for i := 0; i < 8; i++ {
    markVisible(root, true)                       // which nodes will actually print
    named := make(map[string]bool)
    namedMoveSources(root, named)                 // origins those lines will name
    if !reinstateHiddenMoveSources(root, named, false) { break }
    propagateStatus(root)                         // ancestors re-summarise
}
```

- `markVisible` mirrors `collectResults`: recursion continues only through the virtual
  root and through `Mixed` nodes, so a node is `visible` exactly when a line is emitted at
  its own path.
- `namedMoveSources` collects the `SourcePath` of every **visible** `Move` line.
- A `MovedSource` is left alone if it is covered, and demoted to `Removed` if not. It is
  covered when either its path or an ancestor of its path is named as an origin — "Move
  new/ <- old/" accounts for everything that was under `old/` — or when a visible
  ancestor line already reports loss (`Removed` or `Modified`), whose counts include it.
  The virtual root is excluded from that second rule: it never prints, so it covers
  nothing. When a node is covered, its whole subtree is, and recursion stops.
- Demotion applies to the entire subtree (`demoteMovedSources`). Leaving `MovedSource`
  descendants behind would let the next `propagateStatus` roll the node straight back to
  `MovedSource` and undo the fix.

Each pass only ever turns `MovedSource` into `Removed`, so the loop converges; the bound of
8 is a guard, not an expected limit.

## Stage 9: `collectResults`

A pre-order walk with sorted sibling names, emitting at most one `DiffResult` per subtree.
A node with a `FileTwin` emits both aspects, A-side first, so a path that changed kind
produces two adjacent lines.

- `Unchanged` or `MovedSource` → emit nothing, stop.
- `Added`/`Removed`/`Modified`/`Move`/`Copy` → **emit and stop recursing.** The line
  represents the entire subtree; `accumulateStats` walks the subtree (twins included) to
  fill in the per-kind counts shown in parentheses. Directory paths get a trailing `/`.
- `Mixed` → emit nothing for the node itself, recurse into sorted children. With
  `--show-unchanged`, a trailing context row carrying only the unchanged counts is emitted
  *after* the children (post-order), which is why unchanged-context lines appear below the
  changes they contextualise.

`accumulateStats` counts **files** for changed statuses but counts **directories** for
`UnchangedDirCount` — an asymmetry that is intentional (so output can say "3 directories,
8 files unchanged") but is not obvious from the code.

## Remaining known defects

Full details in [known-issues.md](known-issues.md).

### Merkle collisions and merkle bloat

See stage 4 above. Still open, and still the single highest-value change available in this
package: a real digest fixes a false-unchanged bug and the dominant memory cost together.

### Memory

Two identical 200,000-file snapshots: **200 MiB retained heap**, 398 MiB total allocated —
about 1 KiB per file, most of it merkle strings and `Node` overhead. ROADMAP 0.8.11 claims
"memory use optimization for diff"; that optimisation was on the *store* side (streaming
iterators), and the tree itself is still fully materialised. Fixing the merkle scheme is
also the largest single win here.

## Testing notes

`internal/diff` is pure and needs no database:

```go
// internal/diff/test_helper.go
mapToIter(map[string]models.FileRecord{...})
```

Tests: `property_test.go` (start here), `diff_test.go` (main matrix),
`diff_extra_test.go`, `diff_zero_byte_test.go`, `diff_sort_test.go`.

The golden tests are thorough about the *cases that were designed*, which is exactly why
they missed the file↔directory transitions — nobody wrote that case. `property_test.go`
exists to catch what nobody thought to write down. It asserts two invariants over
hand-built scenarios and over generated tree pairs:

- **completeness** — every file that differs between A and B is accounted for somewhere in
  the output, either by its own line or by an ancestor line whose status can legitimately
  subsume it (the `collapsing` map encodes which status covers which kind, and it is
  direction-sensitive: an `Added` directory line does not account for a *removed* file);
- **soundness** — a collapsed `Added` or `Removed` directory line must not contradict the
  snapshots (nothing under an `Added` directory existed in A).

It found five distinct data-loss bugs on first run — 200 of the first 300 generated trees
lost files — and every one of them was real. Failures print the seed and the whole diff.
`randomTreeSeeds` is 5000 for routine runs; raise it by hand after changing the engine
(400000 takes about 20 seconds and is currently clean).

**When you change this package, run the property test before the golden tests.** A golden
test tells you that output changed; the property test tells you whether the change loses
data.
