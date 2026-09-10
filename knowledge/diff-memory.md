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
| **Measured cost now, tree engine** | **178 B/node** (same case, 34.1 MiB — an 83% reduction, see "What the constant-factor work achieved") |
| **Measured cost now, streaming engine** | **`O(depth × budget)`** — bounded, does not scale with file count at all (Phase 2, below). Since Phase 3 that includes move/copy detection, at the cost of a fixed sort buffer. |
| Observed failure | 200 GB of swap ⇒ roughly 100–200M nodes in the unified tree |
| Target | **1 GB resident**, temp storage unconstrained |
| Implied budget at 200M nodes | **≈5 bytes per node** |

Five bytes per node settles the design question before it is asked: **nothing proportional
to file count can live in RAM.** Compaction is not a route to the goal — it is a
multiplier on a curve that still goes to infinity. The tree has to leave memory.

**That conclusion survived the constant-factor work, and it is why Phase 2 matters.**
178 B/node is a 5.75× improvement on the tree engine and moves the practical ceiling a
long way — a 10M-node diff went from ~10 GiB to ~1.7 GiB — but it is still a smaller
constant on the same curve, and the 200M-node case that stalled the project still wants
~34 GB.

The streaming engine (Phase 2, built 2026-09-10) is off that curve entirely: retention is
`O(depth × budget)`, measured identical at 20,000 and 200,000 files, and 10.7× lower peak
RSS than the tree engine on a real 2.27M-file fleet diff. **Phase 3 (also 2026-09-10)
removed its one restriction**: move/copy detection now runs as an external sort, so there
is no longer a choice between a bounded diff that answers "what changed" and an unbounded
one that also answers "where did it go". Both are the same command.

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

### Phase 2 — streaming rollup and collector — BUILT 2026-09-10

`streamCompare` (`internal/diff/streaming.go`) produces the diff from a
depth-first stream instead of a materialised tree. One `streamFrame` per
directory on the current path, each holding the rollup accumulator, running
counts and its budgeted output — `O(depth × budget)`, nothing per file.

**Measured.** Peak frames 3 and peak retained lines 59 at *both* 20,000 and
200,000 files per side: ten times the input, identical retention
(`TestStreaming_RetentionIsBoundedByDepthAndBudget` asserts they do not move).
End to end on real fleet data — `luna/mike/archives` vs `luna/mike/unsorted`,
2.27M files — **peak RSS 1,362 MiB → 127 MiB, a 10.7× reduction, byte-identical
output**.

**Scope: `--no-moves --no-copies` only.** Not a shortcut — it is where the phase
boundary genuinely falls. `detectMovesCopies` needs a hash-keyed index over every
node in both snapshots, which is exactly the global view a stream does not have;
Phase 3 below is what replaces it. A stream can currently answer "what changed"
but not "where did it go". That still covers the cases that actually blow up: two
scans of the same multi-million-file tree, the stale-replica comparisons in
[fleet.md](fleet.md), where moves are rare and the question is "what does the
replica lack".

**Selection and fallback.** `Options.Engine` is `auto` / `tree` / `streaming`.
Auto streams when it can and builds the tree when it cannot, so the answer is
always the full-featured one and only the memory profile changes; `streaming`
refuses rather than falling back, for a caller that would rather hear "no". The
two ways streaming declines are options it cannot honour and input whose order it
cannot trust.

**What building it actually cost, and what that says about doing Phase 3.** The
equivalence harness found four real bugs, every one of which would have shipped
as plausible-looking wrong output rather than a crash:

- leaf lines carried no per-file counts;
- a `FileTwin` whose fate differs from its directory must force `Mixed` — without
  it, a file-becomes-directory collapsed to one line and silently dropped the
  other half, which is the severity-1 direction;
- `mergeJoinStreams` compared raw paths, so DFS-ordered inputs came back out
  re-interleaved: **a merge join imposes its own order regardless of how its
  inputs were sorted**, so it now takes an explicit key function;
