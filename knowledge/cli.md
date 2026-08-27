# CLI reference and file formats

Dispatch lives in `cmd/fluxion/main.go`: one `case` per command, one `runX()` per command
building an `app.XConfig`. Every command uses its own `flag.FlagSet`, so **flags must come
before positional arguments** (Go's `flag` package stops parsing at the first
non-flag argument). `fluxion diff 1 2 --db x.db` silently ignores `--db`.

`--db` is required almost everywhere and has no default. "Default DB in `~/.fluxion`" is
an open v1.0 roadmap item.

## Commands

| Command | Aliases | Purpose |
|---|---|---|
| `snapshot` | `snap`, `scan`, `s` | scan a directory into a new snapshot |
| `list` | `l` | table of snapshots; the status column appends `(N errors)` for an incomplete scan |
| `delete` | `x` | tombstone a snapshot and drop its file rows |
| `diff` | `d` | compare two snapshots |
| `dupes` | `z` | find duplicates within one snapshot |
| `merge` | `m` | combine snapshots into a new one |
| `import` | `i` | copy snapshots from another Fluxion DB |
| `import-legacy` | — | ingest `dupe-finder` flat files |
| `export-legacy` | — | write `dupe-finder` flat files |
| `size` | — | total bytes of a snapshot |
| `find` | — | search a snapshot by filename |
| `coverage` | `c` | is every file of one snapshot present, by content, in others? |
| `zfs-scan` | `zscan`, `zs` | enumerate every dataset under a ZFS root and scan each into its own snapshot |
| `version` | `v` | print `consts.Version` |

## snapshot

```
fluxion s --db <db> [--name N] [--dir D] [--threads N] [--md5]
         [--new] [--resume <id|name>] [--hostname H]
         [--cross-mounts=false] [--fail-on-mount]
         [--skip-estimation] [--estimate] <dir>
```

- Root may be given positionally or via `--dir`.
- `--db` defaults to `<dirname>.db`; `--name` defaults to `<dirname>_<date>`.
- `--threads` defaults to `runtime.NumCPU()`.
- `--cross-mounts` defaults to **true** — it will walk into other filesystems. See
  [scanner.md](scanner.md).
- Without `--new`, an existing `in_progress` snapshot for the same root is offered for
  resume.
- `--estimate` does the filesystem-usage estimate (`internal/util/fs_usage.go`) and exits.

## diff

```
fluxion d --db <db> [-u|--update] [-e|--exclude PATH]... 
         [--no-copies] [--no-moves] [--show-unchanged] [--max-lines N] <A> <B>
```

`A` and `B` are snapshot IDs or names. Output symbols:

| Symbol | Status |
|---|---|
| `[+]` | Added (in B only) |
| `[-]` | Removed (in A only) |
| `[M]` | Modified |
| `[>]` | Move (with `(from ...)`) |
| `[C]` | Copy (with `(from ...)`) |
| `...` | Truncation summary: `... 8 more under many/ (8 modified)` |

- `--update` filters to what A has that B lacks or has differently — it drops `Added`,
  `Move`, and `Copy` rows, keeping `Removed` and `Modified`. Read it as **"what would I
  still have to copy from A to B?"**, which is the same question as **"is it safe to
  delete A?"**: if `diff --update A B` prints nothing, every file in A is present in B
  with identical content, and A's copy is redundant. It is a filter on the *printed
  rows*, not a different comparison, so the roll-up rules apply to it unchanged.
  Truncation summaries are never filtered out — what they hide may be exactly what the
  mode is looking for.
- `--max-lines N` (default 25, `0` = unlimited) caps how many lines any one directory may
  print; the rest become a single `... N more under dir/` line carrying their combined
  counts. It applies per directory, not to the run as a whole — the top level is exempt.
  A directory containing unchanged files may not be collapsed (that would claim something
  about files that did not change), so this is the only lever that shortens such a block.
- `--exclude` is repeatable (`arrayFlags`). **It currently matches by raw
  `strings.HasPrefix` with no path-boundary check**, so `--exclude data` also excludes
  `data2/` and `database/`. Confirmed bug; see [known-issues.md](known-issues.md).
