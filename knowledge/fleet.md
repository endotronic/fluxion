# The fleet — what Fluxion is actually for

Recorded 2026-08-23, from the author's live infrastructure and the sibling planning
project in `../scratch`. [goals.md](goals.md) says *why* the tool exists in the abstract;
this file says what it is being pointed at, and it is the context that should settle most
judgement calls about priorities.

## The goal, in the author's words

> "This is precisely why I started building fluxion, but I never used it to achieve the
> ultimate goal, which is to free space by removing duplicates across datasets. There are
> many datasets named 'deprecated', 'copy', and 'backup'. They should all be deleteable,
> and fluxion will confirm it."

So the deliverable is not a diff, or a dupes report. It is **a defensible verdict that a
multi-terabyte tree can be destroyed**, on infrastructure that is 94–96% full. Everything
in [goals.md](goals.md)'s severity rule — a false "present/unchanged" loses data, a false
"changed" only wastes reading time — is about to be exercised for real, on data with no
third copy.

## The fleet

Four Proxmox hosts. Fluxion has never been run against any of them.

| Host | Pool | Alloc | Usable AVAIL | Role |
|---|---|---|---|---|
| terra | `luna` | 82.8T | **293G** (95.8% full) | Primary / hot. Source of truth for real data. |
| mars | `phobos` | — | 2.62T (84.9%) | Hot. Local-primary for rebuildable/redownloadable data. |
| saturn | `artemis` | 102T | **5.20T** (93.6% full) | Cold storage, replica-only. Often powered off for months. |
| saturn | `dione` | 1.24T | — | **DEGRADED**, one disk FAULTED. Urgent for recovery. |
| saturn | `iapetus` | 4.79T | — | Clean, but hardware considered unfit for long-term reliance. |
| saturn | `rhea` | 5.41T | — | Checksum errors on one member, auto-corrected so far. |
| venus | `rpool` only | — | — | Out of scope except for Proxmox VM images. |

Fleet-wide usable free: **≈8.11T**. Note that `zpool list` FREE is *raw* (pre-parity) and
overstates writable space badly on raidz — always use `zfs list` AVAIL.

Scale, from `terra.txt` / `saturn.txt`: **339 datasets** (82 on terra, 257 on saturn) and
**2,916 snapshots**. A one-time full hash pass over luna at a disk-bound ~800 MB/s is
roughly 30 hours; artemis is another ~35. That is the budget any plan has to fit inside.

## The sibling project: `../scratch`

`/home/kevin/projects/scratch` is an ongoing, separate effort to reorganise ZFS
replication across these hosts. **Read its `CLAUDE.md`, `REPLICATION_REPORT.md`, and
`RECONCILIATION_PLAN.md` before proposing anything fleet-related** — they hold the
current inventory, the replica analysis, and the phased execution plan.

- Raw inventory: `terra.txt`, `mars.txt` (currently empty), `saturn.txt`, `venus.txt`,
  produced by `collect-zfs-info.sh` via `collect-all.sh`.
- `analyze.py` cross-references **snapshot GUIDs** (preserved across `send`/`recv`) with
  union-find to prove which datasets are genuinely replicas of one another.
- That project has no SSH access from this environment either; the user runs the scripts
  and pastes results back.

Its stated policy: every dataset should exist on exactly 2 of {luna, phobos, artemis};
artemis holds replicas only; dione/rhea/iapetus never count toward the target and must
never hold a sole copy.

## The gap only Fluxion can close

The replica analysis is **GUID-based**, which proves *"this dataset is not a `send`/`recv`
descendant of that one."* It cannot prove *"the files in it don't exist somewhere else
under a different path."* Every "verified only copy" verdict in that plan rests on that
inference, and it is the thing standing between the author and the fleet's biggest
reclaimable numbers:

| Size | Dataset | Plan's verdict | The Fluxion question |
|---|---|---|---|
| 9.36T | `artemis/deprecated/historian_newer/content` | "Not deletable — verified only copy" | Is every file of it present on luna under *any* path? |
| 5.77T | `artemis/deprecated/zalt` + `historian_new` | same | same |
| 3.79T | `luna/historian/arctic_shift` | 0 copies; name collision with an artemis dataset that is *not* the same data | Is the collision partial? Which files overlap? |
| 1.52T | `artemis/temp` | "investigate before trusting as just temp" | Content-covered elsewhere or not? |
| 256G | `artemis/deprecated/kevin/these_should_all_be_copied_out_now@2024-02-21` | — | The dataset name is literally the question |

