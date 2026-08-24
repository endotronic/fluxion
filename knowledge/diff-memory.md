# Making `diff` fit in 1 GB

Written 2026-08-23, after the author reported needing **200 GB of swap** to diff real
snapshots — the reason the project stalled. This file records where the memory goes, why
no amount of tuning the current design reaches the target, and the plan to replace it.

Read [diff-algo.md](diff-algo.md) first; this file assumes the ten-stage pipeline and
refers to stages by number. Read [fleet.md](fleet.md) for why the numbers are this big.

## The budget

| | |
|---|---|
| Measured cost today | **~1 KiB per unique path** (CONFIRMED: two identical 200,000-file snapshots retained 200 MiB) |
| Observed failure | 200 GB of swap ⇒ roughly 100–200M nodes in the unified tree |
| Target | **1 GB resident**, temp storage unconstrained |
| Implied budget at 200M nodes | **≈5 bytes per node** |

Five bytes per node settles the design question before it is asked: **nothing proportional
to file count can live in RAM.** Compaction is not a route to the goal — it is a
multiplier on a curve that still goes to infinity. The tree has to leave memory.

That does not make compaction worthless; see phase 0 below, which is worth ~5× on its own
and is a prerequisite for the rest anyway.

## Where the 1 KiB goes

Per `Node` (see the struct in `internal/diff/diff.go`):

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

### Phase 0 — fixed-width digests instead of merkle strings

Already tracked as issues 2.2 and 3.1. Replace the concatenated merkle string with a
16-byte digest, and store leaf hashes as bytes rather than 40-char hex.

Semantics to preserve exactly (from `computeMerkleHashes`): the digest is taken over the
children's `name:hash` pairs in sorted order, twins contribute a second entry tagged
`name:file:hash`, children with an empty hash on that side contribute nothing, and `DirA`
/ `DirB` remain tracked separately from "has a hash".

Pays for itself three times: fixes a real collision bug, cuts memory ~5×, and — the reason
it comes first — makes every intermediate record **fixed-width**, which is what makes the
external sorts in phases 3–4 cheap.

### Phase 1 — a `NodeStream` abstraction, still in memory

Refactor `CompareSnapshots` to build its tree *from* a DFS-ordered merge join of A and B,
rather than from two independent inserts. No behaviour change; the point is to prove the
ordering assumptions and the twin/straggler handling against the existing property test
before any of it runs on disk.

The merge join itself is `O(1)` memory: both sides arrive sorted, and
`idx_files_snapshot_path UNIQUE(snapshot_id, path)` means SQLite can produce each side as
an index scan with no sort. Two things must be checked before relying on that: relative
paths preserve absolute order only while `root_path` is a genuine prefix (`app/diff.go`
has a fallback branch for records that are not), and the DFS key is not the index's order.

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
