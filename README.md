# Fluxion

**Fluxion** is a filesystem snapshot and verification tool designed to help you track, verify, and clean up your data as it changes over time.

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
    *   `--resume`: Force resume of an interrupted scan.
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

## Development

Run tests and verification:
```bash
make test
make verify
```

## Credits

*   **endotronic** - Concept & Development
*   **Antigravity** (Google DeepMind) - Development & Implementation