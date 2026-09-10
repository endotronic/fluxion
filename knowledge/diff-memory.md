# Making `diff` fit in 1 GB

Written 2026-08-23, after the author reported needing **200 GB of swap** to diff real
snapshots — the reason the project stalled. This file records where the memory goes, why
no amount of tuning the current design reaches the target, and the plan to replace it.

Read [diff-algo.md](diff-algo.md) first; this file assumes the ten-stage pipeline and
refers to stages by number. Read [fleet.md](fleet.md) for why the numbers are this big.

## The budget

| | |
|---|---|
| Measured cost when this was written | ~1 KiB per unique path (two identical 200,000-file snapshots retained 200 MiB) |
| **Measured cost now** | **178 B/node** (same case, 34.1 MiB — an 83% reduction, see "What the constant-factor work achieved") |
| Observed failure | 200 GB of swap ⇒ roughly 100–200M nodes in the unified tree |
| Target | **1 GB resident**, temp storage unconstrained |
| Implied budget at 200M nodes | **≈5 bytes per node** |

Five bytes per node settles the design question before it is asked: **nothing proportional
to file count can live in RAM.** Compaction is not a route to the goal — it is a
multiplier on a curve that still goes to infinity. The tree has to leave memory.

**That conclusion survived the constant-factor work, and is the thing to keep in mind
before doing more of it.** 178 B/node is a 5.75× improvement and it moves the practical
ceiling a long way — a 10M-node diff went from ~10 GiB to ~1.7 GiB, which is the
difference between impossible and routine on an ordinary machine. It does nothing for the
200M-node case that stalled the project: that is still ~34 GB. Phases 2–5 remain the only
route to the stated target.

## What the constant-factor work achieved

All measured on the same case — two identical 200,000-file snapshots, 200,551 nodes — by
`TestMemory_UnifiedTree`, which asserts a per-node ceiling so these cannot silently regress.

| Change | B/node | Note |
|---|---|---|
| (original) | ~1024 | Concatenated merkle strings dominated. |
| Phase 0: fixed-width digests, byte leaf hashes | 322 | 2026-09-09. Also fixed a real collision bug. |
| `Children` no longer pre-allocated on leaves | 274 | An empty Go map is a ~48 B allocation that most nodes never use. |
| `Path` replaced by `Parent` + `path()` | 226 | Storing the path per node re-stored every component once per depth level. |
| `Status` string → `uint8` | 210 | 16 bytes of string header to hold one of nine constants. |
| `HashA`/`HashB` string → inline `hashVal` | 178 | Header plus a separate allocation each; now 21 B inline. |

`Sizeof(Node)` is 136 B; the rest is the `Name` text and the parent map's per-entry
overhead. What remains, and why it was left:

- **`SourcePath string` (16 B)** is set only on Move/Copy nodes but costs every node its
  header. A side map keyed by `*Node` would reclaim it, at the price of threading that map
  through `propagateStatus`, `detectMovesCopies` and the collector, and a lookup per
  Move/Copy child on every one of stage 8's up-to-32 rollup passes. ~9% for a real
  complexity increase in the most delicate code in the package.
- **The `Children` map's per-entry overhead (~29 B/node)** is the largest single item left.
  A sorted `[]*Node` would be far smaller, but `locateNode` needs lookup *while* building,
  so a slice makes construction O(k²) per directory — fatal for a wide one. Converting
  after the build does not help, because the peak is what matters.
- **`Name` text** could be interned; trees repeat names heavily. Variable payoff, and it
  trades a bounded win for an unbounded intern table.

## Retained tree vs. process RSS — use the right number when sizing a machine

The per-node figures above are **retained heap for the tree**, measured after a
collection. They are the right metric for comparing changes to the data structure and the
wrong one for answering "will this run on my box". Measured on real fleet data
(`luna/mike/archives` + `luna/mike/unsorted`, 1,158,320 + 1,112,170 files ≈ 2.27M nodes):

