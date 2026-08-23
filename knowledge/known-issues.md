# Known issues

Findings from a full review of the code as of `v0.8.13`. Ordered by severity using the
rule from [goals.md](goals.md): **a bug that makes something look present/unchanged when
it is not can cost the user data; a bug that over-reports only costs reading time.**

Items marked **CONFIRMED** were reproduced with executed tests, not inferred from reading.

**Everything listed here is still open.** Issues fixed since the review have been removed
from this file rather than annotated — `git log` is the record of what was fixed. As of
2026-08-23 that is 1.1, 1.2, 1.4, 1.5, 2.1, 2.3, 3.3, 3.4, and 3.6, plus the cgo
modernisation item; the numbering of what remains is unchanged so earlier references still
resolve.

The diff rewrite that closed 1.1, 1.2 and 2.1 also added `internal/diff/property_test.go`,
which found and closed four further data-loss bugs that this review had missed entirely —
all of them cases where a move source or a lost directory went unmentioned after a rollup.
See [diff-algo.md](diff-algo.md) for what it asserts and why. **Anything you fix in
`internal/diff` should be fixed against that test, not against the golden output.**

---

## Severity 1 — silent data loss in diff output

### 1.3 `--exclude` over-excludes on non-boundary prefixes — CONFIRMED
`isExcluded` (`internal/app/diff.go:254`) tests `strings.HasPrefix(path, excl)` with no
path-separator check. Verified:

| exclude | also silently excluded |
|---|---|
| `data` | `/project/data2/file`, `/project/database/file` |
| `secrets.txt` | `secrets.txt.bak` |
| `backup` | `backup2/x` |

In a backup-verification tool this makes a diff look clean when it is not. Fix: require the
match to end at a separator or at end-of-string (`path == excl || strings.HasPrefix(path,
excl + string(os.PathSeparator))`). `scripts/verify.sh` already tests the over-exclusion
direction, but only with a name (`exclude_me`) that has no confusable sibling.

The same non-boundary `HasPrefix` appears in `createIter` (`internal/app/diff.go:113`) and
in `import-legacy`'s root autodetection (`internal/app/import.go`).

---

## Severity 2 — wrong or unstable results

### 2.2 Directory hashes are not injective (collision) — CONFIRMED
`computeMerkleHashes` joins `child.Name + ":" + child.Hash` with `,` and does not escape
either delimiter. A directory containing one file literally named `a:AA,b` with hash `BB`
produces `"a:AA,b:BB"` — identical to a directory containing `a`(`AA`) and `b`(`BB`). The
two directories then compare equal and one may be reported as a move or copy of the other.
`internal/dupes/dupes.go` has the same scheme and the same defect.

Fix (also fixes 3.1 and helps 3.2): SHA-1 the sorted, length-delimited child list and store
the digest.

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

---

## Severity 3 — scale and performance

### 3.1 Merkle strings dominate memory — CONFIRMED
A directory's "hash" contains every descendant hash. Measured on a synthetic depth-5 /
4096-file tree: **1,336,663 total bytes** of `HashA`, largest single directory string
**196,603 bytes** — ~326 B/file, growing with depth. Every map insert and comparison in
`detectMovesCopies` hashes those long strings. Fixed by 2.2's digest change.

### 3.2 Diff peak memory ~1 KiB/file — CONFIRMED
Two identical 200,000-file snapshots: **200 MiB retained heap**, 398 MiB total allocated.
The store side streams (that was ROADMAP 0.8.11's "memory use optimization"), but the
whole unified tree is materialised. A 10M-file tree would want ~10 GiB. 3.1 is the largest
single component.

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

- **Replace the synthetic merkle string with a real digest** (issue 2.2). Highest
  value-per-line change in the project; fixes a correctness bug and the dominant memory
  cost at once, and is a prerequisite for the "directory entirely contained in another"
  roadmap item.
- **Property-based tests for diff.** The existing tests are case-by-case golden output,
  which is why 1.1 and 1.2 went unnoticed. The invariant worth asserting is: *every file
  present in A and absent from B appears somewhere in the output.* All three severity-1
  diff bugs violate it.
- **A `Store` fake.** The interface exists but is never substituted, so every `app`-level
  test writes a real database to a temp directory.
