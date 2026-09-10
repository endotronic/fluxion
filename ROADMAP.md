# Fluxion Roadmap

## v1.0.0
- [ ] Very thorough review of the code to make sure diff works as expected in edge cases
- [ ] detect if attached to a TTY and handle accordingly
- [ ] default DB location at ~/.fluxion
- [ ] first run is detected as being attached to TTY, DB location is default, and no DB exists. Just gives an intro and explains where the DB will be.
- [ ] if attached to TTY, prompt for DB upgrade. if not, fail.
- [ ] DB and auto upgrade are top level CLI options
- [ ] automatic snapshot name should not have the part after the date; it is too long
- [ ] dupes: if a directory is entirely contained in another, it counts as a folder dupe

## v0.9.0
- [ ] command: show largest files, directories
- [ ] In snapshots table, track the device(s) that held the filesystems (need to think about multiple devices case)
- [ ] ~~ZFS tools~~ — **dropped 2026-08-23, see `knowledge/fleet.md`.** Measured against
  the author's own fleet, all 2,916 snapshots on terra+saturn hold 5.65 TiB combined
  (one of them is 2.40T), while scanning a snapshot costs hashing everything it
  *references*. ZFS already answers "how many bytes" exactly and for free; Fluxion would
  answer it worse, because it has no concept of shared blocks. A ZFS snapshot directory
  can already be scanned today as an ordinary tree.
    - [ ] ~~Support ZFS snapshot as target (e.g. pool/filesystem@snapshot)~~
    - [ ] ~~Snapshot all ZFS snapshots for a filesystem (recursive)~~
- [x] `coverage` command: "is every file in A present by content anywhere in B", answered
  without a diff tree by a SQL semi-join over partial indexes on `(sha1, snapshot_id)` and
  `(md5, snapshot_id)` (schema v5). Union of multiple keepers, exit 2 when something would
  be lost. Measured at 1.3 MiB of heap over 4M file rows. See `knowledge/cli.md`
- [x] `zfs-scan` command: enumerate every dataset under a pool with `zfs list`, mount
  *every* dataset it scans (even one already mounted at its usual location) at a fresh,
  temporary, isolated path via `mount -t zfs -o zfsutil` — never touching the persistent
  `mountpoint` property — scan each into its own snapshot with `--cross-mounts=false`, then
  unmount by path and remove the temporary directory. One invocation instead of driving
  `snapshot` dataset-by-dataset by hand. `--dry-run` prints the full plan with no mounting
  or DB writes. Does not read ZFS's own snapshot history — orthogonal to the dropped "ZFS
  tools" item above. See `knowledge/cli.md`
- [x] external/streaming diff engine so `diff` fits in 1 GB regardless of tree size —
  **done 2026-09-10**, all six phases of `knowledge/diff-memory.md`. Peak RSS measured
  flat at ~210 MiB from 1.6M to 6.4M files a side, where the tree engine extrapolates to
  ~1.5 GiB and ~6 GiB; move/copy detection runs as an external sort rather than a
  whole-tree hash index. `--engine auto|tree|streaming` (not the `memory|external` names
  planned) plus `--temp-dir`. Byte-identical to the tree engine over 200,000 property-test
  seeds, budgeted and not, with `--show-unchanged` on and off. Temp space is guarded two
  ways: an advisory estimate against free space up front (plus a warning when the temp
  directory is a `tmpfs`, which the default usually is), and a hard abort in the engine
  once writing would leave less than 512 MiB free. Still open: nothing exposes the sort
  buffer size from the command line
- [x] metadata-only scan mode (no hashing) so a 185T fleet can be triaged by size+name
  first and hashed only where trees actually overlap — **done 2026-09-10** as
  `snapshot --no-hash` / `zfs-scan --no-hash`. The `CHECK (length(sha1) > 0 OR length(md5)
  > 0)` constraint is gone from fresh databases; existing ones keep it and are not
  migrated, because dropping a CHECK needs either a full table rebuild (impossible on a
  49 GB database with 14 GB free) or a `PRAGMA writable_schema` edit, which is not
  something to run unbidden on an irreplaceable scan. `scripts/convert-db` moves an old
  database across instead
- [ ] import-legacy gets line count while determining root, then uses that to show progress

## v0.8.14
- [ ] if copies are disabled, don't show the copies as additions since the hash is not new

--- main branch ---

## v0.8.13
- [ ] Add find subcommand

## v0.8.12
- [x] Add export-legacy subcommand

## v0.8.11
- [x] memory use optimization for diff
- [x] sorted dupes by wasted desc, option for limit of results

## v0.8.10
- [x] In a diff, when collapsing to a common ancestor, when there is a mix of copies and additions, it should just compress down to additions. Right now the copies make it many lines which is hard to parse. Maybe moves should be included in that - not sure.