- a directory's `DirA` comes from its children, never from its twin.

Two design notes worth carrying forward. `rollupAccum`/`decideRollup`
(`internal/diff/rollup.go`) exist so both engines reach the rollup verdict through
the same code rather than the streaming engine transcribing rules
[diff-algo.md](diff-algo.md) itself calls "reverse-engineered from individual test
cases" — Phase 3 should extend that seam, not fork it. And the DFS-key ordering is
*verified as it walks*, not trusted: 0x01 is a legal filename byte, so a
pathological name could still sort wrong, and under the severity rule a mis-nested
node is a wrong answer, so `streamCompare` returns `errStreamOutOfOrder` and lets
the caller fall back instead of guessing.

### Phase 3 — external move/copy matching (stage 6) — BUILT 2026-09-10

`internal/diff/external.go`. The streaming engine now does a full-featured diff; there is
nothing left that only the tree engine can answer, bar one unreachable shape (below).

**No spine file was needed, and that is the main structural surprise.** The plan below
assumed the matcher would carry an 8-byte ordinal instead of a path (40-byte records) and
then join those ordinals back to paths through a spine written in DFS order. As built,
there is no spine: the *source* records carry their path directly, and the destination is
identified by ordinal because the walk that consumes the decisions re-derives every path as
it goes anyway. The spine, the ordinal→path resolution pass and one whole external sort all
disappear with it. What is paid for that is a fatter hash-record file — a path instead of
nothing on the source side — which is the cheaper end of the trade, since the alternative
was an extra full pass over both snapshots.

What it does, in three steps:

1. **Pass 1** walks the stream and writes, per node, a *source* record keyed on its A-side
   hash (for every node `detectMovesCopies` would have indexed) and a *target* record keyed
   on its B-side hash (for every node it would have tried to match). Every guard the tree
   engine applies is applied at emit time, so a node it would never have indexed never
   reaches the sort: the `presentInA` prune, the empty-hash skip, the zero-byte-file skip
   (one hash shared by every empty file would make each of them a move of all the others),
   and the refusal to match a directory that already held something in A.
   This is where the streaming engine starts computing directory digests, which Phase 2
   skipped because nothing consumed them.
2. **The sort key is `(hash, ordinal)`**, so byte order groups by content and, within a
   group, restores the pre-order traversal — which is what decides *which* source a move is
   attributed to. Each group is then scanned with **four forward-only cursors** over the
   group's byte range: one per map the tree engine kept (`removedMap`, `modifiedMap`,
   `existingMap`) plus one for the targets. Taking a source is advancing a cursor, which
   consumes it exactly as `take()` popped a slice head.
3. **The decisions** — "node 4,201 is a Move whose source is `/old/x`", "node 900 is that
   source, suppress it" — are sorted back into ordinal order and merge-joined into a second
   walk, which produces the output.

**The pathological group costs nothing now.** The plan flagged that one digest can cover an
enormous group (every directory of zero-byte files hashes the same) and suggested a bounded
buffer with spill. The cursor design removes the problem instead of managing it: four
forward scans over a contiguous byte range, no buffer at all. Where the tree engine's
`map[hashVal][]*Node` held every member of the group, this holds one record per cursor.
`TestExternal_HugeContentGroup` pins a 4,000-member group.

**Stage 8 came with it**, because externally it is the same shape of problem: another walk
with a larger demotion set merge-joined in. It is written up under Phase 4 below, including
the three things that had to be got right and the measured round counts — read that section
before touching the fixed point, not this paragraph.

**Measured.** With every one of 400,000 files a side moving from one tree to another, peak
frames 2 and peak retained lines 27 at *both* 20,000 and 200,000 files a side, two passes
in both cases (`TestExternal_RetentionIsBoundedWithMovesOn`). Peak RSS, everything moving,
against the tree engine on the same input:

| Files per side | Tree engine | Streaming + external matching |
|---|---|---|
| 100,000 | 100 MiB | 62 MiB |
| 200,000 | 192 MiB | — |
| 400,000 | 376 MiB | 179 MiB |
| 1,600,000 | ~1.5 GiB (extrapolated; would not fit) | 215 MiB |
| 6,400,000 | ~6 GiB (extrapolated) | **210 MiB** |

**The streaming column is flat, not merely slower-growing** — the last two rows differ by
4× in input and 5 MiB in the wrong direction, which is measurement noise. What is left
resident is the sort buffer (`sortMemLimit`, 64 MiB) plus `O(depth × budget)` plus Go's GC
headroom, none of which grows with the file count. The tree column is a straight line
through ~470 B/node.

Measured on a 4 GB machine, so the tree rows stop where they stop; that is itself the
point. Note the temp files must be on real storage for a measurement like this to mean
anything — on this box `/tmp` is a `tmpfs`, so the first attempt was spilling into RAM.

**Two real defects the equivalence sweep caught**, both of which would have shipped as
plausible wrong output rather than a crash:

- **The root was never emitted to the matcher.** `closeTop` never runs for the virtual
  root, so its digest and status never reached the index — and the root is a legitimate
  copy source. Seed 6084 (`A = {/b}`, `B = {/c/b}`) should read `Removed b` + `Copy c/`,
  because `c`'s digest equals the root's A-side digest; the stream reported
  `Move c/b <- b` instead.
- **`accumulateStats` counts the root itself as an unchanged directory**, so a diff with
  nothing to report says "1 directory unchanged". The frame never counts itself — `closeTop`
  does that, and the root never closes. Only visible with `--show-unchanged`, which is why
  the harness now sweeps that flag too.

**One tree-engine behaviour a stream cannot reproduce, and refuses to guess at.** A
`matched` node freezes its subtree at stage 5 statuses (`propagateStatus` returns without
recursing into it). That is invisible while the node collapses to one line — but a
`FileTwin` whose fate differs forces `Mixed`, and the collector then prints those frozen
children, where a stream has already rolled them up with the move statuses included.
Reaching it needs a path that is a file **and** a directory *within the same snapshot*: a
matched directory is never present in A, so its twin can only be the B-side file half. No
filesystem holds both, the scanner cannot record both, and 200,000 generated tree pairs
contain none. `streamCompare` returns `errStreamMatchedTwin` (wrapping
`errStreamUnsupported`) and the caller falls back to the tree engine.

**Two things carried over from Phase 2 as intended.** `rollupAccum`/`decideRollup` is still
the single implementation of the rollup rules — the streaming engine feeds move and copy
statuses (and their source paths) into the same accumulator rather than transcribing the
rules a third time. And DFS-key ordering is still *verified* as the walk proceeds, on every
pass, rather than trusted.

**Two things that fell out of the work and are worth knowing:**

- **Phase 2's retention bound had a hole in it.** The line budget was applied when a
  directory closed, so a directory with a million changed children accumulated a million
  lines and only then capped them. It is now applied as lines are appended, which is
  equivalent output and actually `O(budget)`.
- **The streaming engine never reported progress.** Nobody noticed while it only ran under
  `--no-moves --no-copies`; the moment `auto` started streaming the ordinary diff, the
  progress bar would have sat at 0% for the whole run. It reports per pass now, so a
  multi-pass diff sweeps the bar once per pass.

### Phase 4 — external fixed point (stage 8) — BUILT 2026-09-10, with Phase 3

Built alongside Phase 3 rather than after it, because externally the two are the same
machinery: once stage 6 has made the engine re-walk the input, reaching a fixed point is
another re-walk with a bigger merge-join, not a new mechanism.

**Three things this section used to say turned out to be wrong**, and the corrections are
the load-bearing part:

- *"the trial output written to a temp file"* — no. `[]DiffResult` is the function's return
  value; it is in RAM whatever happens, and `accountedPaths` over it is therefore free in
  the asymptotic sense. **The output is the one thing in this engine that legitimately
  scales with the answer rather than with the input.** Externalising it would have bought
  nothing and cost a pass.
