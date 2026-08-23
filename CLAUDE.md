# Fluxion — agent guide

Fluxion records **metadata-only snapshots of a filesystem** (path, size, mtime, SHA-1,
optional MD5) into SQLite, then diffs them, finds duplicates, and answers *"is it safe to
delete this copy?"* It never writes to or deletes the files it scans.

Go 1.25.5, module `fluxion` (imports are `fluxion/internal/...`). **Pure Go — no C
compiler needed**; the SQLite driver is `modernc.org/sqlite`. It was `mattn/go-sqlite3`
(cgo) until 2026-08-23, so an old checkout or an old binary may still fail at runtime with
a "this is a stub" error. See `knowledge/build.md`, which also covers the timestamp
handling that swap forced.

```bash
make build     # go build -o fluxion ./cmd/fluxion
make test      # go test ./...
make verify    # build + scripts/verify.sh end-to-end smoke test
```

## Knowledge base

`knowledge/` holds durable findings about this project — the reasoning behind the design
and the results of a full code review. **Read the relevant file before working in the
matching area**; they contain non-obvious facts and confirmed bugs that are not visible
from the code alone.

| File | Read it when… |
|---|---|
| `knowledge/goals.md` | you need to make any judgement call. Defines what the tool is for and the **severity rule** (a false "unchanged/present" answer can lose the user data; over-reporting only costs reading time) that should decide every trade-off. Also covers lineage from the author's Python `dupe-finder` and the explicit non-goals. |
| `knowledge/architecture.md` | you are adding a command, moving code between packages, or touching output/error handling. Covers the `cmd → app → store/algorithms` layering, the two testability seams (`store.Store`, `diff.FileIterator`), which store methods stream vs. materialise, and the inconsistent stdout/stderr conventions. |
| `knowledge/diff-algo.md` | **before touching `internal/diff` or `internal/app/diff.go` — this is the highest-risk code in the project.** Explains the unified two-snapshot tree, the ten-stage pipeline, `FileTwin` and the presence flags, the synthetic "merkle" directory hashes and their two failure modes, the `propagateStatus` rollup precedence, move/copy matching and consumption rules, the hidden-move-source fixed point, and the collapsing logic — plus what the property test asserts and the defects still open. |
| `knowledge/dupes-algo.md` | working on `internal/dupes` or `dupes` output. The highest-level-duplicate rule, the `covered` suppression, and why `--min-size` behaves the way it does on directories. |
| `knowledge/scanner.md` | working on `internal/scanner` or `snapshot`. What is recorded and — more importantly — **what is not** (symlinks, hard-link identity, empty directories, permissions), mount-boundary handling, and resume semantics. |
| `knowledge/data-model.md` | touching the schema, migrations, or snapshot lifecycle. Table definitions, the missing unique constraint on `(snapshot_id, path)`, how to add a migration safely, and the `in_progress`/`completed`/`failed`/`deleted` states. |
| `knowledge/cli.md` | adding or changing a command or flag, or dealing with legacy import/export. Full command/flag reference plus the `dupe-finder` flat-file format. |
| `knowledge/known-issues.md` | **before starting any bug-fix work.** Every issue found in review, ranked by the severity rule, with reproduction evidence. Items marked CONFIRMED were reproduced with executed tests. |
| `knowledge/build.md` | the build or tests misbehave, or you are touching the SQLite driver or timestamp storage. Which packages need a database to test (notably: `internal/diff` needs none), the cgo→pure-Go history, and what `scripts/verify.sh` covers. |

`ROADMAP.md` in the repo root is the author's feature backlog, in reverse-version order
with `--- main branch ---` marking the shipped/planned boundary. It is the source of truth
for intent; `knowledge/` is the source of truth for how things actually behave today.

## Working notes

- **Keep `knowledge/` current.** When you learn something durable — a confirmed bug, a
  non-obvious invariant, a design rationale — write it into the matching file rather than
  leaving it in a comment or a commit message. When you fix something listed in
  `known-issues.md`, remove it from that file in the same change.
- `internal/diff` is pure and needs no database: drive it with `mapToIter` from
  `internal/diff/test_helper.go`. Any diff investigation should start with a test there.
- Much of this codebase was AI-authored in 2023 and retains the author-agent's
  deliberation as comments — comments that ask questions and weigh options rather than
  describing the code. Treat them as archaeology; the load-bearing reasoning has been
  extracted into `knowledge/`.
- `internal/diff/property_test.go` asserts the invariants that catch the worst bugs —
  *every file that differs is accounted for somewhere in the output*, and *a collapsed
  Added/Removed directory line does not contradict the snapshots*. **Run it before the
  golden tests when changing `internal/diff`:** golden output tells you something changed,
  the property test tells you whether the change loses data.
