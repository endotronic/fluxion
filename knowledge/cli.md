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