| | |
|---|---|
| Retained tree, extrapolated at 178 B/node | ~400 MiB |
| **Actual peak process RSS, default `GOGC`** | **~1,370 MiB** (~620 B/node) |
| Actual peak process RSS, `GOGC=40` | ~1,140 MiB, byte-identical output |

The ~3.5× gap is not overhead anyone forgot about: Go's collector lets the heap grow to
roughly twice the live set before collecting (`GOGC=100`), and on top of the tree there are
`detectMovesCopies`' three transient index maps and the SQLite driver's row buffers.

Two consequences:

- **Size machines against ~600 B/node, not 178.** A 10M-node diff wants roughly 6 GB of
  RSS, not 1.7 GiB.
- **`GOGC` is a free lever when a diff is close to the limit.** Lowering it trades CPU for
  peak RSS and changes nothing about the output — worth reaching for before concluding a
  diff cannot run. It does not change the asymptotics, so it rescues a run that is close,
  not one that is off by an order of magnitude.

## Where it used to go

Kept because it explains what each change above was aimed at. Per `Node`, as measured
before any of this work:

| Component | Approx. | Note |
|---|---|---|
| Merkle hash strings (`HashA`/`HashB` on directories) | **~326 B/file, grows with depth** | CONFIRMED as issue 3.1 — the single largest component. A directory's "hash" literally contains every descendant hash concatenated. |
| `Path` (full path, per node) | ~80–120 B | Stored in full on every node, so every path component is re-stored once per depth level. |
| Struct fields + padding | ~144 B | |
| Leaf `HashA`/`HashB` (40-char hex ×2) | ~80 B + 32 B headers | Hex, not bytes: 2.5× larger than necessary. |
| `Children` map + the parent's map entry | ~100 B+ | Go map overhead per entry, plus a 48 B allocation per directory. |
| `detectMovesCopies` index maps | transient, ~50 B/node | Three `map[string][]*Node` keyed on those long merkle strings. |

Two structural facts follow, and they drive everything below:

1. **The whole unified tree is materialised** before stage 5 can run. The store side
   already streams (ROADMAP 0.8.11), so the input is not the problem; the tree is.
2. **Only three stages are genuinely global.** Stage 6 (move/copy matching) needs a
   hash-keyed index over every node. Stage 8 needs the trial output. Everything else —
   insert, merkle, rollup, collect — is a bottom-up computation over a tree, and a
   bottom-up computation over a tree is exactly what a **depth-first stream with a stack**
   computes without holding the tree.

## Why the stack works: two non-obvious facts

**Fact 1 — a directory's pending output is bounded by the line budget.** Collapsing is a
bottom-up decision: a directory cannot emit until its children are known. But since
`--max-lines` (default 25) caps how many lines a directory may contribute, a directory
never needs to retain more than `MaxLinesPerDir + 1` pending lines plus a set of counters.
Everything beyond the budget is already being summarised into `StatusTruncated`. So
retained output is `O(depth × budget)` — kilobytes, not gigabytes.

*Consequence:* `--max-lines 0` (unlimited) is unbounded by construction and must either be
rejected by the streaming engine or backed by a spill file. This is the one place where
the line budget stops being a readability feature and becomes load-bearing.

**Fact 2 — DFS order must be produced explicitly; plain path sort is not it.** All entries
sharing a prefix `a/` are contiguous under byte sort, which is enough for subtree
contiguity. But the `FileTwin` case is not adjacent: given a file `a`, a file `a.txt`, and
a file `a/x`, byte order is `a`, `a.txt`, `a/x`, because `.` (0x2E) sorts below `/` (0x2F).
Detecting that `a` is a twin of directory `a/` would then require remembering every file
child seen so far — unbounded for a wide directory.

Sorting on a **DFS key** — the path with `/` rewritten to `0x01` — makes `a` and `a\x01x`
adjacent and restores O(1) twin detection. Caveat: `0x01` is a legal byte in a filename
(only `/` and NUL are not), so a pathological name could sort wrong. The streaming engine
must therefore *verify* the prefix relationship as it walks and divert any entry that does
not extend the current stack path into a small stragglers buffer, rather than trusting the
order blindly. Under the severity rule a mis-nested node is a wrong answer, not a cosmetic
one.

