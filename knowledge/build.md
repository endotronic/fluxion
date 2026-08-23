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
  slow on a large tree.

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