- `--no-moves` / `--no-copies` disable the corresponding half of `detectMovesCopies`;
  setting both skips the stage entirely.

## dupes

```
fluxion z --db <db> [--min-size 1MB] [--limit N] <snapshot>
```

`--min-size` parses suffixes via `util.ParseSize` (`10M`, `1G`, …) and defaults to `1MB`.
It applies to a directory's **total** size. Groups are sorted by wasted space
(`(count-1) * size`) descending, and a total is printed. See [dupes-algo.md](dupes-algo.md).

## find

```
fluxion find --db <db> [-s|--case-sensitive] [-E|--regex] <snapshot> <pattern>
```

Glob by default (`filepath.Match` semantics), regex with `-E`. Globs get a SQL `LIKE`
prefilter in `sqlite/search.go` before Go-side verification; regex matching happens
entirely in Go. Note: `--case-sensitive` is **ignored in regex mode** — the pattern is
compiled as given, with no `(?i)`.

## coverage

```
fluxion c --db <db> [--min-size 1M] [--limit N] [--by-dir]
         [-e|--exclude PATH]... <candidate> <keeper>...
```

**The delete predicate.** Answers "if I destroy `<candidate>`, is every byte of it still
somewhere in `<keeper>...`?" — the question the tool exists for, and the reason it is a
separate command from `diff --update`: it is answered by a SQL semi-join in O(1) memory,
with no unified tree, no merkle hashes, and no move/copy matching. See
[fleet.md](fleet.md) for the job it was built for and
[diff-memory.md](diff-memory.md) for why the diff route could not be used.

- **Content only.** It compares hashes and ignores paths entirely, which is what makes it
  right for the "same tree, reorganised, replicated to another host" case that `diff`
  reports as thousands of moves. It says nothing about paths, nothing about which side is
  newer, and nothing about files the keepers have that the candidate does not.
- Multiple keepers are a **union**: a file is covered if its content appears in any one of
  them. Keepers may be in any state; naming the candidate as its own keeper is rejected.
- The hash is negotiated across the candidate and every keeper — SHA-1 if they all have it,
  otherwise MD5 (legacy imports), otherwise an error. Printed in the header so the answer
  is never ambiguous about what it compared.
- **A file with no comparable hash counts against coverage**, listed separately in the
  summary as `no hash`. Per [goals.md](goals.md), absent evidence is not evidence of
  absence.
- `--min-size` (`util.ParseSize`, default 0) skips small files entirely; they are reported
  as skipped, never as covered.
- `--limit` (default 50, `0` = all) caps the **listing** only. The totals in the summary
  always count everything.
- `--by-dir` aggregates the listing to one line per containing directory, streamed — use it
  when a whole subtree is missing and the per-file list would be noise.
- Incomplete (`in_progress` / `failed`) snapshots are warned about on stderr but still used;
  an incomplete *keeper* is the dangerous direction and is called out as such.

Exit status is meaningful: **0** = fully covered, **2** = something would be lost, **1** =
error. This is the only command that can be used as a shell predicate:

```bash
fluxion c --db f.db artemis_deprecated luna_kevin && zfs destroy artemis/deprecated
```

## zfs-scan

```
fluxion zs --db <db> [--threads N] [--md5] [--dry-run]
          [--include-canmount-off] [--new]
          [--exclude-dataset NAME]... <pool-or-dataset>...
```

Drives `snapshot` (`--cross-mounts=false`, non-interactive) once per dataset under the
given root(s), instead of the user running it dataset-by-dataset by hand. Each snapshot is
named after its full ZFS dataset name (e.g. `artemis/deprecated/zalt`). This is the way to
do the per-dataset pass described in [fleet.md](fleet.md).

**Live datasets only.** It runs `zfs list ... -r <roots>`, then, for every dataset that
isn't skipped, mounts a *fresh, isolated copy* of it at a temporary directory
(`os.MkdirTemp` + `zfsutil.MountAt`) and scans that, then unmounts and removes the
temporary directory. It never reads `.zfs/snapshot/*` or ZFS's own snapshot history — see
the "Not filesystem-aware" note in [goals.md](goals.md); this is a mount-and-walk driver
for a present-moment scan, not the rejected per-ZFS-snapshot scanning feature.

