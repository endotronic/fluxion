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
    Parent   *Node             // path() walks this; the path is NOT stored per node
    IsFile   bool
    Status   Status            // uint8, not a string
    Children map[string]*Node  // nil on leaves, allocated on first insert

    InA, InB   bool            // recorded as a regular *file* on that side
    DirA, DirB bool            // something at or under this path existed on that side

    HashA, HashB hashVal       // leaf: file hash. dir: merkle digest (below). Inline,
                               // 20 bytes + a length; n == 0 means "no hash of this type"
    SizeA, SizeB int64

    FileTwin   *Node           // the file half of a path that is a dir on the other side
    SourcePath string          // set only for Move/Copy

    matched bool              // Move/Copy came from content matching, not a rollup
}
```

Every field above that looks unusual is there for memory: the tree is materialised in full,
so anything per-node is multiplied by the file count. `Sizeof(Node)` is 136 B and the whole
tree costs 178 B/node, down from ~1 KiB — [diff-memory.md](diff-memory.md) has the
change-by-change table. Two consequences worth knowing before editing:
**`node.path()` is computed, not stored** (cheap, but do not call it per-node in a hot
loop), and **`Children` is nil until a node has a child**, which is safe to range and read
but not to write without the guard `locateNode` already has.

**One tree holds both snapshots.** There is no "tree A" and "tree B" — `insertNode` is
called for every file of A and then every file of B into the *same* root. That is what
makes move detection cheap: both sides are already in one addressable structure.

**Presence is tracked explicitly, not inferred from the hash.** `InA`/`InB` say the path
was a regular file on that side; `DirA`/`DirB` say something at or under it existed.
Earlier versions asked `HashA == ""` instead, which is a different question: a record can
legitimately carry no hash *of the type being compared* — an MD5-only legacy import merged
into a SHA-1 snapshot is the reachable case — and every one of those files then read as
absent from A. `presentInA()` / `presentInB()` combine the flags with the twin's.

**Statuses** (`Status` is a `uint8` with a `String()` method, not a string type):

| Status | Meaning |
|---|---|
| `Unchanged` | present in both, same hash |
| `Added` | in B only |
| `Removed` | in A only |
| `Modified` | in both, different hash — or present in both with a hash missing on one side |
| `Mixed` | directory only: children disagree, do not collapse — recurse |
| `Move` | in B, content matched a `Removed` node in A; `SourcePath` set |
| `Copy` | in B, content matched a node still present in A; `SourcePath` set |
| `MovedSource` | the A-side of a `Move`. Suppressed *if* the output names it (stage 8) |
| `Truncated` | stands in for the lines a directory had no budget to print; carries their counts and `HiddenCount` |

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
and keeps only the directory aspect for itself. The twin shares the node's `Name` and
`Parent` (so `twin.path()` equals its host's), carries
its own status, and is matched, propagated and emitted alongside its host — so such a path
produces **two lines**, one for each aspect. If the two aspects disagree, `propagateStatus`
forces the host to `Mixed` so no ancestor can collapse over it and hide one of them.

## The pipeline

`CompareSnapshots(iterA, iterB FileIterator, opts Options)` — `Options` carries the two
roots, the hash type, the `--no-moves`/`--no-copies`/`--show-unchanged` switches, the line
budget `MaxLinesPerDir`, and the progress callback.

| # | Stage | Function |
|---|---|---|
| 1+2 | build the unified tree from both streams (leaf status set by `fileStatus`) | `mergeJoinInsert` (default) or `twoPassInsert`, chosen via the `treeBuilder` parameter of `compareSnapshotsWith` |
| 3 | split file/directory collisions into host + twin | `splitFileDirCollisions` |
| 4 | compute directory hashes and `DirA`/`DirB` bottom-up | `computeMerkleHashes` |
| 5 | roll child statuses up into directories (**pass 1**) | `propagateStatus` |
| 6 | match added/modified content against removed/existing | `detectMovesCopies` |
| 7 | roll up again, now that moves/copies exist (**pass 2**) | `propagateStatus` |
| 8 | collect a trial output, reinstate any move source it failed to mention, repeat to a fixed point | `collector.collect` → `accountedPaths` → `reinstateHiddenMoveSources` → `propagateStatus` |
| 9 | the last trial run *is* the output | `collector` |
| 10 | turn relative node paths back into absolute paths | inline in `CompareSnapshots` |

Stages 1 and 2 were two independent passes (all of A, then all of B) until 2026-09-09;
they are now one merge join over both streams, which locates a path present on both sides
once instead of twice. `twoPassInsert` is retained as the oracle the merge-join builder is
checked against — see [diff-memory.md](diff-memory.md)'s Phase 1 notes for why, and
`mergejoin_test.go` for the equivalence harness that later phases should extend.

Two `propagateStatus` passes are required: `detectMovesCopies` needs directory hashes and
per-node statuses to exist (so it can index `Removed` nodes and match whole directories),
but it also *changes* statuses, which then have to re-roll into the parents. This is the
single most important structural fact about the algorithm.

Stage 8 is a loop, described under its own heading below.

**There is a second engine, and it is the default.** `internal/diff/streaming.go` and
`internal/diff/external.go` compute the same output from a depth-first stream and a stack
instead of a materialised tree — stages 3-5 and 7-9 as one walk, stage 6 as an external
sort, stage 8 as repeated walks. `CompareSnapshots` uses it whenever the input arrives in
DFS-key order and falls back here otherwise. **Everything in this file is the
specification it is checked against**, byte for byte, so a change to any rule below has to
be made in the shared code (`rollup.go`) or in both engines — and the equivalence harness
in `streaming_test.go` is what tells you which. See [diff-memory.md](diff-memory.md).

### Why paths are relative

`app/diff.go` strips each snapshot's `root_path` before yielding to the iterators, and
step 10 re-attaches `rootA`/`rootB`. That is what lets the same tree scanned at
`/mnt/backup1` and at `/mnt/backup2` diff clean — the property `scripts/verify.sh` guards
with its "Relative Path Move" check. Step 10's choice of which root to re-attach is
status-dependent: `Removed`/`Modified` resolve against `rootA`, everything else against
`rootB`, and `SourcePath` always resolves against `rootA`.

## Stage 4: the "merkle" hashes — read this carefully

Directory hashes are **not cryptographic digests** of file content — they are a digest
*over the child list*, used to tell "these two directories hold the same names mapped to
the same hashes" apart from "they don't."

**Fixed 2026-09-09 (Phase 0 of [diff-memory.md](diff-memory.md)'s memory plan).**
`computeMerkleHashes` used to build a concatenated string — a directory containing `a`
(hash `AA`) and `b` (hash `BB`) got the literal string `"a:AA,b:BB"`, sorted for
order-independence and joined with `,`. It worked (equal content produced equal strings)
but had two real defects, both now fixed by `digestEntries` hashing the sorted,
length-prefixed child list with SHA-1 instead of concatenating it:

**1. It was not injective (collision) — now fixed.** `:` and `,` are legal filename
characters and were not escaped, so a directory containing one file literally named
`a:AA,b` with hash `BB` produced `"a:AA,b:BB"` — byte-identical to the two-file directory
above. Two structurally different directories then compared equal, and one could be
reported as a move/copy of the other. Verified experimentally at the time; a
*false-unchanged* class bug, top severity per [goals.md](goals.md). Explicit
length-prefixing (a 4-byte big-endian length before every name and every hash, plus a
1-byte host/twin tag) means no separator byte is ever interpreted as content, so this
class of collision can no longer occur.

**2. It was O(total subtree bytes) per node — now fixed.** The root's hash string used to
contain every file's hash. Measured on a synthetic depth-5 / 4096-file tree: 1,336,663
bytes of `HashA` across the tree, largest single directory string 196,603 bytes — roughly
326 B/file, growing with depth. `digestEntries` returns a fixed 20-byte SHA-1 digest
regardless of subtree size. Leaf hashes were also switched from 40/32-char hex text to raw
decoded bytes (`compactHash`, called once in `insertNode`) for the same reason at the leaf
level.

**Measured effect** (`TestMemory_TwoIdenticalSnapshots`, `internal/diff/memory_test.go`):
retained heap for two identical 200,000-file snapshots dropped from the ~200 MiB
[diff-memory.md](diff-memory.md) measured under the old scheme to **72.8 MiB** — a real
2.75x, short of that document's ~5x estimate (`Node` struct/map overhead untouched by this
change is a larger fraction of the remainder than the estimate assumed) but a genuine,
tested reduction with **zero semantic change**: nothing outside this package ever reads a
`HashA`/`HashB` value directly (`DiffResult` carries no hash field, confirmed by grep;
`app/diff.go` never touches `HashA`/`HashB`), so the digest's exact byte content doesn't
matter to anything except `==` comparison and map-keying, both unaffected by switching from
hex text to raw bytes or from a joined string to a digest. Validated against 400,000
property-test seeds (both the plain and line-budget-1 runs) with zero failures, plus the
full existing golden-test suite unchanged, before being reverted to the routine 5,000-seed
count — see [diff-memory.md](diff-memory.md) for what's next (Phases 1-5, the actual
`O(1)`-memory external-sort rewrite, which this change is a prerequisite for but does not
attempt).

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
   - rolled-up `Move`/`Copy` inherit `SourcePath` from the *first* such child — where
     "first" means **first by name**. `propagateNodeStatus` ranges `sortedChildren`, not
     `node.Children`, for exactly this reason: over a map, "first" meant "whichever the
     runtime handed us", and the same input named a different source on every run
     (fixed 2026-09-09; see `TestDeterminism_RepeatedRunsAgree` and
     [diff-memory.md](diff-memory.md)'s Phase 1 notes). Any new "pick one child" rule added
     here must iterate in sorted order too.
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
1. `removedMap` hit → `Move`, mark the source node `MovedSource`, and **consume** the
   entry, so N removals can absorb at most N moves.
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

**The streaming engine reaches the same verdicts without the tree.** `internal/diff/external.go`
replaces the three maps with one external sort on `(hash, ordinal)` and four forward-only
cursors per content group — the ordinal being this stage's own pre-order traversal, which is
what decides which candidate a move is attributed to. Everything above is the specification
it is checked against: the guards, the preference order, and which maps consume. See
[diff-memory.md](diff-memory.md)'s Phase 3, in particular the one shape a stream refuses to
guess at (a matched directory with a `FileTwin`).

## Stage 8: hidden move sources

A `MovedSource` is normally suppressed, on the assumption that the destination's line
names it (`Move new/x <- old/x`). That assumption breaks when the destination line is
itself collapsed into an ancestor summary, or dropped by the line budget: the source is
then mentioned **nowhere**, and a file that is gone from B reads as untouched. This is the
false-unchanged failure that [goals.md](goals.md) ranks worst, and it is what the property
test found first.

The stage does not try to *predict* what will print. It collects a trial output and asks
it, then repeats — because reinstating a source changes what its ancestors say, which
changes the output again:

```go
c := newCollector()
c.collect(root)

