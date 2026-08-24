# Goals

## What Fluxion is

Fluxion records **metadata-only snapshots of a filesystem** (path, size, mtime, SHA-1,
optionally MD5) into a SQLite database, and then answers questions *about the snapshots*
rather than about the live filesystem.

It never reads or writes the user's data files except to hash them. It never deletes
anything. Every command is read-only with respect to the scanned filesystem.

## The question it exists to answer

From the README's motivation: the author has decades of personal files that have been
copied and moved between machines and disks, and wants to clean them up **without ever
losing something**. Every feature serves one of two questions:

1. **"Is it safe to delete this copy?"** — is everything in snapshot A also present
   somewhere in snapshot B? (`diff --update`, `dupes`)
2. **"What actually changed between these two points?"** — and crucially, distinguish a
   *real* change from a *relocation*, so a reorganised directory tree doesn't look like
   a catastrophic delete-plus-add. (`diff` with move/copy detection)

This framing drives the design more than anything else. In particular it explains why
the diff works on **content hashes**, not paths or mtimes: a file that moved is the same
file, and the tool must say so.

As of 2026-08-23 question 1 has a concrete, urgent target: see [fleet.md](fleet.md). The
author's four ZFS hosts hold ~185T at 94–96% capacity, with multi-terabyte datasets named
`deprecated`/`copy`/`backup` that are presumed deletable. Fluxion is the evidence that
turns that presumption into a defensible verdict — and the presumption runs in exactly the
direction the severity rule below says is dangerous, so the tool must be the thing that
says *no*.

## Design consequences of that framing

- **False "unchanged"/"present" answers are the worst possible failure.** If Fluxion says
  a file is safe to delete and it isn't, the user loses data permanently. Any bug that
  causes a file to silently vanish from diff output is a top-severity bug, not a cosmetic
  one. See [known-issues.md](known-issues.md) — several such bugs currently exist.
- **False "changed" answers are merely annoying.** Over-reporting costs the user reading
  time. This asymmetry should decide every trade-off.
- **Output must collapse.** A diff of two multi-terabyte trees produces millions of raw
  changes. The whole point of the tree/rollup machinery in
  [diff-algo.md](diff-algo.md) is to present "this directory moved" as one line instead
  of 40,000 lines. Aggressive collapsing is a *feature*, but only while it never hides a
  removal.
- **Snapshots are portable and comparable across roots.** The same tree scanned at
  `/mnt/backup1` and `/mnt/backup2` must diff clean. Paths are therefore made relative to
  each snapshot's `root_path` before comparison.
- **The DB is the durable artifact.** Snapshots outlive the disks they describe; you can
  keep a snapshot of a drive you have since wiped and still ask whether its contents are
  covered by a current backup.

## Lineage

Fluxion is a rewrite of the author's Python tool
[dupe-finder](https://github.com/endotronic/dupe-finder), which stores MD5 hashes in flat
text files and has been in real use for years verifying backups. That history is why:

- MD5 is still supported alongside SHA-1 (`--md5`), purely for interoperability.
- `import-legacy` / `export-legacy` read and write the flat-file format
  (see [cli.md](cli.md) for the exact format).
- Legacy-imported snapshots have **no SHA-1 and no mtime** — only MD5 and (optionally)
  sizes. Any code touching hashes must cope with a snapshot that has only one algorithm.

## Non-goals (as the code currently stands)

- Not a backup tool. It stores no file contents and cannot restore anything.
- Not a live/continuous watcher. Snapshots are explicit, point-in-time, and manual.
- Not a deduplicating filesystem. `dupes` reports; it never links or deletes.
- No handling of symlinks, hard links, devices, sockets, FIFOs, permissions, ownership,
  xattrs, or empty directories. Only **regular files** are recorded. See
  [scanner.md](scanner.md) for exactly what this means for diff results.
- **Not filesystem-aware.** No ZFS, btrfs, or snapshot-format knowledge; the scanner is a
  plain directory walk. A ZFS snapshot directory can be scanned like any other tree, and
  space accounting for shared blocks is ZFS's job, not Fluxion's. [fleet.md](fleet.md)
  records why the `ROADMAP.md` v0.9.0 "ZFS tools" item is deliberately not being built.
  `zfs-scan`'s dataset enumeration and mount/unmount driver doesn't reverse this: it drives
  ordinary present-moment `snapshot` calls against *live* datasets and never reads
  `.zfs/snapshot/*` or ZFS's own snapshot history.

## Where the project is

Version `0.8.13` (`internal/consts/consts.go`), pre-1.0, single author, no CI.
`ROADMAP.md` in the repo root is the authoritative feature backlog and is kept in
reverse-version order with `--- main branch ---` marking the boundary between shipped
and planned. The v1.0 list is dominated by usability work (TTY detection, a default
`~/.fluxion` DB, DB-upgrade prompting) plus one explicitly-called-out item:

> "Very thorough review of the code to make sure diff works as expected in edge cases"

That item is the reason [diff-algo.md](diff-algo.md) and
[known-issues.md](known-issues.md) exist.