- *"`accountedPaths` merge-joined back into the next pass by ordinal"* — the join runs the
  other way round. `accountedPaths` is a **path-keyed** set, consulted through
  `sourceNamed`, which walks up path components; ordinals never touch it. What is
  merge-joined by ordinal is the **demotion set**, which is the *output* of the check
  rather than its input.
- *"one sequential pass over the spine"* — there is no spine (see Phase 3), so each round is
  a **full re-walk of both snapshots**. At fleet scale that is a whole DB scan per round,
  far more expensive per round than the tree engine's re-collection over a materialised
  tree. It is affordable only because extra rounds are rare, which is now measured rather
  than assumed (below).

**Two things it did not anticipate, and both are wrong answers if got wrong:**

- **The demotion set must be cumulative.** The tree engine gets this for free, because
  `reinstateHiddenMoveSources` only ever looks at nodes that are *still* `MovedSource` and
  its demotions persist in the tree. A re-walking engine recomputes from scratch every
  round, so unless the set is carried forward it oscillates: a source demoted in round *k*
  emits a `Removed` line, round *k+1* sees that line, concludes the source was mentioned
  after all, and puts it back.
- **The candidate is the highest suppressed node, and that is a bottom-up fact.**
  `reinstateHiddenMoveSources` stops recursing at the first `MovedSource` it meets and
  decides for that whole subtree, which is *not* the same as deciding for each leaf:
  coverage is monotone downward, so a leaf can be named by the output while the directory
  above it is not. A stream cannot know a directory is wholly `MovedSource` until it
  closes, by which point its children have already written their candidate records — hence
  `spillFile.truncate`, and hence a directory closing as `MovedSource` cancelling its whole
  subtree's records and writing one for itself. The log stays in ordinal order because
  candidates are mutually incomparable, and for such nodes pre-order and post-order
  coincide.

**And the root is a node.** If every file moved away, the root rolls up to `MovedSource`,
and no line can ever name the root — so the whole diff correctly reverts to `Removed`.

**Measured convergence** (`TestExternal_FixedPointConverges`, 100,000 runs over
`property_test.go`'s corpus, unbudgeted and at budget 1 — a deliberately move-dense
corpus, far worse than real data):

| Output rounds | Runs |
|---|---|
| 1 (nothing needed reinstating) | 76,118 |
| 2 | 23,643 |
| 3 | 236 |
| 4 | 3 |

So three quarters of diffs pay one output walk, and the worst case observed is four. The
cap of 32 remains a guard, not an expected limit — and the test now fails well before it,
because hitting the cap means returning a diff that never reached its fixed point, which is
a wrong answer rather than a slow one.

The invariant from [diff-algo.md](diff-algo.md) carries over unchanged and gets sharper
teeth here: **anything that removes lines from the output must live inside the collector**,
because stage 8 only sees what the collector emitted. Translated to the streaming engine:
it must live inside the walk that produces `results`. The line budget is the live example —
`streamFrame.appendOut` applies it as lines are appended, inside the walk, so stage 8 sees
its consequences.

### Phase 5 — engine selection and measurement — BUILT 2026-09-10

`--engine auto|tree|streaming` (not the `memory|external` names guessed here), defaulting
to `auto`. It does **not** switch on estimated node count: `auto` streams whenever the
input arrives in DFS order and falls back only when the streaming engine says it cannot
answer, so the choice costs nothing and cannot be wrong — a misjudged threshold would have
been a silently worse memory profile. `--temp-dir` is the other knob; see
[cli.md](cli.md), including the warning that the default may be a `tmpfs`.

The tree engine is kept permanently, as planned: it is faster below a few million nodes,
and it is the oracle for the equivalence test.

Still unbuilt: nothing tunes `sortMemLimit` (a package-level 64 MiB) from the command line.
It is the single largest resident item once a diff is big enough for the file count not to
matter, so it is the knob to expose first if a fleet run needs to trade RAM for I/O.