for i := 0; i < 32; i++ {
    if !reinstateHiddenMoveSources(root, accountedPaths(c.results)) { break }
    propagateStatus(root)
    c = newCollector()
    c.collect(root)
}
results := c.results          // the last trial run is the answer
```

- `accountedPaths` reads the trial results and returns the paths the output tells the
  reader something about: the `SourcePath` of every `Move` line, and the path of every
  `Removed` or `Modified` line (whose counts cover its whole subtree).
- **Truncation summaries are deliberately excluded.** A suppressed move source emits no
  line, so it contributes nothing to a summary's counts — being inside a truncated block
  is not being mentioned.
- A `MovedSource` is left alone if it, or a directory above it, is in that set — "Move
  new/ <- old/" accounts for everything that was under `old/`. Otherwise it is demoted to
  `Removed`. When a node is covered, its whole subtree is, and recursion stops.
- Demotion applies to the entire subtree (`demoteMovedSources`). Leaving `MovedSource`
  descendants behind would let the next `propagateStatus` roll the node straight back to
  `MovedSource` and undo the fix.

Each pass only ever turns `MovedSource` into `Removed`, so the loop converges; the bound of
32 is a guard, not an expected limit.

The streaming engine reaches the same fixed point by re-running the walk instead of
mutating a tree: each round merge-joins a larger set of demoted ordinals in by ordinal, and
the walk writes out the sources it suppressed so the round's output can be asked about them.
Two facts from this section become load-bearing there and are easy to lose: **the demotion
set is cumulative** (a demoted source emits a `Removed` line, which would otherwise make the
next round think it had been mentioned all along), and **the candidate is the highest
suppressed node, not each leaf**. See [diff-memory.md](diff-memory.md)'s Phase 3.

An earlier version predicted the output with a `markVisible` pass instead. It was wrong
twice over — once because a `Move` line collapsed into an enclosing `Move` still names the
source, and again because it knew nothing about the line budget. Deriving the answer from
real output is what keeps the two mechanisms in step. **Anything new that removes lines
from the output has to be added to the collector, not applied afterwards**, or this stage
will not see it.

## Stage 9: the `collector`

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

### The line budget

A directory holding unchanged content may not be collapsed — a single `Modified dir/` line
would claim something about files that did not change — so a directory where 20,000 files
changed and 20,000 did not prints 20,000 lines. `Options.MaxLinesPerDir` (CLI
`--max-lines`, default `DefaultMaxLinesPerDir` = 25, `0` = unlimited) caps what one
directory contributes: `applyBudget` keeps the first N lines and replaces the rest with one
`StatusTruncated` line carrying their combined counts and a `HiddenCount`.

This is the only mechanism in the engine that *removes* information rather than summarising
it in place, so two things are load-bearing:

- The summary is emitted at the directory's own path and carries the counts of everything
  it replaced, so no change is silently dropped — the reader is told how much more is there
  and of what kind, and `--max-lines 0` shows it all.
- **The virtual root is exempt.** The budget exists to stop one directory drowning the
  output, not to cap the run. A truncated directory always emits budget+1 lines, so a
  capped root would immediately re-truncate the summary it had just produced, collapsing
  the entire diff into a single line.

Truncation interacts with stage 8: dropping a `Move` line un-names its source. That is
handled by stage 8 running *after* collection rather than before it.

## Multi-source sides (`--from`/`--to`), and the overlap case that isn't built

Added 2026-09-13, in `internal/app/diffmulti.go` — above `internal/diff` entirely, not a
change to the engine. `CompareSnapshots` takes exactly two `FileIterator`s; what's new is
how a `FileIterator` for one side gets built, not the engine that consumes it.

**Why this exists.** The fleet's actual comparison (`knowledge/fleet.md`) is one legacy
baseline snapshot against a *union* of a fleet's many per-dataset `zfs-scan` snapshots —
`coverage` already does unions of keepers, but `diff` took exactly two snapshot IDs. The
naive fix is `merge` (`internal/app/merge.go`) into one physical snapshot, then diff that —
but `merge` still has to read every source *and* write every row back out before `diff` can
read it a second time, doubling the I/O for no reason: `diff` only ever needs to read a
side once anyway.

**First version, and the real-fleet bug that replaced it same-day.** The first cut sorted
sources by root path and *concatenated* their whole streams, reasoning that snapshots with
pairwise-disjoint roots (`rootsDisjoint`, checked up front) can't share a path, so root order
is already global path order. That reasoning has a hole a real ZFS fleet hits immediately:
`zfs-scan` scans each dataset with `--cross-mounts=false`, so a parent dataset's root
(`luna/kevin`) is a completely normal *string* prefix of a child dataset's root
(`luna/kevin/archives/2016-2020`) despite the two never sharing a single file — the child is
a separate mounted filesystem the parent's own scan never descended into. `rootsDisjoint`
can't tell that apart from a genuine overlap, so it refused the pair outright — making the
feature unusable for exactly the fleet hierarchy it exists for, discovered running it against
the author's actual data. Worse, concatenation would have produced wrong *output* even
without the false refusal: a parent's own files can sort anywhere relative to a child's
entire subtree, so emitting one source's whole block before another's can violate global
order with zero real collisions.

**The fix: a genuine k-way merge, and dropping the static check entirely.** There is no
cheap, static way to tell "nested names, disjoint content" from "nested names, colliding
content" — that depends on the data, not the root paths, so it can't be decided before
reading them. `multiSnapshotIter` now runs a real k-way merge: one pull cursor per source
(`pullIter`, mirroring `internal/diff`'s own — see `internal/diff/pulliter.go`'s doc comment
for why `iter.Pull` rather than a goroutine and channel), each source relativized to the
combined set's common ancestor (`findLCA`, the same function `merge` uses to name its
physical output), repeatedly taking whichever active cursor holds the lexicographically
smallest current path. This interleaves sources correctly regardless of how their roots
relate, string-wise, to each other. `rootsDisjoint` is no longer consulted here at all (it
remains correct and in use for `merge`'s own, different, question — see
`knowledge/known-issues.md` 3.5). When a side has exactly one source this still degenerates
to the plain case `diff` has always had (`singleSnapshotIter`, unchanged).

**"Smallest" by which order? A second real-fleet bug, found the same day.** A streamable
source is read via `IterateFilesDFS`, sorted by DFS key (`/` sorting below every other byte -
see Stage 2's streaming engine notes and `internal/diff/streaming.go`'s `dfsKey`), not plain
path order. The merge's "smallest current path" comparison has to use that same key or it
can pick the wrong winner: plain order says `a < a.txt < a/x`, DFS order says
`a < a/x < a.txt`, because `'\x01'` (what `/` becomes) sorts below `'.'`. Comparing
DFS-sorted sources with plain `<` produced exactly this on the real fleet - a "collision"
error naming a path that existed in only one snapshot, because the merge's own comparator
had walked out of the order its data actually satisfied. Fixed with a `key` function
(`dfsKey` when streamable, identity otherwise, matching whichever of `IterateFilesDFS` /
`IterateFiles` actually produced each source) used consistently for both picking the winner
and the monotonicity check below. `TestMultiSnapshotIter_UsesDFSOrderWhenStreamable` pins
the classic `a`/`a.txt`/`a/x` shape split across two sources — see
`knowledge/known-issues.md` 3.7.

**Refuse, don't guess, on the one thing that's actually left: a genuine collision.** The
only way this can still go wrong is two sources really producing the same relative path —
the last-input-wins case `merge` resolves with its collision map, which this does not
attempt. That surfaces naturally during the merge as two cursors tied for the next record;
`multiSnapshotIter` requires every taken path to sort strictly after the previous one across
the *whole* merge, so a tie (or, defensively, any other ordering violation) returns
`errMultiSourceOverlap` and the diff stops rather than picking a winner arbitrarily.
`TestMultiSnapshotIter_CatchesGenuineCollision` pins that; `TestRunDiff_AllowsNestedRootsWithDisjointContent`
pins the fixed bug itself — the exact parent/child dataset shape that used to be refused.

**Verified equivalent, not just plausible.** `TestMultiSourceDiff_EquivalentToMergeThenDiff`
asserts the same `[]diff.DiffResult` from (a) the new multi-source path and (b) physically
merging the same sources and diffing the result the ordinary, long-trusted way — the same
"is the shortcut actually equal to the slow, obviously-correct thing" proof this package
always asks for before trusting a new fast path.

**Future work: the overlapping-content case.** Two sources that genuinely hold the same
path are still refused; combining them is `merge`'s job today. Removing that restriction
needs `merge`'s last-input-wins precedence built into the k-way merge itself: when cursors
tie, decide instead of refusing, using input order (the same rule `merge` documents) to pick
a winner and require duplicate-path decisions to be threaded through the merge as a first-class
outcome rather than an abort. That's real but bounded work — the k-way merge machinery
built here is exactly what a tie-breaking version would extend, not a mechanism to replace —
and, as `internal/diff`-adjacent code, it should get the same equivalence-testing rigor as
everything else in this file: an oracle test proving the tie-breaking result matches
physically merging the same overlapping sources and diffing that.

## Remaining known defects

Full details in [known-issues.md](known-issues.md).

### Merkle collisions and merkle bloat — FIXED 2026-09-09

See stage 4 above. Was the single highest-value change available in this package; now
built (`digestEntries`/`compactHash`), measured (200 MiB → 72.8 MiB retained heap on the
200,000-file case), and validated against 400,000 property-test seeds.

### Memory — bounded in the streaming engine, reduced 5.75× in the tree engine

Two identical 200,000-file snapshots now retain **34.1 MiB / 178 B per node**, down from
200 MiB / ~1 KiB. [diff-memory.md](diff-memory.md) has the change-by-change table and what
was deliberately left on the table.

The tree is still fully materialised in *this* engine, so that part is constant-factor
work: a 200M-node diff still wants ~34 GB. **`streamCompare` (Phase 2, built 2026-09-10)
is not on that curve** — it retains `O(depth × budget)` and measured 10.7× lower peak RSS
on a real 2.27M-file fleet diff, with byte-identical output. Phase 3 (2026-09-10) gave it
move/copy detection as an external sort, so it is now what `CompareSnapshots` uses for
every diff whose input arrives in DFS order; peak RSS measured 210 MiB at 6.4M files a
side, flat. This engine is retained as the fallback and as the oracle every equivalence
test is written against. [diff-memory.md](diff-memory.md) has both. `coverage` remains the
right command for a pure delete decision, per [fleet.md](fleet.md).

`TestMemory_UnifiedTree` asserts a per-node ceiling, so any of this regressing is a test
failure rather than a swap storm.

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

`TestInvariant_RandomTrees_Budgeted` re-runs every generated pair through a line budget of
1, because truncation is the one mechanism that removes lines on purpose and so the one
most able to lose a file.

It found five distinct data-loss bugs on first run — 200 of the first 300 generated trees
lost files — and every one of them was real. Failures print the seed and the whole diff.
`randomTreeSeeds` is 5000 for routine runs; raise it by hand after changing the engine
(400000 takes about 20 seconds and is currently clean).

**When you change this package, run the property test before the golden tests.** A golden
test tells you that output changed; the property test tells you whether the change loses
data.