`artemis/deprecated` alone is **16.65T** currently written off as a permanent 1-copy
exception. Confirming even part of it is redundant is worth more than every other lead on
the board combined.

Same shape, lower stakes: the stale replicas. `kevin/photos` (+3.79T behind) and
`kevin/images` (+4.00T) already have a 2nd copy on artemis that is ~10.8T out of date.
Two live scans and one `diff --update` say exactly which files are missing — no snapshot
history required.

**The naming heuristic the author trusts, which Fluxion exists to verify:** any dataset
named `deprecated`, `copy`, `backup`, `old`, `_new`, or similar is *presumed* deletable.
Fluxion's job is to turn that presumption into evidence, one tree at a time. Note that the
presumption is exactly the direction the severity rule says is dangerous — the tool must
be the thing that says no.

## Verdict: do NOT build per-snapshot scanning

`ROADMAP.md` v0.9.0 lists "Support ZFS snapshot as target" and "Snapshot all ZFS
snapshots for a filesystem (recursive)". The idea was to scan every ZFS snapshot so the
author could decide which to prune. **The fleet's own numbers kill it.** Across the 2,916
snapshots on terra + saturn:

| Measure | Value |
|---|---|
| Total snapshot `USED` (everything, if you deleted them all) | **5.65 TiB** |
| Snapshots ≥ 1T | **1** — `luna/kevin/photos/immich@2026-02-16-before-redo`, 2.40T, already flagged in the plan |
| Snapshots sized in GB | 283 |
| Snapshots sized in MB | 638 |
| Snapshots sized in KB | 510 |
| Snapshots sized in **bytes** | 1,484 |

Three reasons this stays unbuilt:

1. **ZFS already answers it, exactly.** `zfs list -t snapshot -o name,used,written,refer`
   and `zfs destroy -nv pool/ds@a%b` for ranges. Fluxion would answer it *worse*: blocks
   are shared between snapshots, so summing recorded file sizes over-counts reclaimable
   space wildly. Fluxion has no concept of block sharing and should not acquire one.
2. **The prize is the smallest on the board.** 5.65T maximum, ~3.2T realistically, against
   16.65T in `artemis/deprecated` and a fleet that needs space *now*.
3. **The cost is the largest.** Scanning a snapshot means hashing everything it
   *references*, not its delta — a 24-snapshot dataset costs 24× its size in reads. `zfs
   diff` would cut that by orders of magnitude (it costs bytes-changed, not bytes-total),
   and it still would not be worth doing.

If it is ever revisited, the caveats found while evaluating it: `zfs diff` needs root or
`zfs allow -d <dataset> diff <user>`, octal-escapes special characters in paths, emits
two paths on `R` (rename) lines, and reports metadata-only changes as `M`. More
importantly, a snapshot record *derived* from a delta is not an *observed* one — under
the severity rule that is a false-unchanged risk, so provenance would have to be stored
per snapshot and a full-verify escape hatch offered.

**The division of labour to keep: ZFS says how many bytes. Fluxion says what you'd lose.**

