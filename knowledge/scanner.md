# The scanner: what gets recorded, and what does not

`internal/scanner/scanner.go` turns a directory tree into a stream of
`models.FileRecord`. It is deliberately dumb — no database, no snapshot lifecycle, no
printing beyond a few warnings. `internal/app/snapshot.go` owns everything around it.

## Shape

```
filepath.WalkDir (1 goroutine)  ──paths──▶  N workers (hash)  ──ScanResult──▶  caller
```

- `paths` is buffered at `NumWorkers * ScannerChannelBufferMultiplier` (1000), so up to
  `threads*1000` paths queue ahead of the hashers. On a fast metadata walk over a slow
  disk this fills immediately and the walk then blocks on the hashers, which is the
  intended back-pressure.
- `results` is created and sized by the caller; `RunScan` closes it via `defer`.
- Workers exit when `paths` closes; `RunScan` then `wg.Wait()`s and finally calls
  `OnWalkComplete`. Note the name is misleading: `OnWalkComplete` fires **after all
  hashing finishes**, not after the walk.
- `NumWorkers` defaults to `runtime.NumCPU()` (`--threads`).

## What is recorded

`models.FileRecord`: `Path` (absolute, as walked), `Filename` (basename), `SizeBytes`,
`ModTime`, `SHA1`, and `MD5` when `--md5` is given. SHA-1 and MD5 are computed in a single
read via `io.MultiWriter`, so `--md5` costs almost nothing extra in I/O.

## What is NOT recorded — this matters for reading diffs

`WalkDir` yields the entry, and anything failing `d.Type().IsRegular()` is skipped
outright:

- **symlinks** (never followed, never recorded — a symlink and its target are not the same
  file to Fluxion, and a tree of symlinks scans as empty)
- hard links: recorded as **independent files**, once per path. Two names for one inode
  are two records with the same hash, so `dupes` will report them as duplicates even
  though deleting one frees nothing.
- devices, FIFOs, sockets
- **empty directories** — nothing is recorded for a directory as such, so a directory that
  exists in A and not in B is invisible unless it contained files
- permissions, ownership, xattrs, ACLs, timestamps other than mtime

Consequences worth internalising: a diff that says "no differences" means *the regular
files match*. It says nothing about permissions, symlinks, or empty directories. And
because `internal/diff` ignores `ModTime` entirely (it compares hashes only), a file
touched but not changed diffs clean — which is the intended behaviour, but means mtime is
stored and never used by diff.

## Mount boundaries

Every directory is `os.Stat`ed and its `syscall.Stat_t.Dev` compared with the root's:

- `--cross-mounts` (default **true**) traverses into other filesystems, printing a warning
  the first time it crosses. `deviceMap` caches only *foreign* device IDs by path, so the
  warning fires on the transition rather than on every directory below it.
- `--cross-mounts=false` prints "Skipping filesystem boundary" and `SkipDir`s.
- `--fail-on-mount` aborts the walk with an error at the first boundary.

The default being `true` means a scan of `/` will happily walk into `/proc`, `/sys`,
network mounts, and external drives. That is a real footgun for the tool's own use case.

This code is Unix-only (`syscall.Stat_t`); there is no Windows build path.

## Resume

`ScannerConfig.ResumeMap` is a `map[path]FileRecord` of already-hashed files, loaded by
`app/snapshot.go` from the previous `in_progress` snapshot. A path is taken from the map
(and reported with `FromResume: true`) only if:

```go
rec.SHA1 != "" && (rec.MD5 != "" || !cfg.ComputeMD5)
```

so turning `--md5` on mid-resume correctly forces a re-hash of records that lack MD5.
`app/snapshot.go` then re-writes the resulting record, which is now an **upsert** against
a unique `(snapshot_id, path)` — it replaces the MD5-less original. Before schema v4 it
appended a second row instead, inflating `size` and making `dupes` report the file as its
own duplicate. See [data-model.md](data-model.md).

The resume map is fully materialised, so resuming a 10M-file scan loads 10M records into
memory before the walk starts.

## Error handling

`ScanResult.Error` is set for walk errors and per-file stat/read errors, and the walk
continues (`return nil`) so one unreadable file does not abort a multi-hour scan. That is
the right policy, and the caller now honours it: `app/snapshot.go` counts every such
error, prints up to 10 samples, and ends the scan with `FailSnapshot` and a non-zero exit
instead of `CompleteSnapshot`. Files that *were* read are still recorded — a partial
snapshot is worth looking at — but it is marked so a later diff cannot mistake its gaps
for deletions.

`getDevice(cfg.RootPath)` is resolved **before** the workers start, so its failure path
cannot strand goroutines blocked on `paths`. Keep it that way if you reorder `RunScan`.

## Testing

`internal/scanner` tests use real temp directories and no database.
`scanner_test.go` covers the basic walk, symlink exclusion, MD5, and resume.
Not covered: mount-boundary logic (needs a real mount), permission errors, and files
disappearing mid-scan.
