# Data model: schema, migrations, and snapshot lifecycle

## Tables

```sql
snapshots(
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  name          TEXT NOT NULL,            -- UNIQUE via idx_snapshots_name
  root_path     TEXT NOT NULL,            -- absolute; diff strips it to compare
  started_at    DATETIME NOT NULL,
  finished_at   DATETIME,                 -- NULL while in_progress
  status        TEXT NOT NULL,            -- in_progress | completed | failed | deleted
  hashes        TEXT DEFAULT '',          -- comma-joined, e.g. "sha1" or "sha1,md5"
  computer_name TEXT DEFAULT '',          -- added in schema v2
  error_count   INTEGER DEFAULT 0         -- added in schema v3; see below
)

files(
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
  path        TEXT NOT NULL,              -- absolute, as walked
  filename    TEXT NOT NULL,              -- basename, denormalised for `find`
  size_bytes  INTEGER NOT NULL,
  mod_time    DATETIME NOT NULL,
  sha1        TEXT NOT NULL,              -- may be '' for legacy imports
  md5         TEXT NOT NULL,              -- may be ''
  CHECK (length(sha1) > 0 OR length(md5) > 0)
)

db_metadata(key TEXT PRIMARY KEY, value TEXT)   -- holds 'schema_version'
```

Indexes: `idx_files_snapshot_path ON files(snapshot_id, path)` (unique as of schema v4)
and `idx_snapshots_name ON snapshots(name)` (unique).

### Things to know about this schema

- **`(snapshot_id, path)` is UNIQUE** as of schema v4, and `AddFile`/`BatchAddFiles` are
  upserts (`ON CONFLICT ... DO UPDATE`). One path holds one thing at one instant, so a
  second write to the same path in the same snapshot *replaces* rather than appends.
  Before v4 it appended, and resume with `--md5` newly enabled did exactly that; the
  duplicates then corrupted `size` (double-counted) and `dupes` (a file appeared to be its
  own duplicate). Migration 4 de-duplicates existing databases, keeping the highest `id`
  of each group. Note the consequence for `merge`: inputs that share a path now collapse
  to one record and the **last input listed wins** — `app/merge.go` reports how many
  collapsed and warns when the collapsed records disagreed about content.
- **`error_count` is the completeness signal.** A snapshot with a non-zero count did not
  read everything it walked, and anything missing from it will look *deleted* in a later
  diff. It is 0 for every snapshot taken before schema v3 — including ones that were
  silently incomplete, because nothing recorded the failures at the time. Old snapshots
  cannot be retroactively verified; re-scan if it matters.
- The `CHECK` constraint is the only thing guaranteeing a record is usable: at least one
  hash must be present. Legacy-imported snapshots satisfy it with MD5 only.
- **There is no index on `sha1` or `md5`.** All hash-based analysis is done in Go after
  streaming rows out, never in SQL. That is a deliberate consequence of the tree-based
  algorithms, but it means `dupes` and `diff` cannot be made incremental without adding
  one.
- `hashes` is a comma-joined string, not a relation — parsed with `strings.Split` in the
  sqlite store and used by `app/diff.go` to negotiate a common algorithm.
- `path` is stored **absolute**. Relativisation happens at read time in `app/diff.go`.
  Storing absolute paths is what makes `root_path` load-bearing: change it and comparisons
  break.

## Migrations

`internal/store/sqlite/schema.go`. `migrations` is an ordered `[]func(*sql.DB) error`;
`CurrentSchemaVersion = 5`. `migrations[i]` upgrades version `i` → `i+1`, and the version
is written to `db_metadata` after each step.

Version 5 (2026-08-23) added two **partial** indexes on the content hashes:

```sql
CREATE INDEX idx_files_sha1 ON files(sha1, snapshot_id) WHERE sha1 != '';
CREATE INDEX idx_files_md5  ON files(md5,  snapshot_id) WHERE md5  != '';
```

They exist for `coverage` (`sqlite/coverage.go`), which asks "does this content appear in
any of these other snapshots?" once per candidate file. With the index that is a covering
lookup; without it, a table scan per file. The `WHERE ... != ''` clause both keeps the
index off the many empty-hash rows and is what the query must repeat (`g.sha1 != ''`) for
SQLite to prove the partial index applies — see the comment in `coverage.go`. The measured
plan on 4M rows is `SEARCH f USING INDEX idx_files_snapshot_path` + `SEARCH g USING
COVERING INDEX idx_files_sha1`, with no temp b-tree.

