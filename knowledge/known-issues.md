# Known issues

Findings from a full review of the code as of `v0.8.13`. Ordered by severity using the
rule from [goals.md](goals.md): **a bug that makes something look present/unchanged when
it is not can cost the user data; a bug that over-reports only costs reading time.**

Items marked **CONFIRMED** were reproduced with executed tests, not inferred from reading.

Issues fixed since the review are removed from this file rather than annotated — `git log`
is the record of what was fixed. As of 2026-09-10 that is 1.1, 1.2, 1.3, 1.4, 1.5, 2.1,
2.3, 3.3, 3.4, and 3.6, plus the cgo modernisation item; the numbering of what remains is
unchanged so earlier references still resolve. **No severity-1 issue is open.**

Four entries are kept in place with a FIXED/solved marker rather than deleted (2.2, 2.7,
3.1, 3.2), because what they explain — the merkle collision, the map-order
non-determinism, and where diff's memory went — is the reasoning behind how those areas
are now built, and it is referenced from the other knowledge files. Everything else here
is open.

The diff rewrite that closed 1.1, 1.2 and 2.1 also added `internal/diff/property_test.go`,
which found and closed four further data-loss bugs that this review had missed entirely —
all of them cases where a move source or a lost directory went unmentioned after a rollup.
See [diff-algo.md](diff-algo.md) for what it asserts and why. **Anything you fix in
`internal/diff` should be fixed against that test, not against the golden output.**

---

## Severity 1 — silent data loss in diff output

**Nothing open.** 1.3 was the last one; it was fixed 2026-09-10 (see `git log`, and
[cli.md](cli.md) for what `--exclude` now means). Every path prefix test in `internal/app`
goes through `pathHasPrefix` (`internal/app/paths.go`), which requires the match to land on
a path boundary.

---

## Severity 2 — wrong or unstable results

### 2.2 Directory hashes are not injective (collision) — FIXED 2026-09-09 (`internal/diff`), FIXED 2026-09-09 (`internal/dupes`)
`computeMerkleHashes` (`internal/diff`) and `computeMetadata` (`internal/dupes`) both used
to join `child.Name + ":" + child.Hash` with `,` without escaping either delimiter. A
directory containing one file literally named `a:AA,b` with hash `BB` produced
`"a:AA,b:BB"` — identical to a directory containing `a`(`AA`) and `b`(`BB`). The two
directories then compared equal, and `diff` could report one as a move/copy of the other,
or `dupes` could report one as a duplicate of the other.