**Every dataset is mounted this way, even ones already mounted at their usual location.**
This is deliberate, not just a fallback for datasets without a `mountpoint`. `MountAt` uses
the generic `mount -t zfs -o zfsutil <dataset> <path>` instead of `zfs mount` (which only
knows how to mount a dataset at its own `mountpoint` property, refuses when that property is
`none`, and refuses outright if the dataset is already mounted anywhere) — `-o zfsutil`
registers the mount with ZFS the same way `zfs mount` would, and ZFS-level properties
(notably `readonly`) are still honored, but the dataset's `mountpoint` property is never
read or written, so persistent ZFS configuration is untouched. Two benefits fall out of
mounting this way unconditionally:
  - A brand-new, empty temporary directory can never already have anything nested inside
    it, so the scanner's mount-boundary logic never has to reason about the fleet's real
    mount layout — `--cross-mounts=false` becomes a formality rather than something doing
    real work.
  - A dataset's live mountpoint can have files underneath it that are invisible in the
    normal tree because a child dataset happens to be mounted at that same path, shadowing
    them (e.g. leftover content in `artemis/deprecated` from before `artemis/deprecated/zalt`
    became its own dataset mounted on top of it). Mounting the parent fresh, on its own,
    with nothing else mounted inside, surfaces that content instead of silently missing it —
    which matters for a tool whose job is proving a tree is safe to delete.
