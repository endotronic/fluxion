# Fluxion Roadmap

## v1.0.0
- [ ] Very thorough review of the code to make sure diff works as expected in edge cases
- [ ] detect if attached to a TTY and handle accordingly
- [ ] default DB location at ~/.fluxion
- [ ] first run is detected as being attached to TTY, DB location is default, and no DB exists. Just gives an intro and explains where the DB will be.
- [ ] if attached to TTY, prompt for DB upgrade. if not, fail.
- [ ] DB and auto upgrade are top level CLI options

## v0.9.0
- [ ] command: show largest files, directories
- [ ] In snapshots table, track the device(s) that held the filesystems (need to think about multiple devices case)
- [ ] ZFS tools
    - [ ] Support ZFS snapshot as target (e.g. pool/filesystem@snapshot)
    - [ ] Snapshot all ZFS snapshots for a filesystem (recursive)

## v0.8.12
- [ ] dupes: if a directory is entirely contained in another, it counts as a folder dupe
- [ ] if copies are disabled, don't show the copies as additions since the hash is not new
- [ ] import-legacy gets line count while determining root, then uses that to show progress

## v0.8.11
- [x] memory use optimization for diff
- [x] sorted dupes by wasted desc, option for limit of results

## v0.8.10
- [x] In a diff, when collapsing to a common ancestor, when there is a mix of copies and additions, it should just compress down to additions. Right now the copies make it many lines which is hard to parse. Maybe moves should be included in that - not sure.