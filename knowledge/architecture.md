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
`scanner.go`), and each worker checks it in its own non-blocking `select` *ahead of* the
blocking receive on the path channel, every loop iteration — not just as one of two cases in
a single `select`, which would let Go's uniform-random tie-breaking between ready cases
leave a worker draining an already-queued backlog for a while instead of stopping on its
next iteration (found 2026-08-26; see [cli.md](cli.md) `## zfs-scan` for the report that
surfaced it). `RunSnapshot` returns the sentinel
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
  `SnapshotConfig.OverallLine` (zfs-scan's overall-progress hook, a pure `func(int64) string`
  - no printing side effects) is logged on its *own*, faster 5s ticker when stderr isn't a
  TTY, independent of this 30s heartbeat - 30s reads as "nothing is happening" to someone
  watching a multi-dataset run interactively, even though a piped log is fine checking in
  that rarely. On a real TTY it doesn't need its own ticker at all: see the next bullet.
  See [cli.md](cli.md) `## zfs-scan`.
- **The bar and `OverallLine` are rendered as one coordinated two-line block on a TTY, not
  two independent writers** (fixed 2026-08-26, after a report that the two visibly collided:
  the overall line's text landing glued onto the tail of the bar's `\r`-redrawn line, and a
  new permanent scrollback line appearing every 5s instead of a clean in-place second line).
  The root cause was two uncoordinated writers landing on the same terminal - the bar wrote
  newline-less `\r` redraws to stderr, `OverallLine`'s old equivalent was a plain
  `fmt.Println` to stdout on its own ticker; different file descriptors don't imply
  independent cursor state on a shared terminal. The fix (`internal/app/snapshot.go`,
  `dualLine`/`redraw()`/`linesReserved`): when `OverallLine != nil` and stderr is a TTY, the
  bar's writer becomes `io.Discard` (it never writes itself) and the UI goroutine instead
  pulls its rendered text via `bar.String()` and draws both lines together every 100ms tick,
  using `\x1b[J` (erase to end of screen) plus a cursor-up before each rewrite so the block
  always redraws in place regardless of the previous frame's length. One writer, one
  goroutine, nothing left to race. Off a TTY this doesn't apply - `OverallLine` just gets
  logged periodically instead (previous bullet); the plain `fluxion snapshot` path
  (`OverallLine == nil`) is untouched either way. See [cli.md](cli.md) `## zfs-scan`.
