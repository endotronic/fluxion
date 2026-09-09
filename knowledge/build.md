# Build, test, and toolchain

## Commands

```bash
make build     # go build -o fluxion ./cmd/fluxion
make test      # go test ./...
make verify    # build, then ./scripts/verify.sh  (end-to-end CLI smoke test)
make clean     # rm -f fluxion
```

Module name is `fluxion` (not a domain path), so imports are `fluxion/internal/...`.
`go.mod` pins `go 1.25.5`.

## The build is pure Go — no C compiler needed

The SQLite driver is `modernc.org/sqlite`, a pure-Go implementation. `go build ./...`,
`go test ./...`, and `scripts/verify.sh` all run on a machine with no C toolchain at all.

This was not always true. Until 2026-08-23 the driver was `github.com/mattn/go-sqlite3`, a
**cgo** binding, and a `CGO_ENABLED=0` environment produced a binary that *compiled fine*
and then failed at `sql.Open`/`Ping` time with `Binary was compiled with 'CGO_ENABLED=0',
go-sqlite3 requires cgo to work. This is a stub`. If you ever see that message, you are on
an old checkout or an old binary.

### What the swap changed, and what to watch for

- **Driver name.** `sql.Open("sqlite", ...)`, not `sql.Open("sqlite3", ...)`.
- **Connection settings are in the DSN**, not in `PRAGMA` statements — see `buildDSN` in
  `internal/store/sqlite/sqlite.go` and [data-model.md](data-model.md).
- **Timestamps.** mattn wrote `time.Time` values in its own layout
  (`2006-01-02 15:04:05.999999999-07:00`); modernc passes Go's default `String()` form
  through, which includes a monotonic-clock suffix (`m=+0.002088029`) that SQLite's own
  date functions cannot parse. Everything now goes through `dbTime()`, which stores UTC
  RFC3339Nano. **Databases written before the swap remain readable** — the scan path
  accepts all four historical layouts, and `internal/store/sqlite/timeformat_test.go`
  pins that. Do not "simplify" that parser.
- Bulk-insert throughput is lower than cgo's. It has not been a problem in practice
  (hashing dominates a scan), but it is the one thing to measure if `snapshot` ever feels
  slow on a large tree. **It is a real problem for `import-legacy` at fleet scale** — see
  below.

## `import-legacy` does not scale to a fleet-size flat file as shipped

Found 2026-09-08 importing a real 49.7M-line `dupe-finder` hashes file (13 GB) on a
memory-constrained box (3.8 GB RAM, mostly already in use). Two separate problems, both
worth knowing before pointing `import-legacy` at anything bigger than a test fixture:

1. **The sizes-file side table is an unbounded `map[string]base64path]int64` held for the
   whole run** (`RunImportLegacy`, `internal/app/import.go`). For a sizes file with tens of
   millions of lines this is multiple GB of Go map overhead on top of the raw data — on a
   box with only ~1.3 GB actually free, this is not a slowdown, it is a swap-storm risk of
   exactly the kind [diff-memory.md](diff-memory.md) documents for `diff`. There is no flag
   to disable size loading; the workaround is to point `--sizes` at an existing **empty**
   file (this bypasses the "infer from the hashes filename" branch, which only fires when
   `--sizes` is unset) and accept `size_bytes = 0` for the whole snapshot. Sizes can be
   backfilled afterward with a separate streaming pass keyed on `(snapshot_id, path)` — the
   unique index makes that an indexed `UPDATE` per row, no side table required — but this
   run didn't need to bother; the coverage question doesn't consume `size_bytes` on the
   candidate's own path field, only its content hash.
2. **The per-row `INSERT ... ON CONFLICT DO UPDATE` path (`BatchAddFiles`) sustains only
   ~3,000-7,000 rows/sec against a fresh, empty-of-this-snapshot target** on this hardware —
   at that rate 49.7M rows is 2-5 hours. The bulk of that cost turned out to be **maintaining
   the two partial hash indexes** (`idx_files_sha1`, `idx_files_md5` — see
   [data-model.md](data-model.md) migration 5) on every single insert, not the upsert logic
   itself or modernc's driver overhead in general: dropping both indexes before the load and
   recreating them once afterward took throughput from ~5-7K rows/sec to **~68,000 rows/sec**
   — a ~10-15x speedup, turning a multi-hour import into a ~17.5-minute bulk load plus a few
   minutes of `CREATE INDEX`. Recreating those two indexes over the resulting ~85M-row table
   needs real temp disk headroom (this run hit "database or disk is full" on the first
   attempt with only ~4 GB free; it wants closer to the final index size again in temp space,
   here several GB) — check `df` before doing this on a nearly-full disk, not after.

Neither of these is specific to the legacy path — `merge`'s and `import`'s materialising
reads have the same "no problem until it's fleet-scale" character (see
[architecture.md](architecture.md)) — but `import-legacy` is the one most likely to be
pointed at a decades-old flat file with no idea how many lines it holds. `wc -l` the file
first if it's coming from real fleet history; see [fleet.md](fleet.md) for where that
history actually lives.

### Test coverage without a database

| Package | Needs a DB? | Why |
|---|---|---|
| `internal/diff` | no | pure tree/hash logic over in-memory iterators |
| `internal/dupes` | no | pure tree logic over an in-memory map |
| `internal/scanner` | no | real filesystem + temp dirs, no DB |
| `internal/util` | no | statfs / `/proc/mounts` parsing |
| `internal/table` | no | (no tests) |
| `internal/app` | **partly** | `merge_test.go` and `size_test.go` open a real DB in a temp dir; `diff_test.go` (`isExcluded`) does not |
| `internal/store/sqlite` | **yes** | every test opens a DB |

Practically: **the entire diff algorithm — the most complex and highest-risk part of the
project — is testable with no C compiler and no database.** `internal/diff` takes
`FileIterator` callbacks, and `internal/diff/test_helper.go` has `mapToIter` to drive it
from a plain `map[string]models.FileRecord`. Use that for any diff investigation.

```bash
go test ./internal/diff/ ./internal/dupes/ ./internal/scanner/ ./internal/util/ -v
```

## scripts/verify.sh

An end-to-end shell test against a real built binary and a real temp tree. It is not
wired into `make test`; run it via `make verify`. It covers, in order:

1. snapshot → list → modify files → second snapshot
2. `diff` detects `[M]` modification and `[+]` addition
3. `merge` of two snapshots
4. `dupes` finds a copied file
5. **root relocation**: renames the scanned directory, re-snapshots, and asserts the diff
   against the previous snapshot reports `No differences found` — this is the regression
   test for relative-path comparison
6. `--exclude` both excludes the named directory and does *not* over-exclude a sibling

It `rm -rf`s `tmp_verify_data*` and `verify.db` in the **current working directory** at
start and end. Those names are in `.gitignore`.

## No CI, no linter config

There is no `.github/`, no golangci-lint config, no LICENSE file. `go vet ./...` is clean
as of this writing. Nothing enforces formatting; a few files have inconsistent
indentation (`internal/app/snapshot.go`, `internal/dupes/dupes.go`) and would move under
`gofmt`.