- Unmounting is done **by path** (`umount <path>`, `zfsutil.UnmountPath`), not by dataset
  name (`zfs unmount <dataset>`): since the same dataset may now be mounted twice at once
  (its usual location plus zfs-scan's temporary copy), a name-based unmount would be
  ambiguous about which instance to tear down. Unmounting the exact path zfs-scan mounted
  has no such ambiguity, and only that mount is ever affected.
- The temporary directory is removed with `os.Remove` (never `os.RemoveAll`) only after its
  unmount succeeds, so a failed unmount just leaves the directory behind rather than risking
  a recursive delete into anything still mounted there.

Per-dataset skip reasons (printed, and folded into `Skipped` — not treated as failures):
`not a filesystem (zvol)`, `canmount=off` (container dataset, no files of its own — unless
`--include-canmount-off` is set, see below), `excluded`. A dataset already scanned to
`completed` under its own name is silently reported
as already done, making re-runs of the same root idempotent and cheap — that DB check
happens *before* any mount attempt, so a re-run mounts nothing for datasets it's already
scanned.

**A dataset interrupted mid-scan (`in_progress`) is resumed by name, not by mount path, and
this only works because `RunZFSScan` passes `ResumeFrom: <dataset name>` into `RunSnapshot`
explicitly.** Every zfs-scan invocation mounts each dataset at a brand-new
`os.MkdirTemp` path, so `RunSnapshot`'s own implicit-resume detection — which looks up
`GetLastSnapshot(TargetDir)` by root path — can never find a snapshot left `in_progress` by
an earlier, interrupted run; that row was recorded against a temp path that no longer
exists. Without the explicit `ResumeFrom`, a rerun falls through to "start new" and then
fails outright on the name collision (`getUniqueSnapshotName`'s exact-name check). This was
a real, confirmed bug until this was added — an interrupted dataset was permanently stuck,
failing identically on every rerun. Resuming by name is correct because zfs-scan always
names a dataset's snapshot after the dataset itself, which is stable across runs even though
the mount path isn't.

Counted as **failures** (`Failed`, and the run's exit code): a mount that errors (permission,
degraded pool, or any other `mount -t zfs -o zfsutil` failure — including for a dataset
that's already mounted elsewhere and perfectly healthy there), a scan error, or a dataset
whose most recent snapshot by that name is `failed` (blocked until that snapshot is deleted
— a silent skip here is exactly the kind of false "safe" answer [goals.md](goals.md)'s
severity rule warns about). One failing dataset does not abort the run; everything else
still gets scanned.

- `--dry-run` prints the full per-dataset plan (mount+scan, noting when a dataset is
  already mounted elsewhere / already scanned / skip \<reason\>) with **no mounting, no
  scanning, no DB writes at all** — review this before pointing it at a whole pool.
- `--exclude-dataset` is repeatable and boundary-aware (exact name, or `name/` prefix) —
  unlike the raw-`strings.HasPrefix` `--exclude` bug on `diff`/`coverage`, `luna/kevin`
  does not also exclude `luna/kevin2`.
- A dataset already mounted at its usual location gets logged (`logrus.Infof`) when zfs-scan
  mounts a second, temporary copy of it — worth knowing since it's the more surprising case
  operationally (a live, in-service mount now briefly has a second, independent mount of the
  same content elsewhere too).
- `--include-canmount-off` mounts and scans `canmount=off` datasets instead of skipping
  them. `canmount=off` conventionally means "not mountable via `zfs mount`, ignored by `zfs
  mount -a`" — but that restriction is enforced by the `zfs` CLI/libzfs, not the kernel or
  the `mount.zfs` helper, so the same `mount -t zfs -o zfsutil` bypass `MountAt` already uses
  for every dataset works for these too. Off by default because `canmount=off` is normal for
  purely organizational parent datasets with no files of their own; opt in when a fleet has
  `canmount=off` datasets that do hold real content worth scanning.
- `--new` starts a completely fresh snapshot for every dataset, ignoring any
  `completed`/`failed`/`in_progress` snapshot already recorded under its name — the usual
  skip/resume/blocked-by-failure logic above is bypassed entirely. Because `snapshots.name`
  is unique in the schema, the *existing* row (whatever its status) is renamed to
  `<name>_superseded_<old id>` before the new one is created under the clean name — kept as
  history, not deleted or overwritten. `FindSnapshot`/`GetLastSnapshot` name lookups only
  ever see the newest generation from then on; the superseded row is still reachable by ID
  or by its renamed value. Running `--new` again re-supersedes whatever currently holds the
  name, so repeated `--new` runs just keep growing the history chain.
- `SIGINT`/`SIGTERM` mid-run trigger cleanup instead of leaving mounts orphaned: a handler
  installed for the whole run (skipped for `--dry-run`, which never mounts anything) logs a
  warning, unmounts and removes every temporary directory currently mounted (the same
  `cleanupMounts` helper normal end-of-run teardown uses), and exits with the conventional
  `128 + signal number` (130 for `SIGINT`, 143 for `SIGTERM`). A **second** signal while
  cleanup is in flight force-exits immediately, in case `umount` itself is stuck on a wedged
  or degraded mount. Whatever dataset was mid-scan when the signal arrived is left
  `in_progress` in the DB exactly as before — the resume-by-name behavior described above is
  what makes rerunning after a `^C` actually pick it back up instead of getting stuck.
  - **The scan itself is cancelled before unmounting is attempted** (`StopCh`, added
    2026-08-26, superseding an earlier retry-only fix that turned out not to be enough). A
    first attempt at this fixed only the transient case — retrying `umount` up to 5 times,
    1s apart — on the theory that a process which had just finished scanning could still be
    holding the mount open for a brief moment (a walker/hasher goroutine unwinding, or the
    kernel not yet having dropped the last open file reference). That's real, but it's not
    what a live fleet run actually hit: on `^C`, the dataset **currently being scanned**
    keeps scanning — `RunSnapshot` had no way to be told to stop — so its mount stays
    genuinely busy for as long as that dataset's scan would otherwise take, not briefly.
    5 retries over ~4s can't win a race against a scan with no bound on how long it keeps
    running. Confirmed against a real run: `luna/historian/arctic_shift`'s scan kept
    reporting new bytes hashed for 4+ seconds *after* the interrupt handler had already
    started retrying its unmount, so every attempt failed identically.
    - The actual fix threads a `StopCh <-chan struct{}` from `ZFSScanConfig` through
      `SnapshotConfig.StopCh` into `scanner.ScannerConfig.StopCh`
      (`internal/scanner/scanner.go`): the walker checks it before descending (returns
      `filepath.SkipAll`) and before every send into the path-work channel (a plain send
      there would otherwise deadlock forever once idle workers have already exited — see
      the comment at that call site), and each worker selects on it alongside its normal
      work-channel read. `RunZFSScan`'s interrupt handler closes this `stopCh` *before*
      calling `cleanupMounts`, so by the time the retry loop runs, the scan is actually
      winding down instead of still fighting for the same file descriptors — retrying is
      now papering over a real but bounded delay (worst case roughly one in-flight file's
      hash time; a worker doesn't abort a read mid-file) instead of an unbounded one.
    - `RunSnapshot` returns the sentinel `ErrScanInterrupted` when `StopCh` fires, and
      deliberately does **not** call `CompleteSnapshot` or `FailSnapshot` in that case — the
      snapshot row is left `in_progress` so the resume-by-name path above picks it back up
      on the next run. Marking an interrupted, partial scan as anything other than
      `in_progress` would be exactly the false "safe" signal
      [goals.md](goals.md)'s severity rule warns about. `RunZFSScan`'s scan loop checks for
      this sentinel and stops iterating the remaining `toScan` datasets immediately, rather
      than uselessly attempting (and instantly self-cancelling) each of them in turn while
      the interrupt goroutine's cleanup is already in flight.
    - `unmountWithRetry` (5 attempts, 1s apart, ~4s worst case) is kept as-is on top of this
      — it still matters for the ordinary end-of-run case and for the brief post-scan window
      the original fix targeted. `unmountRetries` / `unmountRetryDelay` are package-level
      `var`s (not `const`s) purely so tests can shrink the delay instead of sleeping for real
      seconds. `cleanupMounts` is shared by normal end-of-run teardown and the interrupt
      handler, so both benefit from the retry.
    - **`StopCh` alone was still only probabilistically prompt, not deterministically so**
      (found 2026-08-26, from a report that a scan kept visibly hashing new files for ~39s
      after `^C`, with the terminal scrolling a new line every ~5s throughout — the second
      symptom turned out to be the display bug described below, but the first sent a real
      look at this). Each worker's loop was `select { case path := <-paths: ...; case
      <-cfg.StopCh: return }`. Once the walker has raced ahead and filled the `paths` buffer
      (`NumWorkers * consts.ScannerChannelBufferMultiplier`, easily thousands of entries),
      that `select` has *two* ready cases as soon as `StopCh` closes — Go picks between
      multiple ready cases uniformly at random, not in source order — so a worker only gave
      up after happening to win that coin flip, rather than noticing `StopCh` on its very
      next loop iteration. In expectation this only costs a couple of extra files per worker
      (geometric with p=0.5), which is why it's an unlikely full explanation for a
      continuous, 39-second-long burst of new activity — but it was still a real gap between
      the code and its own doc comment's claim ("stops within roughly one in-flight file's
      hash time"). Fixed by checking `StopCh` in its own non-blocking `select` *ahead of* the
      blocking one, every loop iteration, so it wins deterministically the moment it's
      closed: a worker now finishes at most the one file already in flight (if any) and no
      more, never a random slice of the queued backlog. See
      `TestRunScan_StopChStopsPromptly` in `internal/scanner/scanner_test.go`, which asserts
      an upper bound on files processed after `close(stopCh)`, not just that `RunScan`
      eventually returns.
- **No progress bar in a piped/logged run** (e.g. `zfs-scan ... | tee log`): the progress
  bar writer is discarded whenever stderr isn't a TTY (see
  [architecture.md](architecture.md) "Output conventions"), and `RunSnapshot` fills that gap
  with a periodic `logrus.Infof` heartbeat every 30s instead — expect status lines, not a
  redrawing bar, when this command's output is redirected.
- **A run-wide overall-progress line, printed on its own, separate from each dataset's own
  progress bar/heartbeat.** `RunSnapshot`'s own bar only ever knows about the one dataset
  it's currently scanning; without a second line there was no way to tell how far through a
  multi-dataset run you were or estimate when it would finish. `overallProgress`
  (`internal/app/zfsscan.go`) renders `-- overall: K/N datasets done, X / Y (P%), elapsed E[,
  ETA ~T]` at the start of each dataset, repeatedly while a dataset is mid-scan, and again
  once it finishes. It's driven by `SnapshotConfig.OverallLine func(processedBytes int64)
  string`, a pure text-producing callback plumbed through `RunSnapshot` for this purpose —
  see below for why it deliberately has no printing side effects of its own.
  - **On a TTY, this line and the dataset's own bar are rendered together as one
    cursor-coordinated two-line block, redrawn in place** (`internal/app/snapshot.go`'s UI
    loop, `redraw()`/`dualLine`/`linesReserved`; fixed 2026-08-26). Before this, they were
    two independently-scheduled, independently-streamed writes: the bar wrote `\r`-prefixed,
    newline-less redraws to **stderr**, while the overall line was a plain `fmt.Println` to
    **stdout** on its own 5s ticker. Two file descriptors landing on the same terminal have
    no cursor coordination between them — the overall line's text appeared wherever the
    bar's cursor happened to be (visually glued onto the tail of the bar, e.g.
    `...15h1m34s]-- overall: 3/27 datasets done...`), and because it always carried a hard
    newline, it permanently ended that terminal row every time it fired — forcing the bar to
    start over on a fresh line for its next redraw instead of updating in place. This read as
    "creating a newline at a consistent interval" and, together with the *next* bar redraw
    line, as the scan "still going" even when it may already have stopped. The fix: when
    `OverallLine` is set and stderr is a TTY (`dualLine`), the bar's own writer is set to
    `io.Discard` — it never writes anywhere on its own — and the single UI goroutine that
    already owns the bar's state instead reads it back out via `bar.String()` (the bar
    library's "current rendered text, nothing written" accessor) and draws both lines
    together on every 100ms tick: move the cursor up `linesReserved-1` rows, `\r`, erase to
    end of screen (`\x1b[J`, so a frame shorter than the last one can't leave stale
    characters), write the bar line, `\n`, write the overall line, no trailing newline. Since
    there is now exactly one writer for that whole region, on one goroutine, there is nothing
    left to race. A final redraw plus one real `\n` on completion leaves both lines in
    scrollback exactly once. This ANSI dance only happens when `dualLine` is true; the plain
    `fluxion snapshot` single-bar path (`OverallLine == nil`) is completely untouched.
  - **Off a TTY (piped/logged), `OverallLine`'s text is logged via a plain `fmt.Println` on
    its own 5s ticker instead** — the ANSI cursor management above would just be unreadable
    noise in a piped log, and there's no bar to coordinate with anyway (its writer is
    already discarded whenever stderr isn't a terminal, same as always). This 5s cadence is
    deliberately separate from the `quiet` piped-log heartbeat's 30s cadence (that split was
    the fix for an earlier, distinct complaint — "no progress shown of the total scan" — 30s
    reads as dead air to someone watching a TTY, though 30s is fine for an unattended log; on
    a TTY the overall line no longer needs its own ticker at all, since dual-line mode
    redraws it together with the bar on every 100ms tick).
  - **Weighted by ZFS's `referenced` property** (`zfsutil.Dataset.Referenced`, the `zfs list`
    column named `referenced`), not a flat per-dataset count — a small quick dataset
    finishing shouldn't read as meaningful progress next to a multi-terabyte one still
    running. `referenced` is physical, on-disk bytes (post-compression) reachable through
    that dataset's own filesystem right now, deliberately mirroring the existing convention
    that each dataset's own progress bar is *also* scaled against an on-disk figure
    (`util.GetFSUsage`), while the numerator in both cases (`processedBytes`) is the
    *logical* size of scanned files — the same pre-existing logical-vs-physical mismatch,
    just kept consistent at both levels rather than fixed or newly introduced. Expect the
    percentage to be an estimate, not an exact fraction, especially on heavily compressed
    datasets.
  - **This was `used` until a real-fleet run caught it as wrong (2026-08-27).** `used` is
    cumulative down the tree — a parent's `used` already includes every descendant's usage —
    so summing it across every `eligible` dataset (which spans both parents and children,
    since zfs-scan scans each one independently) multiply-counted the same physical bytes
    once per level of nesting. On the author's `luna` pool this reported an overall total of
    205.6T against a pool that only holds 69.7T. `referenced` doesn't have that problem: a
    container dataset with no files of its own reports it near zero, so it contributes
    nothing to the total beyond what its own scan will actually find. See
    `zfsutil.Dataset.Referenced`'s doc comment.
  - **The total covers every `eligible` dataset** — everything except the permanent skips
    (`excluded`, zvol, `canmount=off` without `--include-canmount-off`) — not just the
    datasets this invocation still has left to mount and scan (`toScan`). Datasets already
    `completed`, or blocked by a prior `failed` snapshot, are folded into the "done" side of
    `K/N` and `X/Y` bytes *before* the scan loop even starts, via a `doneCount` offset and
    `progress.datasetDone` calls made during the initial DB-status pass. This was a fix
    (2026-08-26) for the original version, which scoped totals to `toScan` only — on a
    resumed/repeated run against a mostly-already-scanned root, that made the overall line
    show a misleadingly small denominator (and no visible progress at all) instead of an
    honest "how far through this whole root" figure. It's also why issue reports like "no
    progress shown for the total scan" on a run with many already-`completed` datasets are
    now addressed: those datasets show up in the very first overall line, not just as
    individual `skip` entries.
  - A dataset that fails still counts as "no longer remaining work" once it's done — ETA is
    about time, not success.
  - The percentage/ETA fields are omitted entirely when `totalBytes` is 0 (nothing in this
    run reported a nonzero `referenced`, e.g. `zfs list` couldn't parse it — see
    `Referenced`'s doc comment in `internal/zfsutil/zfsutil.go`), and ETA specifically is also withheld for the
    first 2 elapsed seconds of the run (too little signal to divide by) and clamped to `~0s`
    rather than going negative if `done` overshoots `totalBytes` (possible precisely because
    of the logical-vs-physical mismatch above).

Exit status: **0** = every dataset scanned or expectedly skipped, **1** = at least one
dataset failed (mount/scan error, or blocked by a prior failed snapshot) — same discipline
as `coverage`'s exit 2.

## merge / import

`merge` builds a new snapshot from two or more existing ones in the same DB (`--name`
required). `import` copies snapshots between DBs (`--source` DB, `--all` or named
snapshots). Both materialise each source snapshot in memory and both log-and-continue on
batch-insert failures.

## Legacy `dupe-finder` format

Two parallel flat files, both with a **double-space** separator and base64-encoded paths
(so arbitrary bytes in filenames survive):

```
hashes file:   MD5HEX  utf-8  BASE64_PATH
sizes file:    SIZE    utf-8  BASE64_PATH
```

`import-legacy`:
- `--sizes` is inferred by replacing `hashes` with `sizes` in the hashes filename if that
  file exists; without it, everything imports with size 0.
- `--root` defaults to `/`, which triggers **root autodetection**: it streams the file
  computing the longest common directory prefix, then rewinds. The prefix shrink uses a
  raw `strings.HasPrefix`, so it can pick a prefix that is not a path boundary.
- The snapshot name defaults to the hashes filename with `_files_hashes.txt` /
  `_hashes.txt` / `.txt` stripped.
- Imported snapshots have **MD5 only** — no SHA-1, no mtime. They can only be diffed
  against snapshots that also carry MD5 (i.e. scanned with `--md5`).

`export-legacy` writes the pair back out, emitting the sizes file only when the snapshot
`HasSizes()`. It **overwrites existing output files without asking**.

## Exit codes and output streams

Errors print and `os.Exit(1)`. There are no distinct exit codes — notably, `diff` exits 0
whether or not differences were found, so it cannot be used as a shell predicate. Results
go to stdout, logrus narration to stderr, and some progress bars incorrectly go to stdout;
see [architecture.md](architecture.md).