- **The `Scanning...` bar's own built-in ETA (`schollz/progressbar`'s `predictTime`) is
  disabled and replaced with a hand-rolled one** (fixed 2026-08-27, after a real-fleet report
  that the bracketed `[elapsed:ETA]` figure only ever showed the elapsed side moving - `ETA`
  itself sat at a flatly wrong `0s` for over an hour on a dataset dominated by a few huge
  files). Root cause, confirmed by direct reproduction against the vendored library: schollz
  samples throughput over a short (sub-10s) rolling window and divides by that window's rate
  to predict remaining time; if the window sees *zero* progress - routine here, since hashing
  one large file produces no `processedBytes` update for the entire time it's in flight - the
  zero-rate division overflows through a `float64`→`Duration` conversion into a large negative
  number, and the library's own "if negative, clamp to zero" guard then displays that as `ETA
  ~0s` for the whole stall, not as "unknown" or "long". `OptionSetPredictTime(false)` +
  `OptionSetElapsedTime(true)` keeps the library's (correct) elapsed-time display and drops its
  broken ETA; `estimateETA` (`internal/app/zfsscan.go`, shared with `overallProgress.line()`)
  computes a whole-run-so-far average (`done/elapsed` since the bar's own start, guarded the
  same way as the overall line: no estimate before 2 elapsed seconds or before any progress,
  remaining clamped to zero if `done` overshoots the current max) and gets appended to the
  bar's description text instead. A whole-run average can't hit the zero-rate case - elapsed
  and done only ever grow - at the cost of reacting to a real slowdown/speedup more slowly, an
  acceptable trade given the alternative is a number that reads as precise while sometimes
  being outright wrong. `barMax` (a local tracking `estimatedTotal` until `bar.ChangeMax64`
  fires, then the walk-discovered total) is threaded through alongside `barStart` so the ETA
  always divides against the same total the bar's own percentage is drawn against. This bug is
  independent of `dualLine`/`OverallLine` - it also affects a plain, non-`zfs-scan` `fluxion
  snapshot` run on large files, since it's inherent to the schollz library's rate calculation,
  not something introduced by the two-line coordination above.
- **The `Scanning...` bar's throughput figure (`X/s`) has the same failure mode as its ETA,
  and gets the same fix** (2026-08-27, same report - "I thought I saw a rate earlier, but now I
  don't"). schollz's own `showBytes`-driven rate segment (inside the `(current/max, rate)`
  parens) is computed from the identical short rolling-window `averageRate` that broke
  `predictTime` - but instead of dividing by a zero rate and showing something wrong, the
  library just omits the segment entirely whenever `averageRate <= 0` (`if c.showBytes &&
  averageRate > 0 { ... }`), so the rate silently vanishes mid-scan during any stall (a large
  file being hashed) and reappears once small-file throughput resumes. `averageRate` (the
  `internal/app/zfsscan.go` helper `estimateETA` now calls internally) is used directly for
  this too: a whole-run-so-far `done/elapsed` figure, appended to the bar's description
  alongside the ETA as `(X/s avg, ETA ~Y)` - always present once there's 2s of elapsed time and
  any progress, never blank. `OptionShowBytes(true)` is left on the bar config as before (it
  still drives the humanized `(current/max)` byte formatting in schollz's own segment); its own
  rate figure inside that segment is simply superseded, not actively disabled - there is no
  public option to turn off only the rate half of `showBytes` without also losing the byte
  formatting, so it may still opportunistically show its own (unreliable) number too on top of
  ours. That's a minor cosmetic redundancy, not a correctness problem: ours is always accurate
  and always present, which is what matters.
- **"Found N/Y" (in the bar's description, during the walk phase) and "(seen/max)" (in the
  bar's own parens) are two different counters, not a before/after of the same ratio** - easy
  to misread as related. "Found N/Y" is the **walker's** running tally: N is the count of files
  the directory walk has discovered so far, Y is their total size - pure enumeration progress,
  independent of hashing. "(seen/max)" is the **bar's own** progress: `seen` is
  `processedBytes` (bytes actually hashed and recorded to the DB so far), `max` is
  `estimatedTotal` (the pre-scan FS-usage estimate) until the walk finishes, then
  `bar.ChangeMax64` swaps it to the walk's actual found-total. Because hashing runs
  concurrently with walking (a streaming pipeline, not walk-then-hash), it's normal and
  expected for `seen` to differ from `Found`'s Y in either direction while walking is still in
  progress.
- **`redraw()`'s dual-line block stopped landing in place and instead scrolled a fresh copy of
  itself down the terminal every ~100ms** (fixed 2026-08-27, real-fleet report at `--threads 4`:
  "the updates are no longer in place; the progress rapidly creates many newlines"). Two
  independent bugs, both in `redraw()`, both required to reproduce:
  1. **The rate/ETA text added by the previous two bullets made the bar's description long
     enough to wrap.** `linesReserved` (the cursor-up distance for the next frame) was hardcoded
     to `2`, implicitly assuming each of the bar line and `OverallLine` fits in one physical
     terminal row. With `[100% Saturated]` plus the new `(X/s avg, ETA ~Y)` suffix, the bar
     line can run 150+ visible characters — comfortably wider than an 80- or 100-column
     terminal — so it wraps onto 2 rows, `linesReserved` under-counts, and each redraw's
     cursor-up/erase starts one row too low, permanently under-erasing. Fixed with
     `truncateLine` (`snapshot.go`): both lines are clipped to the real terminal width (via
     `golang.org/x/term.GetSize` on `os.Stderr`, re-read every frame since the terminal can be
     resized mid-scan) before being written, so a line can never wrap and `linesReserved == 2`
     stays true. Width `<= 1` (not a real terminal, or the ioctl failed) leaves both lines
     untouched — matches the pre-existing behavior for a non-terminal `os.Stderr`.
  2. **Even with wrapping ruled out, the erase strategy itself was wrong.** The prior code
     issued a single `\x1b[J` (erase-to-end-of-*screen*) right after the cursor-up/`\r`, before
     writing either line. Reproduced directly against a real terminal (tmux,
     `TERM=xterm-256color`) with a trivial fixed-width two-line block and no bar/schollz
     involved at all: `\x1b[1A\r\x1b[J` followed by new content caused every redraw to be
     appended as a new pair of lines instead of overwriting the previous pair — despite the
     cursor math being individually correct (confirmed by dumping the raw bytes via `tmux
     pipe-pane`). Swapping the *order* of `\x1b[1A` and `\r`, or replacing `\x1b[1A\r` with the
     equivalent combined `\x1b[1F` (CPL), made it work; adding `\x1b[J` back in either of those
     forms broke it again — so the erase call itself, not cursor positioning, was the actual
     cause. No further explanation for *why* `\x1b[J` misbehaves here was pursued (a tmux/VTE
     implementation quirk, not a spec violation worth chasing) - the fix was to stop using it.
     `redraw()` now erases per-line instead: `\x1b[K` (erase-to-end-of-*line*) written
     immediately after each line's content, rather than one screen-wide erase before either
     line. This is also more targeted (only clears the row just written, not everything below
     the cursor) and was confirmed clean over 15+ frames at both 80- and 60-column widths with
     the worst-case (`Saturated` + rate + ETA) description text.
  Point 1 alone (wrapping) could not reproduce the bug without point 2 (`\x1b[J`) also present -
  a `wrappedRows`-style fix that only corrected the cursor-up *distance* to account for wrapped
  lines was tried first and still scrolled, which is what surfaced the `\x1b[J` behavior. If
  `redraw()` needs further changes, re-verify empirically against a real terminal (e.g. via
  `tmux new-session -d -x <cols> -y <rows>` + `tmux capture-pane`), not just by re-reading the
  escape sequences - this bug looked correct on paper at every step until it was actually run.
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
