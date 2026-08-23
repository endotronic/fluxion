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
         [--no-copies] [--no-moves] [--show-unchanged] <A> <B>
```

`A` and `B` are snapshot IDs or names. Output symbols:

| Symbol | Status |
|---|---|
| `[+]` | Added (in B only) |
| `[-]` | Removed (in A only) |
| `[M]` | Modified |
| `[>]` | Move (with `(from ...)`) |
| `[C]` | Copy (with `(from ...)`) |

- `--update` filters to what A has that B lacks or has differently — it drops `Added`,
  `Move`, and `Copy` rows. This is the "is it safe to delete A?" mode, and therefore the
  mode where the data-loss bugs in [known-issues.md](known-issues.md) hurt most.
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