## The plan

Six phases. Each is independently shippable and independently verifiable; nothing after
phase 0 changes observable output.

### Phase 0 — fixed-width digests instead of merkle strings — BUILT 2026-09-09

Was tracked as issues 2.2 and 3.1 (now closed — remove from known-issues.md if still
listed there). Replaced the concatenated merkle string with a 20-byte SHA-1 digest
(`digestEntries`) over a length-prefixed encoding — not 16 bytes as originally estimated
here, but still fixed-width regardless of subtree size — and leaf hashes are now stored as
decoded bytes rather than 40/32-char hex (`compactHash`, called once in `insertNode`). Both
live in `internal/diff/diff.go`; see [diff-algo.md](diff-algo.md)'s Stage 4 section for the
full before/after.

Semantics preserved exactly, as required (from `computeMerkleHashes`): the digest is taken
over the children's `name:hash` pairs in sorted order, twins contribute a second,
distinctly-tagged entry, children with an empty hash on that side contribute nothing, and
`DirA`/`DirB` remain tracked separately from "has a hash". Nothing outside `internal/diff`
reads a `HashA`/`HashB` value directly, so switching from hex text to raw bytes and from a
joined string to a digest is invisible to every caller — confirmed by grep, not assumed.

Paid for itself as promised, measured rather than estimated: fixed the real collision bug
(length-prefixing means no separator byte can be reinterpreted as content), cut retained
heap 2.75× on the 200,000-file case (200 MiB → 72.8 MiB, short of the ~5× guessed below —
`Node` struct/map overhead this phase doesn't touch is a bigger fraction of the total than
assumed), and made every intermediate record **fixed-width**, which is what makes the
external sorts in phases 3–4 below cheap when they get built. Validated against 400,000
property-test seeds (`internal/diff/property_test.go`, both the plain and
line-budget-1 runs) with zero failures, plus a new permanent regression test
(`internal/diff/memory_test.go`) asserting a heap ceiling so a regression back toward
string concatenation fails a test instead of surfacing as a swap storm on real fleet data.

**Phases 1–5 below remain unbuilt.** This alone does not remove the `O(files)` memory
scaling — a big enough tree still won't fit — it only moves the wall further out. `coverage`
is still the command for anything at fleet scale ([fleet.md](fleet.md)).

### Phase 1 — a merge-join tree builder, still in memory — BUILT 2026-09-09

`CompareSnapshots` now builds its tree from a merge join of A and B
(`mergeJoinInsert`, `internal/diff/mergejoin.go`) rather than two independent passes. The
tree is still fully materialised afterwards, so this buys no memory on its own — the point
was to establish that the tree *can* be built by co-walking two ordered streams, since the
later phases replace the tree with a stack and will have no map to fall back on.

What it took, and what it found:

- **`IterateFiles` had no `ORDER BY` at all.** Today's two-pass builder never needed one —
  `insertNode` works through map lookups and is completely order-independent — so nothing
  was enforcing the order a merge join wants. Added; it is free, because
  `idx_files_snapshot_path` is `(snapshot_id, path)` and an equality match on the first
  column already yields path order (confirmed against the real fleet DB: no temp b-tree in
  the plan).
- **`FileIterator` is push-based**, which cannot serve "show me your next record so I can
  decide". Bridged with stdlib `iter.Pull` (`pullIter`, `internal/diff/pulliter.go`), so
  the source runs as a runtime coroutine — no goroutine, no channel, and none of the
  deadlock/leak failure modes the pre-Go-1.23 goroutine+channel idiom would have added to
  this package.