Special case on open: if `schema_version` is absent but a `snapshots` table exists, the DB
is assumed to be a pre-metadata legacy database and is stamped as **version 1** without
running `migrations[0]`. Fresh databases start at 0 and run everything.

**To add a migration:** append a function to the slice and bump `CurrentSchemaVersion`.
Never reorder or edit an existing entry — the index *is* the version number.

Migrations run automatically and silently on every open, with no prompt and no backup.
"Prompt to upgrade DB" is an open v1.0 roadmap item precisely because of this. There is
also no down-migration and no version-too-new check: an older binary opening a newer DB
sees `version >= CurrentSchemaVersion`, returns "up to date", and then operates against a
schema it does not understand.

## Snapshot lifecycle

```
CreateSnapshot()  ──▶  in_progress  ──▶  CompleteSnapshot()  ──▶  completed
                            │
                            └──▶  (interrupted; row stays in_progress, and is what
                                   `snapshot` finds and offers to resume)
                       DeleteSnapshot()  ──▶  deleted   (tombstone, rows removed)
```

- `in_progress` is both "currently running" and "crashed halfway". They are
  indistinguishable, which is why resume works but also why a stale row can be picked up.
- `failed` is set by `FailSnapshot` when a scan could not read or record everything it
  walked, and `RunSnapshot` then returns a non-zero exit. The snapshot rows it *did*
  gather are kept — a partial snapshot is still useful to look at — but the status and
  `error_count` mark it as not trustworthy for "is it safe to delete this?".
- `deleted` is a tombstone: `DeleteSnapshot` removes the file rows and marks the snapshot,
  keeping the name reserved. Beware: `FindSnapshot` and `GetLastSnapshot` do **not**
  filter tombstones, so a deleted snapshot can be resolved by name/ID and then presents as
  an empty snapshot — a diff against it reports everything as removed.
- Names are unique. `getUniqueSnapshotName` (`internal/app/helpers.go`) resolves
  collisions by appending an incrementing suffix. `Store.RenameSnapshot(id, newName)` is the
  other way a name changes: it's how `RunSnapshot`'s `AllowDuplicateName` option (only set by
  zfs-scan's `--new`) reuses a name that's already taken — the existing row, whatever its
  status, is renamed to `<name>_superseded_<old id>` to free the name up, rather than being
  overwritten or deleted. The unique index still holds; nothing bypasses it.

## Snapshot identity and lookup

`FindSnapshot(query)` accepts either a numeric ID or a name. `list` shows both. Most
commands take one or two of these as positional arguments. `snapshot` additionally uses
`GetLastSnapshot(rootPath)` to offer resume/incremental behaviour for the same root.

## Hash-strategy negotiation (why snapshots aren't always comparable)

`app/diff.go` picks the algorithm both snapshots share: SHA-1 if both have it, else MD5 if
both have it, else it errors. This is the only place `snapshots.hashes` is really used, and
it is the reason `--md5` exists at all: to make a modern snapshot comparable with a
legacy `dupe-finder` import, which has MD5 only. See [goals.md](goals.md) for that lineage
and [cli.md](cli.md) for the legacy file format.

## Connection settings

`NewSqliteStore` opens `modernc.org/sqlite` (pure Go — see [build.md](build.md)) through
`buildDSN`, which encodes the connection pragmas in the URI:

```
file:<path>?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)
```

`buildDSN` percent-escapes `%`, `?`, and `#` in the path, since a raw `?` or `#` would
otherwise be read as the query/fragment delimiter. Note that `journal_mode=WAL` is
persistent in the database file itself, not per-connection.

Bulk inserts go through `BatchAddFiles` in transactions of `DBBatchSize` (1000)
prepared-statement executions, which is the main thing keeping snapshot writes tolerable.

**Timestamps are written by `dbTime()` as UTC RFC3339Nano.** Do not pass a `time.Time`
straight to the driver: modernc renders it with Go's `String()` method, which appends a
monotonic-clock reading (`m=+0.002088029`) that SQLite's own date functions cannot parse.
Reads accept four historical layouts — see [build.md](build.md).