This is orthogonal to `fluxion zfs-scan` ([cli.md](cli.md#zfs-scan)): that command mounts
and walks *live* datasets for a fresh, present-moment scan — it never reads
`.zfs/snapshot/*` or `zfs diff`, so it does not reopen this question.

## Running Fluxion against this fleet

Fluxion is not ZFS-aware in any way — `internal/scanner` is a plain `filepath.WalkDir`
with an `st_dev` boundary check, and nothing knows what a dataset is. It does not need to
be, because of the following.

- **Scan per dataset with `--cross-mounts=false`.** The flag defaults to **true**
  (`cmd/fluxion/main.go`), so pointing a scan at `/luna` walks into all 82 child dataset
  mountpoints and produces one 82.8T snapshot record. Setting it false yields one Fluxion
  snapshot per ZFS dataset — the granularity `../scratch` reasons in — and, because each
  `.zfs/snapshot/<name>` automount has its own `st_dev`, it also makes the snapdir hazard
  below disappear for free. **`fluxion zfs-scan` (`zs`) does this whole pass for you**: it
  enumerates every dataset under a root with `zfs list`, mounts whatever isn't already
  mounted, scans each with `--cross-mounts=false`, then unmounts what it mounted — one
  invocation instead of walking the dataset list by hand. See [cli.md](cli.md#zfs-scan).
  Run it with `--dry-run` first: mounting a previously-unmounted dataset makes it newly
  accessible (and writable) to every other process on the host for the scan's duration, and
  across dozens of datasets in one run that's worth reviewing before it happens for real.
- **Check `zfs get -r snapdir luna artemis` before the first scan.** ZFS defaults to
  `snapdir=hidden`, in which case `.zfs` never appears in `readdir` and a walk cannot
  descend into it. Any dataset set to `visible` would, with default flags, have every one
  of its snapshots scanned — N× the work and a garbage record.
- **A ZFS snapshot can still be scanned directly today, with zero code changes.**
  `/luna/kevin/photos/.zfs/snapshot/<name>` is an ordinary directory tree. Two such scans
  diff correctly against each other despite different root paths, because `app/diff.go`
  strips each snapshot's `root_path` first.
- **Collecting across hosts.** Scan into a local DB on each host, copy it back, then
  `fluxion import --source terra.db --all` into one fleet DB. Since the driver swap to
  `modernc.org/sqlite`, `GOOS=linux GOARCH=amd64 go build ./cmd/fluxion` produces a static
  binary that needs no toolchain on terra or saturn (see [build.md](build.md)).
- **The verdict command is `diff --update <candidate-scan> <keeper-scan>`.** It drops
  `Added`/`Move`/`Copy` and keeps `Removed`/`Modified`, so empty output means every file
  in the candidate is present in the keeper with identical content. Two caveats worth
  repeating to the user each time: it is directional, and it never compares mtimes, so it
  cannot tell you which side is *newer* — only what content would be lost.

### Suggested order of work

0. For any dataset whose only question is *"can I delete this?"* — every `deprecated`,
   `copy`, `backup`, and `old` tree — run `coverage` and stop there. It needs no diff, no
   tree, and no judgement about rollups; it prints exactly what would be lost and exits 2
   if anything would. Use `diff` only when the question is genuinely *"what changed and
   where did it go?"*.
1. A ~100G replica pair first — `luna/kevin/archives/file_records` vs
   `artemis/luna/kevin/archives/file_records` (80.2G used / 572G logical, many small
   files, scans in minutes). Cheap, and it produces real diff output to calibrate the
   rollup against.
2. `artemis/deprecated` vs luna. This is where the terabytes are.
3. The stale replicas, `kevin/photos` and `kevin/images`.

## Gaps this fleet exposes in the tool

Recorded as design pressure, not yet as issues in [known-issues.md](known-issues.md):

- **No metadata-only scan mode.** At 185T, a cheap size+name pass to find *candidate*
  overlaps, followed by hashing only those, would beat hashing everything by a wide
  margin. The schema currently forbids it: `files` carries
  `CHECK (length(sha1) > 0 OR length(md5) > 0)`. This is a far better use of build time
  than any ZFS integration.
- ~~**Cross-tree coverage is not a first-class question.**~~ **Closed 2026-08-23** by the
  `coverage` command ([cli.md](cli.md)). "Is every file under X present *somewhere* in Y,
  at any path" is now a SQL semi-join over content hashes with no diff tree: measured at
  1.3 MiB of heap over 4M file rows, and it exits 2 when something would be lost so it can
  gate a real `zfs destroy`. This is the command to reach for on this fleet — **not**
  `diff --update` — because the trees here were reorganised as well as copied, which is
  exactly the case the path-based diff reports as thousands of moves and the memory
  ceiling it cannot survive.
- **Nothing records which device or host a snapshot came from.** `ROADMAP.md` v0.9.0
  already wants this ("track the device(s) that held the filesystems"); with scans
  arriving from four hosts via `import`, `computer_name` alone is thin provenance.
