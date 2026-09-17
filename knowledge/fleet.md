# The fleet — what Fluxion is actually for

Recorded 2026-08-23, from the author's live infrastructure and the sibling planning
project in `../stash`. [goals.md](goals.md) says *why* the tool exists in the abstract;
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

## The sibling project: `../stash`

`/home/kevin/projects/stash` is an ongoing, separate effort to reorganise ZFS
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
  snapshot per ZFS dataset — the granularity `../stash` reasons in — and, because each
  `.zfs/snapshot/<name>` automount has its own `st_dev`, it also makes the snapdir hazard
  below disappear for free. **`fluxion zfs-scan` (`zs`) does this whole pass for you**: it
  enumerates every dataset under a root with `zfs list`, then mounts *every* dataset it
  scans — even one already mounted at its usual location — at a fresh, temporary, isolated
  path (never touching the persistent `mountpoint` property; see [cli.md](cli.md#zfs-scan)),
  scans each with `--cross-mounts=false`, then unmounts and removes the temporary directory.
  Mounting a dedicated copy unconditionally, instead of reusing whatever's already mounted,
  buys two things worth knowing about for this fleet specifically: `--cross-mounts=false`
  no longer has to reason about this fleet's real mount layout at all (nothing can already
  be nested inside a fresh empty directory), and it surfaces files that a dataset's live
  mountpoint would otherwise hide underneath wherever a child dataset happens to be mounted
  on top of it — exactly the kind of leftover content the fleet's `deprecated`/`copy`/`backup`
  trees are suspected of containing. Run it with `--dry-run` first: mounting a
  previously-unmounted dataset makes it newly accessible (and writable) to every other
  process on the host for the scan's duration, and across dozens of datasets in one run
  that's worth reviewing before it happens for real.
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

## The artemis scan (2026-08-25 → 2026-09-16) and its 22-dataset gap

Fluxion has now been run against the fleet for real, twice. Recorded 2026-09-16.

| DB (on `/mnt/fleet-hdd`, a 503G spinning disk) | Size | Snapshots | Hashes |
|---|---|---|---|
| `artemis-fixed.db` | 47.2G | 154 | **SHA-1 only** |
| `artemis.db` | 44.8G | 154 | SHA-1 only — pre-`convert-db`, dead `/tmp/fluxion-zfsscan-*` roots |
| `luna-fixed.db` | 46.3G | 28 | SHA-1 + MD5, 34.5M rows |
| `luna-md5.db` | 24.1G | 27 + the imported 2025-12-24 legacy baseline | SHA-1 + MD5 |

Both `*-fixed.db` files came from `scripts/convert-db` and carry dataset-name
`root_path`s plus a `UNIQUE(snapshot_id, path)` index. **Queries over these on
that disk take many minutes to hours** — background them.

**Hash negotiation consequence.** `artemis-fixed.db` was scanned without `--md5`,
so artemis↔luna comparisons negotiate to **SHA-1** (fine, both sides have it), but
artemis can **never** be compared against the MD5-only 2025-12-24 legacy baseline.
Worth knowing before planning a comparison that needs it; a re-scan would be the
only fix, and at ~100T that is not a realistic one.

### The gap: 176 artemis datasets, 154 scanned

Own-bytes below are `USEDDS + USEDSNAP` (excludes children, so no double-count).

| Tree | Scanned | Unscanned filesystem | zvol |
|---|---|---|---|
| `artemis/deprecated` (32.50T) | 79 ds, 14.51T | 10 ds, **16.76T** | 7 ds, 1.28T |
| `artemis/zroot` (2.46T) | 51 ds, 1.84T | — | 3 ds, 641G |
| `artemis/temp` (1.52T) | 1 ds, 1.52T | — | — |

Two distinct causes, and only one is a real problem:

1. **The entire `artemis/deprecated/historian_newer` subtree (10 datasets,
   16.76T) was skipped** — including `content` (9.36T) and `untagged_content`
   (5.21T), which the table above in this file names as the single biggest prize
   on the board. Run #1 (2026-08-25) walked `artemis/deprecated` alphabetically,
   died on `historian_newer/comments`, and left it `in_progress`. Run #2
   (2026-08-28) resumed and jumped straight from `artemis/deprecated` to
   `artemis/deprecated/kevin`. An `--exclude-dataset` on run #2 is the likely
   cause but is **not recoverable from the DB** — nothing records which roots or
   excludes a run used, the same gap this file already flags below. Two of these
   have `mountpoint=none` and may also be `canmount=off`, which needs
   `--include-canmount-off`.
2. **10 zvols (1.91T) were skipped as `not a filesystem`**, which is correct and
   permanent. `zroot/vm/hyperion` (573G), `zroot_backup/var/lib/docker_ext4`
   (431G), `kevin/docker_ext4_backup` (334G), `zroot_backup/vm/hyperion` (266G),
   the three `DiskImages/win11*` (215G), two `vm/dev` (130G), `vm/testvm` (3.4G).
   Fluxion cannot and should not answer these — see "Not filesystem-aware" in
   [goals.md](goals.md). The only route to real evidence is mounting the
   filesystem *inside* a clone read-only and scanning that as an ordinary tree.

**`artemis/deprecated/historian_newer/comments` (785G, snapshot id 21) is still
`in_progress` from 2026-08-25.** An incomplete *candidate* under-reports what
would be lost — a false-safe answer, the exact failure [goals.md](goals.md)'s
severity rule forbids. `coverage` warns but still uses it. Rescan with
`zfs-scan --new` before any verdict involving it.

### The candidate-side variant of the unscanned-equals-deleted trap

The section below documents this trap with the gap on the **keeper** side. This
run has it on the **candidate** side, which inverts the symptom and makes it
worse, not better:

- An unscanned **keeper** makes real content look uncovered — noisy, but safe.
- An unscanned **candidate** subtree is simply *invisible*. A `coverage` run over
  `artemis/deprecated` as one unit would return a verdict that silently omits
  16.76T and **would look clean**.

So: run per-dataset against the explicit list of scanned datasets, never against
the tree root, while a gap exists — and report the gap's size alongside every
verdict.

### What this unblocks

The primary comparison needs **no cross-DB work**: both sides of "is
`artemis/deprecated` covered by `artemis/luna`?" are snapshots in
`artemis-fixed.db`. One DB, one host, one hash. 17.87T of candidates are scanned
and awaiting a `coverage` run today. `../stash`'s plan is stalled on 7.79T of
artemis space (the deferred `kevin/photos` + `kevin/images` catch-up) while
artemis has fallen to 3.89T AVAIL at 95% CAP — so roughly 45% of that 17.87T
proving redundant is enough to restart it.

## `zfs-scan` coverage is not self-verifying — cross-check before trusting a comparison

Found 2026-09-08, comparing a real `zfs-scan` DB (`luna-md5.db`, 27 snapshots, one per
dataset under whatever root(s) that run was pointed at) against the author's pre-Fluxion
full-tree `/luna` baseline from 2025-12-24 (see [goals.md](goals.md) "Lineage" for where
that baseline lives). **35% of the baseline's 49.7M files — `luna/historian/minio` (12.8M
files), `luna/kevin/amcrest` (3.5M), `luna/kevin/dropbox` (537K), `luna/kevin/media`
(607K) — had no corresponding snapshot in `luna-md5.db` at all.** These are real, live
datasets (confirmed via `mount`/NFS exports, `luna/historian/minio` alone is 16 TB), simply
never included in whatever root(s) that particular `zfs-scan` invocation was given.

