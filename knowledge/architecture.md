# Architecture

## Layering

```
cmd/fluxion/main.go        CLI layer: flag parsing only, one runX() per subcommand
        │                  builds an app.XConfig struct, calls app.RunX(cfg),
        │                  prints the error and os.Exit(1)
        ▼
internal/app/*.go          One file per subcommand. Owns DB opening, snapshot
        │                  resolution, progress bars, and ALL user-facing output.
        ▼
internal/store             store.Store interface (the seam)
internal/store/sqlite      the only implementation
        │
        ▼
internal/{diff,dupes,scanner}   pure algorithm packages, no DB, no printing
internal/{models,util,table,consts}   shared types and helpers
```

`main.go` carries an explicit architecture note stating this intent, and the code follows
it consistently. Every `app.RunX` has the same skeleton: validate config → open
`sqlite.NewSqliteStore(cfg.DBPath)` → `defer Close()` → `FindSnapshot(query)` → work →
print.

## The important seams

**`store.Store`** (`internal/store/store.go`) is the interface every `app` command talks
to. It is declared as `var _ store.Store = (*SqliteStore)(nil)` in the sqlite package.
Note that `app` commands hold it as `store.Store` but construct it concretely — there is
no injection, so the interface is not currently exercised by a fake in tests. If you want
DB-free tests for an `app` command, this is the seam to use.

**`diff.FileIterator`** (`internal/diff/diff.go`) is the seam that keeps the diff
algorithm free of the database:

```go
type FileIterator func(yield func(string, models.FileRecord) error) error
```

`app/diff.go` wraps `store.IterateFiles` in one of these, applying exclusion filtering and
absolute→relative path conversion on the way through. Tests use `mapToIter`. This is why
the diff is fully testable without cgo (see [build.md](build.md)).

**`internal/dupes`** takes a plain `map[string]models.FileRecord` instead — it is *not*
streaming, and the whole snapshot is materialised in memory before analysis. That
asymmetry with `diff` is deliberate but is also a scaling limit
(see [dupes-algo.md](dupes-algo.md)).

**`scanner.ScannerConfig.StopCh <-chan struct{}`** (added 2026-08-26) is the codebase's
first cancellation seam — before this, nothing in `internal/scanner` or `internal/app` could
be told to stop early; a scan ran to completion or the whole process was killed. Threaded
`ZFSScanConfig`(internal, created in `RunZFSScan`) → `SnapshotConfig.StopCh` →
`scanner.ScannerConfig.StopCh`, closed by `zfs-scan`'s SIGINT/SIGTERM handler before it
attempts to unmount anything. The walker checks it before descending and before every send
into the path-work channel (the latter matters: with idle workers already exited, a plain
channel send would otherwise block forever — see the comment at that call site in
`scanner.go`), and each worker selects on it. `RunSnapshot` returns the sentinel
`ErrScanInterrupted` and deliberately leaves the snapshot `in_progress` (never
`completed`/`failed`) when this fires — see [cli.md](cli.md) `## zfs-scan` for why a
retry-only fix wasn't enough here and what this replaced.

## Streaming vs. materialising — read the store methods carefully

`store.Store` offers three different ways to read files, with very different memory
profiles. Picking the wrong one is the main way to reintroduce the memory problems that
ROADMAP 0.8.11 set out to fix:

| Method | Shape | Memory |
|---|---|---|
| `IterateFiles(id, onFile)` | streams row by row | O(1) |
| `SearchFiles(id, pattern, cs, onFile)` | streams, SQL `LIKE` pre-filter | O(1) |
| `GetFilesForSnapshot(id, onProgress)` | `map[path]FileRecord` | O(snapshot) |
| `GetFileList(id, onProgress)` | `[]*FileRecord` | O(snapshot) |

Current users: `diff` and `export-legacy` stream. `dupes`, `merge`, `import` (DB→DB), and
snapshot **resume** all materialise. `merge` in particular loads each source snapshot
fully into a slice before writing it out, which is unnecessary — it only ever iterates.
(It does now also hold a `path → hash` map across *all* inputs, to detect and report
paths that several inputs disagree about; that part is inherent, the slice is not.)

## Output conventions (inconsistent — worth knowing before changing them)

- `logrus` → **stderr**, used for status/progress narration ("Comparing using strategy…").
  Configured in `main.go`'s `init()` with `FullTimestamp: true` (RFC3339), level Info —
  added so a `zfs-scan` run piped to a log file can be correlated to wall-clock time
  afterward; before 2026-08-24 timestamps were disabled entirely.
- `fmt.Print*` → **stdout**, used for actual results (diff lines, dupe groups, tables).
- Progress bars (`schollz/progressbar/v3`) → mostly stderr, but any bar built with
  `NewOptions`/`NewOptions64` and no explicit `OptionSetWriter` defaults to the library's
  own default, which is **stdout** (`progressbar.go`'s `writer: os.Stdout` in `NewOptions64`
  — `progressbar.Default(...)` is just that constructor with stderr set explicitly, despite
  the name). `merge`, `import`, `export-legacy`, and the diff's own progress bar all omit
  `OptionSetWriter` and so contaminate stdout with `\r`-driven redraws. That matters because
  it makes those commands unpipeable — a redirected `diff`/`merge` output file gets binary
  progress-bar noise mixed into the actual result lines, not just a noisy log.