Fixed in `internal/diff` first (`digestEntries` in `diff.go`: SHA-1 over a sorted,
length-prefixed child list — see [diff-algo.md](diff-algo.md) Stage 4), then ported to
`internal/dupes` the same day (`dirDigest` in `dupes.go`, same approach minus the `FileTwin`
tag `internal/diff` needs and `internal/dupes` has no equivalent of). One test
(`dupes_test.go`'s "Directory Duplicate (Collapsed)" case) had hardcoded the old scheme's
exact string (`"file1:h1"`) as its expected hash — a coupling to internal representation
that the fix necessarily broke — and was rewritten to check the meaningful property
instead (a directory-level group covering the expected paths), not the opaque digest value.

### 2.4 Deleted snapshots are still resolvable
`FindSnapshot` and `GetLastSnapshot` do not filter `status = 'deleted'` tombstones. Naming
a deleted snapshot in a diff yields an empty side, so everything reports as added or
removed with no indication of why.

### 2.5 `find --regex` ignores `--case-sensitive`
`internal/app/find.go` compiles the pattern as given in regex mode; the flag only affects
the glob path. Either honour it with `(?i)` or reject the combination.

### 2.6 `export-legacy` overwrites without asking
Output files are created unconditionally. Given the tool's "never destroy anything" posture
this should at least require `--force`.

### 2.7 Rolled-up Move/Copy named a different source on every run — FIXED 2026-09-09
`propagateNodeStatus` captured "the first Move/Copy child" while ranging `node.Children`, a
map, so Go's randomised iteration order decided which source a rolled-up line credited: the
same two snapshots reported `Move d/ <- c/c/b/c` on one run and `Move d/ <- e` on the next.
Found by the equivalence harness added with the merge-join tree builder, not by the property
test — both answers are complete and sound, so the invariants held; it is "wrong or
unstable", not the data-loss class. Fixed by ranging `sortedChildren`, the convention
`detectMovesCopies` and the collector already used, and pinned by
`TestDeterminism_RepeatedRunsAgree`.

---

## Severity 3 — scale and performance

### 3.1 Merkle strings dominate memory — FIXED 2026-09-09 (`internal/diff` and `internal/dupes`)
A directory's "hash" used to contain every descendant hash. Measured on a synthetic
depth-5 / 4096-file tree: 1,336,663 total bytes of `HashA`, largest single directory
string 196,603 bytes — ~326 B/file, growing with depth. Fixed by 2.2's digest change, in
both packages.

### 3.2 Diff peak memory — SOLVED 2026-09-10
Two identical 200,000-file snapshots used to retain 200 MiB (~1 KiB/node). A run of
constant-factor work brought that to **34.1 MiB / 178 B per node**: the 2.2/3.1 digest
fix, then dropping the pre-allocated `Children` map on leaves, replacing the stored `Path`
with a parent pointer, shrinking `Status` from a string to a `uint8`, and holding hashes
inline instead of as strings. [diff-memory.md](diff-memory.md) has the table and what was
left on the table deliberately. `TestMemory_UnifiedTree` pins it with a per-node ceiling.

The store side streams (ROADMAP 0.8.11's "memory use optimization"), but the whole unified
tree is still materialised, so this is a smaller constant on the same curve. A 10M-node
diff went from ~10 GiB to ~1.7 GiB, which is the difference between impossible and routine
on an ordinary machine; a 200M-node one still wants ~34 GB.

**This is the issue that stalled the project.** The author reported needing ~200 GB of
swap to diff real snapshots, which puts the working set at roughly 100-200M nodes.

**Solved** (2026-09-10): the streaming engine retains `O(depth x budget)` regardless of
file count, measured at 10.7x lower peak RSS than the tree engine on a real 2.27M-file
fleet diff with byte-identical output. It is what `--engine auto` picks whenever the input
can be read in DFS order, which is every diff against a snapshot in the DB.

Phase 3 (2026-09-10) removed the last restriction - move/copy detection, which needed a
whole-tree hash index, now runs as an external sort. What remains resident is the sort
buffer (a fixed 64 MiB) plus `O(depth x budget)`, so **peak RSS stops tracking the file
count entirely**: 1.6M files a side measured 215 MiB and 6.4M a side measured 210 MiB,
against a tree engine that extrapolates to ~1.5 GiB and ~6 GiB on the same inputs. The
200M-node case that stalled the project is now a temp-storage question (~20 GB, see
`--temp-dir`) rather than a memory one. See [diff-memory.md](diff-memory.md)'s Phase 3.

The tree engine is kept permanently: it is faster below a few million nodes, and it is the
oracle the streaming one is equivalence-tested against.

Not blocking the fleet work in the meantime: the `coverage` command (2026-08-23) answers
*"is it safe to delete this?"* without building the tree at all, in flat memory. Reach for
it whenever the question is a delete decision; `diff` remains the only way to ask what
changed and where it went.

### 3.5 `dupes`, `merge`, and `import` materialise whole snapshots
`GetFilesForSnapshot` / `GetFileList` load every row into a map or slice. `merge` only ever
iterates its input and could stream trivially. `dupes` cannot without restructuring its
tree build. [architecture.md](architecture.md).

---

## Severity 4 — usability and hygiene

- `flag` package semantics mean flags after positional args are silently ignored:
  `fluxion diff 1 2 --db x.db` runs with no `--db`. [cli.md](cli.md).
- `diff` exits 0 whether or not differences were found, so it cannot be used as a shell
  predicate.
- `progressbar.Default` writes to **stdout** in `merge`, `import`, `export-legacy`, and
  diff, contaminating pipeable output. [architecture.md](architecture.md).
- No TTY detection, no `--quiet`, no `--json`. (TTY detection is an open v1.0 item.)
- Migrations run silently with no prompt and no backup; there is no "DB is newer than this
  binary" check. (Prompting is an open v1.0 item.)
- `--cross-mounts` defaults to **true**, so scanning `/` walks into `/proc`, `/sys`, and
  network mounts. [scanner.md](scanner.md).
- `dupes.FindDuplicates` takes a `rootPath` parameter it never uses.
- Zero-byte files are excluded from diff move/copy matching but **not** from `dupes`.
- Hard links are recorded per-name, so `dupes` reports them as reclaimable space that is
  not reclaimable.
- Dead code: `internal/app/diff.go:209-212` is an `if` block containing only comments.
- `propagateStatus`'s doc comment is duplicated three times
  (`internal/diff/diff.go:263-268`); several files carry AI-authored deliberation comments
  that contradict the final code. [architecture.md](architecture.md).
- No CI, no linter config, no LICENSE. `gofmt` would move
  `internal/app/snapshot.go` and `internal/dupes/dupes.go`.

---

## Modernisation candidates

- ~~**Replace the synthetic merkle string with a real digest**~~ Done 2026-09-09, in both
  `internal/diff` and `internal/dupes` (issue 2.2/3.1).
- **Property-based tests for diff.** The existing tests are case-by-case golden output,
  which is why 1.1 and 1.2 went unnoticed. The invariant worth asserting is: *every file
  present in A and absent from B appears somewhere in the output.* All three severity-1
  diff bugs violate it.
- **A `Store` fake.** The interface exists but is never substituted, so every `app`-level
  test writes a real database to a temp directory.