Nothing in the tool flags this. `list` just shows however many snapshot rows exist, with no
way to tell "this is every dataset under the pool" from "this is every dataset someone
remembered to scan." Run `coverage` or `diff` against a baseline that covers more ground
than the current scan, and every file under the unscanned dataset looks identical to a
**deleted** file — there is no third state for "not compared." At fleet scale this is not
a cosmetic gap: pointing `coverage --by-dir` at a candidate with a 12.8M-file unscanned
subtree produced multiple GB of `--by-dir` output (one line per leaf directory under an
S3/MinIO-style hashed-bucket tree) before being killed, because *every single file* under
it registered as uncovered.

**Before trusting any `coverage`/`diff` result against a baseline that predates or spans
more than the current `zfs-scan` run: diff the two *sets of top-level directory names*
first**, not just the files. A cheap SQL check does it — group the baseline's paths by
first (or first two) path component(s) under the scan root and compare that list of names
against the current snapshot names. Any baseline directory with no matching current
snapshot is a scan-coverage gap, not evidence of deletion; `--exclude` it explicitly from
the comparison and report its size separately. This is the practical, non-code-level
consequence of the "no metadata-only scan mode" and "nothing records device/host
provenance" gaps already listed below — there is also currently nothing that records
*which roots a given zfs-scan run covered*, so that has to be reconstructed by hand from
whatever `--exclude-dataset` flags and root arguments were actually used.

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
