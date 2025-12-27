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
    ./fluxion snapshot /path/to/data
    # or
    ./fluxion s /path/to/data
    ```

*   **`list` (`l`)**: List all snapshots in the database.
    ```bash
    ./fluxion list --db filesystem.db
    # or
    ./fluxion l --db filesystem.db
    ```

*   **`diff` (`d`)**: Compare two snapshots to see what changed (additions, removals, modifications, moves, copies).
    ```bash
    ./fluxion diff --db filesystem.db <old_id> <new_id>
    ```

*   **`import` (`i`)**: Merge snapshots from another Fluxion database.
    ```bash
    ./fluxion import --db main.db --source other.db
    ```

*   **`import-legacy`**: Import legacy flat-file snapshots.
    ```bash
    ./fluxion import-legacy --sizes sizes.txt --hashes hashes.txt --db main.db
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