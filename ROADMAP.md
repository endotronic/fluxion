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
- [ ] metadata-only scan mode (no hashing) so a 185T fleet can be triaged by size+name
  first and hashed only where trees actually overlap — blocked by the
  `CHECK (length(sha1) > 0 OR length(md5) > 0)` constraint on `files`
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