- **`internal/app/snapshot.go`'s two bars are the one place this is fixed**: both are TTY-gated
  via `isatty.IsTerminal(os.Stderr.Fd())` (a `quiet` bool computed once near the top of
  `RunSnapshot`, fed through the local `progressWriter(quiet)` helper) — when stderr isn't a
  terminal the bar writer becomes `io.Discard` instead of spamming a piped log. Added because
  `zfs-scan` drives `RunSnapshot` unattended under `tmux`/`tee` across many datasets; the
  other commands listed above still have the plain-stdout-contamination bug.
- **Discarding the bar when `quiet` left a real gap**: with no bar and no substitute, a
  multi-hour piped scan produced zero output between the initial "Estimated scan size" line
  and the final "Finished" line — indistinguishable from a hang in a `tee`'d log. Fixed
  2026-08-24 by adding a `quiet`-gated 30-second heartbeat inside the same UI goroutine that
  drives the bar, logging via `logrus.Infof` ("Scanning... found N files" during the walk,
  "Hashing... N/M files done" afterward). TTY/interactive behavior is unaffected — the
  heartbeat channel stays `nil` (blocks forever in the `select`) whenever `quiet` is false.
  `SnapshotConfig.OnHeartbeat` (zfs-scan's overall-progress hook) has run on its *own*
  ticker, independent of this 30s one, since 2026-08-26 — a 5s tick, because 30s reads as
  "nothing is happening" to someone watching a multi-dataset run interactively, even though
  a piped log is fine checking in that rarely. See [cli.md](cli.md) `## zfs-scan`.
- **`snapshot.go`'s two bars deliberately do not use `progressbar.OptionFullWidth()`** (removed
  2026-08-26). Full-width mode recomputes the bar's *content* width from the live terminal
  width on every render, but the library's own line-clearing logic tracks the **maximum**
  content width ever rendered (`state.maxLineWidth`, unexported — never shrinks) and blindly
  overwrites with that many spaces before redrawing. Shrinking the terminal after the bar has
  rendered at a wider size makes that clear-string itself wrap onto a second terminal row; the
  trailing `\r` then only returns to the start of *that* row, not the bar's original line, so
  every subsequent redraw drifts and the terminal fills with corrupted, duplicated bar
  fragments until the process exits or the window is widened back out. This was reproduced
  live during a `zfs-scan` run under `sudo` — resizing the terminal mid-scan wedged the
  display this way. There is no public API to reset `maxLineWidth` without also resetting the
  bar's counters (`Reset()`), so the fix is to not let content width track terminal width at
  all: both bars now render at their original fixed `OptionSetWidth` (10/15) regardless of
  terminal size, which keeps `maxLineWidth` essentially constant and starves the bug of the
  width delta it needs. A fixed-width bar just looks fixed-width now instead of stretching to
  fill the terminal - a real but minor cosmetic regression traded for not corrupting the
  terminal on resize.
- Some errors go through `logrus.Errorf`, some through `fmt.Printf`, some are returned.
  `app/diff.go` does all three within the same function.

There is no `--quiet`, no `--json`, and TTY detection exists only for `snapshot`'s bars (see
above) — not applied anywhere else yet. Full TTY detection is an explicit v1.0 roadmap item,
and doing it properly requires first settling the stdout/stderr split above.

## Error-handling posture (important, and currently weak)

Several hot paths **swallow errors and continue**:

- `app/snapshot.go` collector: `if res.Error != nil { continue }` — every per-file scan
  error (permission denied, I/O error, file vanished mid-scan) is silently discarded.
- `BatchAddFiles` failures in `snapshot`, `merge`, `import`, `import-legacy` are logged
  and the loop continues, so a failed batch silently drops up to `DBBatchSize` (1000)
  records.
- `CompleteSnapshot` is called unconditionally, so a snapshot that failed to read half
  the filesystem is marked `completed` and is indistinguishable from a good one.

For a tool whose purpose is "prove my backup is complete", this is the highest-leverage
area to fix after the diff bugs. See [known-issues.md](known-issues.md).

## Package inventory

| Package | Responsibility |
|---|---|
| `cmd/fluxion` | flag parsing, subcommand dispatch, `arrayFlags` for repeated `--exclude` |
| `internal/app` | one `RunX(XConfig) error` per subcommand + `getUniqueSnapshotName` |
| `internal/models` | `Snapshot`, `FileRecord`, `SnapshotStatus` |
| `internal/store` | the `Store` interface |
| `internal/store/sqlite` | driver, schema + migrations, iterators, glob search |
| `internal/diff` | the tree/merkle/move-detection engine — see [diff-algo.md](diff-algo.md) |
| `internal/dupes` | highest-level duplicate detection — see [dupes-algo.md](dupes-algo.md) |
| `internal/scanner` | parallel walk + hash — see [scanner.md](scanner.md) |
| `internal/util` | `ParseSize`, `FormatBytes`, mount enumeration, `statfs` usage |
| `internal/table` | minimal fixed-width ASCII table for `list` |
| `internal/consts` | `Version`, `DBBatchSize=1000`, `ScannerChannelBufferMultiplier=1000` |

## Code-style note for anyone editing

Large stretches of this codebase were written by an AI agent and retain **the agent's
deliberation as comments** — comments that ask questions, weigh options, and describe
what the author was unsure about rather than what the code does. Examples:
`internal/diff/diff.go:846-867`, `internal/dupes/dupes.go:49-100`,
`internal/app/dupes.go:336-374`, `internal/app/snapshot.go:289-294`. Some are stale or
contradict the final code, and `propagateStatus` has its doc comment duplicated three
times (`diff.go:263-268`).

Treat these as archaeology, not documentation. The reasoning that is actually load-bearing
has been extracted into this knowledge folder; the in-code versions should be deleted or
replaced as files are touched.
