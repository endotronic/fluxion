# Fluxion

**Fluxion** is a filesystem snapshot and verification tool designed to help you track, verify, and clean up your data as it changes over time. Take snapshots of the metadata of your filesystem (files, content hashes, sizes, dates) and compare them to see what has changed between snapshots - how files moved, were added, or removed. Examine snapshots for file and directory duplicates shown to you at the highest levels of duplication. These tools can give you the confidence of a second opinion if your backups contain everything and when you are truly safe to delete backups.

## Status

This project is new and in active development. The roadmap is available in [ROADMAP.md](ROADMAP.md). It may be a bit cumbersome to use until the 1.0 release I have planned. The "snapshot" and "diff" commands are pretty solid, though, and have already proven very useful to me.

## Motivation

I have moved and copied all sorts of personal files in my life, leading to a mess of data that I've been working to clean up. This tool aims to provide some features to safely clean up data without accidentally removing anything. It can also be used to verify backups. It is a rework of a quick and dirty Python tool I made a while ago that I have succesfully been using to verify backups for years: [dupe-finder](https://github.com/endotronic/dupe-finder).

## Usage

### Building

You can build the tool using the provided Makefile:
```bash
make build
```
This produces the `fluxion` binary.

### Commands

**Fluxion** supports several subcommands (shortcuts in parentheses):

*   **`snapshot` (`s`)**: Scan a directory and create a snapshot of its contents.
    ```bash
    ./fluxion snapshot [options] <directory>
    ```
    *   `--db <path>`: Path to SQLite DB (default: `<dirname>.db` in current dir).
    *   `--name <name>`: Custom name for the snapshot (default: `<dirname>_<date>`).
    *   `--threads <n>`: Number of worker threads (default: NumCPU).
    *   `--md5`: Compute MD5 hashes in addition to SHA1 (back-compat for dupe-finder).
    *   `--new`: Force a new scan, ignoring previous snapshots.
    *   `--resume <name|id>`: Resume an interrupted scan.
    *   `--fail-on-mount`: Fail if a mount point is encountered (default: false).
    *   `--cross-mounts`: Traverse mount points (default: true).

*   **`list` (`l`)**: List all snapshots in the database.
    ```bash
    ./fluxion list --db <db_path>
    ```

*   **`diff` (`d`)**: Compare two snapshots to see what changed.
    ```bash
    ./fluxion diff --db <db_path> <old_id_or_name> <new_id_or_name>
    ```
    *   `--update` (`-u`): Show only files missing or modified in the second snapshot.
    *   `--exclude` (`-e`): Exclude directory from diff (relative to root or absolute). Can be used (multiple times).
    *   `--no-copies`: Do not detect file copies.
    *   `--no-moves`: Do not detect file moves. Use both to treat moves/copies as pure additions/removals.

*   **`merge` (`m`)**: Merge multiple snapshots into a single new snapshot.
    ```bash
    ./fluxion merge --name <new_name> [options] <snap1> <snap2> ...
    ```
    *   `--db <path>`: Path to SQLite DB.
    *   `--hostname <name>`: Override computer name for the new snapshot.

*   **`import` (`i`)**: Merge snapshots from another Fluxion database.
    ```bash
    ./fluxion import --db <dest_db> --source <source_db>
    ```

*   **`import-legacy`**: Import legacy flat-file snapshots from [dupe-finder](https://github.com/endotronic/dupe-finder).
    ```bash
    ./fluxion import-legacy [options] <hashes_file>
    ```
    *   `--db <dest_db>`: Path to sqlite DB (required).
    *   `--sizes <file>`: Optional path to sizes file. If omitted, attempts to infer from hashes filename (e.g. `_hashes.txt` -> `_sizes.txt`).
    *   `--root <path>`: Root path override. If omitted, auto-detected from hashes file.

*   **`size`**: Report the total size of files in a snapshot.
    ```bash
    ./fluxion size --db <db_path> <snapshot_id_or_name>
    ```
    *   `--total-bytes`: Show raw byte count instead of human-readable format.

*   **`version`**: Print the current version.
    ```bash
    ./fluxion version
    ```

## Development

Run tests and verification:
```bash
make test
make verify
```

## Credits

*   **endotronic** - Concept & Development
*   **Antigravity** (Google DeepMind) - Development & Implementation