- **Sorted input is an optimisation here, not a correctness requirement.** The concern
  flagged below — that relativisation against `root_path` has a fallback branch for records
  not underneath it, so sorted-at-SQL does not imply sorted-at-yield — turns out not to
  threaten this phase: every loop iteration consumes at least one record, the loop runs
  until both sides are exhausted, and `locateNode` is idempotent, so every record is applied
  exactly once whatever the order. Order only decides whether a path's two sides are handled
  in one step or two. `TestEquivalence_MergeJoinToleratesUnsortedInput` asserts that against
  deliberately shuffled input rather than trusting the argument. **The stack-based phases
  below will not enjoy this property** — they need genuine subtree contiguity — which is
  exactly why establishing the seam here first was worth it.

**The equivalence harness is the durable part.** `twoPassInsert` is retained, not deleted:
it is the behaviour every golden test and all 400,000 property-test seeds were validated
against, so it is the only trustworthy definition of "the tree we are supposed to get".
`compareSnapshotsWith(build treeBuilder, ...)` makes the builder a parameter, and
`mergejoin_test.go` runs both over the same corpus asserting byte-identical `[]DiffResult`
— unbudgeted, budgeted at 1 line (truncation being the one mechanism that removes output on
purpose), and with shuffled input. **Phases 2–5 should extend this file rather than start
over**; swapping a stage for an external one is the same shape of change.

**It immediately found a real bug that had nothing to do with the merge join.**
`propagateNodeStatus` captured "the first Move/Copy child" while ranging over
`node.Children` — a map — so a rolled-up Move/Copy line named a *different source on every
run of the same input* (`Move d/ <- c/c/b/c` one run, `Move d/ <- e` the next). The property
test never caught it because both answers are complete and sound; it is "wrong or unstable",
not the data-loss class. Fixed by ranging `sortedChildren`, the convention
`detectMovesCopies` and the collector already followed, and pinned by
`TestDeterminism_RepeatedRunsAgree`. The lesson worth carrying into later phases: **an
equivalence check is worthless while the thing being checked disagrees with itself**, so
determinism is the first property to establish, not the last.

### Phase 2 — streaming rollup and collector

Replace stages 4, 5, 7 and 9 with a single stack walk. Each open directory carries an
accumulator: per-status child counts, the `allAddedLike` / `hasMovedSource` flags that
`propagateNodeStatus` needs, two running digests, and its pending line list capped at the
budget. On close, the directory decides collapse-or-emit and hands the result to its
parent.

Memory: `O(depth × budget)`.

### Phase 3 — external move/copy matching (stage 6)

The one stage that needs a global view. As a three-step external sort:

1. Phase 2 writes a **spine** file in DFS order: front-coded path suffixes plus each
   node's digests, sizes, flags and status. Nodes are numbered by ordinal.
2. Emit a fixed-width `(digest, ordinal, kind, size)` record per node, **external-sort by
   digest**, then scan hash-groups applying the existing pairing and consumption rules —
   `Removed`↔`Added` = Move, existing↔`Added` = Copy, plus the swap cases. Carrying an
   8-byte ordinal instead of a path is what keeps these records at 40 bytes.
3. Sort the resulting `(destOrdinal, sourceOrdinal, kind)` triples back into ordinal order
   and merge-join them against the spine on the next pass.

One pathology to handle deliberately: a single digest can cover an enormous group (every
zero-byte file shares one). The group scan must pair off streaming with a bounded buffer
and spill beyond it — the current in-memory code has the same pathology and simply
survives it by having already lost.

### Phase 4 — external fixed point (stage 8)

Stage 8 collects a trial output, derives `accountedPaths`, demotes unmentioned
`MovedSource` nodes to `Removed`, re-propagates and re-collects. Externally this is the
same loop with the trial output written to a temp file and `accountedPaths` merge-joined
back into the next pass by ordinal. Each iteration is one sequential pass over the spine;
it converges in one or two in practice and is capped at 32.

The invariant from [diff-algo.md](diff-algo.md) carries over unchanged and gets sharper
teeth here: **anything that removes lines from the output must live inside the collector**,
because stage 8 only sees what the collector emitted.

### Phase 5 — engine selection and measurement

