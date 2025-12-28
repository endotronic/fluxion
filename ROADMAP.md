# Fluxion Roadmap

## v1.0.0
- [ ] Very thorough review of the code to make sure diff works as expected in edge cases

## v0.9.0
- [ ] In snapshots table, track the device(s) that held the filesystems (need to think about multiple devices case)
- [ ] ZFS tools
    - [ ] Support ZFS snapshot as target (e.g. pool/filesystem@snapshot)
    - [ ] Snapshot all ZFS snapshots for a filesystem (recursive)

## v0.8.10
- [ ] In a diff, when collapsing to a common ancestor, when there is a mix of copies and additions, it should just compress down to additions. Right now the copies make it many lines which is hard to parse. Maybe moves should be included in that - not sure.