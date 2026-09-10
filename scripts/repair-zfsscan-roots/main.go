// Command repair-zfsscan-roots rewrites the paths in a database produced by a
// pre-2026-09-10 `zfs-scan`.
//
// Those runs recorded the throwaway directory each dataset was mounted at as
// the snapshot's root_path, and stored every file path underneath it - so once
// the scan finished and the directory was removed, every path the snapshot could
// produce pointed at nothing, and two datasets scanned in the same run were
// indistinguishable (`/tmp/fluxion-zfsscan-3181317525/` vs
// `/tmp/fluxion-zfsscan-574009572/`). Issue 2.8 in knowledge/known-issues.md.
//
// `zfs-scan` now records the dataset name instead. This brings an existing
// database into line, so a scan that took days does not have to be repeated.
// What it changes, per affected snapshot:
//
//	snapshots.root_path : /tmp/fluxion-zfsscan-NNNN  ->  luna/mike/archives
//	files.path          : /tmp/fluxion-zfsscan-NNNN/Archives/x  ->  luna/mike/archives/Archives/x
//
// Nothing else moves: hashes, sizes and mtimes are untouched, and the path
// *within* the dataset is unchanged, so comparisons against these snapshots give
// the same answers before and after. Only what is displayed and stored changes.
//
// It is safe to interrupt and re-run. Files are converted before the snapshot's
// root_path is, so a half-finished snapshot still matches on the next run and is
// picked up where it stopped. It reports what it would do and changes nothing
// unless given --apply.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fluxion/internal/app"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := flag.String("db", "", "path to the fluxion database to repair (required)")
	apply := flag.Bool("apply", false, "actually write the changes; without this, only report them")
	batch := flag.Int("batch", 50000, "rows to convert per transaction")
	flag.Parse()

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --db is required")
		flag.Usage()
		os.Exit(2)
	}
	if *batch < 1 {
		fmt.Fprintln(os.Stderr, "Error: --batch must be at least 1")
		os.Exit(2)
	}

	if err := run(*dbPath, *apply, *batch); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// affected is one snapshot needing repair.
type affected struct {
	id      int64
	name    string
	oldRoot string
}

func run(dbPath string, apply bool, batchSize int) error {
	// The same pragmas the store opens with. journal_mode is already persisted
	// in the file, but synchronous=NORMAL is per-connection and is the
	// difference between half an hour and several. It is the right trade here
	// because the repair is resumable: the worst an ill-timed power cut costs
	// is the last batch, which the next run redoes.
	db, err := sql.Open("sqlite", "file:"+dbPath+
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return fmt.Errorf("opening %s: %w", dbPath, err)
	}
	defer db.Close()

	todo, skipped, err := findAffected(db)
	if err != nil {
		return err
	}

	for _, s := range skipped {
		fmt.Printf("SKIP  %-44s root_path %q\n", s.name, s.oldRoot)
	}
	if len(skipped) > 0 {
		fmt.Printf("\n%d snapshot(s) skipped: their root_path looks like a zfs-scan mount but their\n"+
			"name is not usable as a dataset path, so there is nothing to rewrite them to.\n"+
			"Rename them first if they should be repaired.\n\n", len(skipped))
	}

	if len(todo) == 0 {
		fmt.Println("Nothing to repair: no snapshot has a zfs-scan temporary mount as its root_path.")
		return nil
	}

	fmt.Printf("%d snapshot(s) to repair:\n", len(todo))
	for _, s := range todo {
		fmt.Printf("  %-44s %s  ->  %s\n", s.name, s.oldRoot, s.name)
	}

	if !apply {
		fmt.Printf("\nDry run: nothing was changed. Re-run with --apply to write it.\n" +
			"Back the database up first if you have the room - this rewrites every file row\n" +
			"of every snapshot listed above, and there is no undo.\n")
		return nil
	}

	fmt.Println()
	start := time.Now()
	var total int64
	for _, s := range todo {
		n, err := repairSnapshot(db, s, batchSize)
		total += n
		if err != nil {
			return fmt.Errorf("repairing %s (%d rows converted before the failure; re-run to continue): %w",
				s.name, n, err)
		}
		fmt.Printf("  %-44s %d rows\n", s.name, n)
	}
	fmt.Printf("\nDone: %d snapshot(s), %d file rows, in %s.\n", len(todo), total, time.Since(start).Round(time.Second))
	return nil
}

// findAffected splits the candidate snapshots into those that can be repaired
// and those whose name cannot serve as a root path.
func findAffected(db *sql.DB) (todo, skipped []affected, err error) {
	rows, err := db.Query(`SELECT id, name, root_path FROM snapshots ORDER BY id`)
	if err != nil {
		return nil, nil, fmt.Errorf("listing snapshots: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var a affected
		if err := rows.Scan(&a.id, &a.name, &a.oldRoot); err != nil {
			return nil, nil, err
		}
		if !isZFSScanTempMount(a.oldRoot) {
			continue
		}
		if usableAsRoot(a.name) {
			todo = append(todo, a)
		} else {
			skipped = append(skipped, a)
		}
	}
	return todo, skipped, rows.Err()
}

// isZFSScanTempMount reports whether root looks like a directory zfs-scan
// mounted a dataset at - os.MkdirTemp's output for app.ZFSScanTempPrefix.
func isZFSScanTempMount(root string) bool {
	base := filepath.Base(root)
	if !strings.HasPrefix(base, app.ZFSScanTempPrefix) {
		return false
	}
	suffix := base[len(app.ZFSScanTempPrefix):]
	if suffix == "" {
		return false
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// usableAsRoot reports whether a snapshot's name can stand in as its root path.
// A zfs-scan snapshot is named after its dataset ("luna/mike/archives"), which
// is exactly what is wanted; anything else is left alone rather than guessed at.
func usableAsRoot(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return false
	}
	return !strings.ContainsAny(name, " \t\n")
}

// repairSnapshot converts one snapshot's file paths and then its root_path.
//
// The order matters and is what makes an interrupted run resumable: while any
// file row still carries the old prefix, the snapshot's root_path still names it,
// so the next run finds the snapshot again and finishes the job. Updating
// root_path first would strand the remainder with nothing left to identify them.
func repairSnapshot(db *sql.DB, s affected, batchSize int) (int64, error) {
	oldPrefix := strings.TrimSuffix(s.oldRoot, "/") + "/"
	// substr() is 1-based, so this is "everything after the old prefix".
	tailFrom := len(oldPrefix) + 1

	var converted int64
	for {
		res, err := db.Exec(`
			UPDATE files
			   SET path = ? || substr(path, ?)
			 WHERE rowid IN (
			       SELECT rowid FROM files
			        WHERE snapshot_id = ? AND path LIKE ? ESCAPE '\'
			        LIMIT ?)`,
			s.name+"/", tailFrom, s.id, likePrefix(oldPrefix), batchSize)
		if err != nil {
			return converted, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return converted, err
		}
		if n == 0 {
			break
		}
		converted += n
		fmt.Printf("\r  %-44s %d rows...", s.name, converted)
	}
	if converted > 0 {
		fmt.Print("\r")
	}

	if _, err := db.Exec(`UPDATE snapshots SET root_path = ? WHERE id = ?`, s.name, s.id); err != nil {
		return converted, fmt.Errorf("updating root_path: %w", err)
	}
	return converted, nil
}

// likePrefix turns a literal path prefix into a LIKE pattern, escaping the
// wildcards. A real path can contain % and _, and an unescaped one would match
// far more than intended - on a rewrite of 84 million rows that is not a
// mistake anyone would notice until much later.
func likePrefix(prefix string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(prefix) + "%"
}