## How this gets verified

This is the highest-risk code in the project, so the acceptance test is equivalence, not
inspection:

- `streaming_test.go` runs **both engines on the same input and asserts identical
  `[]DiffResult`** — over the hand-built scenarios, over `property_test.go`'s random trees,
  budgeted and not, and with `--show-unchanged` both on and off. Routine runs cover 5,000
  seeds; **clean to 200,000 across all six combinations as of Phase 3**, which takes about
  seven minutes and is the sweep to repeat after any change to either engine.
  `external_test.go` adds the move-specific shapes, the switch combinations
  (`--no-moves` and `--no-copies` each have their own path through the matcher), and a
  run with the spill limits shrunk to a few kilobytes so the disk-backed halves of
  `spillFile`, `recReader` and the sorter's run merge are actually executed — no
  test-sized diff reaches them otherwise.
- The two existing invariants — every differing file is accounted for somewhere; a
  collapsed Added/Removed line never contradicts the snapshots — apply to the streaming
  engine unchanged, and reach it through `CompareSnapshots`.
- Boundedness is asserted structurally rather than by sampling the heap: the engine records
  its own high-water marks (frames, retained lines, passes) and the tests assert that
  **ten times the input does not move them at all**. Heap sampling for this is both noisy
  and, at any useful rate, slower than the work being measured.

## Cost, as built

Resident memory is `sortMemLimit` (64 MiB) + `O(depth × budget)` + I/O buffers + Go's GC
headroom, which measured **215 MiB at 1.6M files a side** and stops tracking the file count
from around there. Temp storage is smaller than the estimate this section used to carry,
because there is no spine file:

| Resource | For 200M nodes |
|---|---|
| Hash records (~45 B: 21 B hash, 8 B ordinal, 3 B flags, path on the source side) | ~9 GB, plus the same again transiently during the run merge |
| Decisions (~20 B, only for nodes that actually matched) | ≤ 4 GB |
| Candidates (one record per suppressed move source, per round) | ≤ 4 GB |
| **Total temp storage** | **≈20 GB** for 200M nodes; ≈2 GB for 20M |
| Passes over both snapshots | 1 for the matcher + 1 per fixed-point round (measured: one round in 76% of runs, two in 24%, four at worst — see Phase 4) |

**~20 GB of temp space is still a real constraint on a fleet with 8.11T free spread across
pools at 94–96%,** which is what `--temp-dir` is for. Both guards are now built (2026-09-10),
and they are deliberately of two different kinds:

- **The up-front estimate is advice, not a gate.** `EstimateTempBytes` is an upper bound on
  the record format, and it runs three to five times the measured peak (224 B/node estimated
  against 46–69 B/node measured, on ~20-byte paths). Refusing on that would turn away runs
  that would have fitted comfortably. `internal/app` logs it against free space, and warns
  when the temp directory is memory-backed — on most Linux systems the default `/tmp` is a
  `tmpfs`, so a diff "spilling to disk" there is not spilling at all and this whole phase's
  bound quietly stops holding. Both are suppressed below a 256 MiB estimate, since an
  ordinary diff never leaves memory.
- **The hard stop is in the engine.** `spillMeter` `statfs`'s the temp directory every
  64 MiB written and aborts once free space would drop below `DefaultMinFreeTempBytes`
  (512 MiB), returning an error that names `--temp-dir` and **no results at all** — a
  partial diff that looks complete is the failure [goals.md](goals.md) ranks worst. That
  refuses when a run genuinely would fill the filesystem rather than when a guess says it
  might, which is why the estimate is allowed to stay pessimistic.

The measured figures come from `spillMeter.peak`, which is a permanent count of live temp
*disk* (bytes still in memory are not disk), so the estimate can be checked against reality
rather than re-guessed: `TestExternal_TempEstimateIsNotOptimistic` asserts the direction of
its error on shapes that really spill.

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