`--engine auto|memory|external`, defaulting to `auto` and switching on estimated node
count (`GetFileCount` on both snapshots, so the choice costs one query). Keep the
in-memory engine permanently: it is faster below a few million nodes, and it is the
oracle for the equivalence test.

## How this gets verified

This is the highest-risk code in the project, so the acceptance test is equivalence, not
inspection:

- `property_test.go` runs **both engines on the same input and asserts identical
  `[]DiffResult`** — for the hand-built scenarios, for the random trees (currently 5,000
  seeds, clean to 400,000), and for the budgeted sweep.
- The two existing invariants — every differing file is accounted for somewhere; a
  collapsed Added/Removed line never contradicts the snapshots — apply to the external
  engine unchanged.
- Add a large-tree memory test asserting a hard ceiling on `HeapInuse` for a synthetic
  10M-node diff, so a regression is a test failure rather than a swap storm.

## Estimated cost

Estimates, not measurements — nothing here has been built. For a 200M-node diff:

| Resource | Estimate |
|---|---|
| Resident memory | sort buffer (tunable, ~256 MB–1 GB) + `O(depth × budget)` + I/O buffers |
| Spine file | ~13 GB (front-coded paths ~15 B, digests 32 B, sizes 16 B, flags) |
| Hash-sorted file | ~8 GB, plus ~8 GB transient during the merge |
| Match + accounted files | ≤ 3 GB |
| **Total temp storage** | **≈40 GB** for 200M nodes; ≈4 GB for 20M |
| Wall clock | I/O-bound: a few tens of GB written, ~100 GB read across all passes |

Note that ~40 GB of temp space is itself a real constraint on a fleet with 8.11T free
spread across pools at 94–96%. The temp directory must be configurable
(`--temp-dir`), and the engine should refuse to start rather than fill a pool.

## The shortcut worth taking first — BUILT (2026-08-23)

**This is now the `coverage` command.** What follows is the reasoning that produced it,
kept because it is the argument for why the six phases above are still deferred. For how
to use it see [cli.md](cli.md); the schema change is migration 5 in
[data-model.md](data-model.md).

It landed as a SQL semi-join rather than the merge-join described below — the partial
index makes SQLite do the streaming — and it measured **1.3 MiB of peak heap over 4M file
rows** (2M per side, 200k uncovered, 21.9s, 956 MiB database). Flat, not merely bounded:
nothing accumulates per row. The index cost 2 partial indexes rather than the 10 GB
estimated here, because it only covers rows with a non-empty hash.

Before any of the above: **the fleet's actual question does not need the diff tree at
all.**

"Is every file in `artemis/deprecated/historian_newer/content` present somewhere on luna?"
is pure set coverage over content hashes. Paths are irrelevant — for a delete decision you
do not care where the surviving copy lives. `diff --update` already answers it, but it
pays for the entire tree, the move/copy matching and the rollup to do so.

A `coverage` command (or `diff --update --by-content`) that merge-joins two hash-sorted
streams answers it with a bounded buffer and no tree. It can be done almost entirely in
SQL given an index on `(sha1, snapshot_id)` — one migration, at a cost of roughly 10 GB of
index for 200M rows — and reports, for each uncovered file, its path and size, plus the
total bytes at risk.

That is days of work rather than weeks, it needs none of phases 0–5, and it directly
unblocks the 9.36T and 5.77T decisions in [fleet.md](fleet.md). The full external diff is
still worth building — "what changed" and "where did it go" are real questions — but it is
not what stands between the author and the disk space.

Caveats to state whenever this command is used: it ignores paths entirely (a tree could be
"covered" while being unrecognisably reorganised), it inherits SHA-1's collision
properties, and it says nothing about which side is newer. As built it also counts a file
with no comparable hash as *not* covered, which is the direction [goals.md](goals.md)
requires.

## Before building any of it: measure

The whole plan is sized against a guess of 100–200M nodes, inferred backwards from the
200 GB swap figure. One `find <dataset> | wc -l` per candidate dataset replaces that guess
with a number, and may well show that phase 0 alone gets individual datasets under the
ceiling even if a whole-pool diff never will.
