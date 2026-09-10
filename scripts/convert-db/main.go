// Command convert-db copies an old fluxion database into a new one, correcting
// what the code that wrote it got wrong.
//
// It exists for one job: a scan that took days, made with a binary that
// predates the fixes below, which nobody wants to repeat. It is deliberately a
// separate program rather than a migration, so that none of this has to live in
// the code that runs every day.
//
// What it corrects:
//
//   - **zfs-scan roots.** Those runs recorded the throwaway directory each
//     dataset was mounted at (`/tmp/fluxion-zfsscan-3181317525`) as the
//     snapshot's root_path, with every file path underneath it. The directory
//     is gone by the time anyone reads the snapshot, and two datasets from one
//     run got two indistinguishable roots, so a diff between them could not say
//     which side a line came from. They become the dataset name.
//   - **The CHECK on `files`.** Older databases constrain every row to carry a
//     hash, which a metadata-only scan (`snapshot --no-hash`) cannot satisfy.
//     The destination is created by the current code, so it simply does not
//     have it.
//
// What it does not do: invent anything. Hashes, sizes, mtimes, timestamps,
// statuses and error counts are copied across exactly as they were, and a
// column the source does not have is left at its default rather than guessed
// at. The path *within* each dataset is unchanged, so every comparison gives
// the same answer before and after.
//
// The source database is opened read-only and is never written to - not even to
// migrate its schema. Re-running is safe: each snapshot is copied whole or not
// at all, and one already fully present in the destination is skipped.
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
	"fluxion/internal/store/sqlite"
	"fluxion/internal/util"

	_ "modernc.org/sqlite"
)

func main() {
	src := flag.String("src", "", "the database to read (required; opened read-only, never modified)")
	dst := flag.String("out", "", "the database to create (required; must not already be a file unless --resume)")
	resume := flag.Bool("resume", false, "continue into an existing --out, skipping snapshots already copied there")
	batch := flag.Int("batch", 20000, "file rows per transaction")
	flag.Parse()

	if *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "Error: --src and --out are both required")
		flag.Usage()
		os.Exit(2)
	}
	if *batch < 1 {
		fmt.Fprintln(os.Stderr, "Error: --batch must be at least 1")
		os.Exit(2)
	}

	if err := convert(*src, *dst, *resume, *batch); err != nil {
		fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
		os.Exit(1)
	}
}

type snapshot struct {
	id       int64
	name     string
	rootPath string
	newRoot  string
	cols     map[string]any // every source column, by name
}

func convert(srcPath, dstPath string, resume bool, batchSize int) error {
	if _, err := os.Stat(dstPath); err == nil && !resume {
		return fmt.Errorf("%s already exists; delete it, choose another --out, or pass --resume", dstPath)
	}

	if err := checkRoom(srcPath, dstPath); err != nil {
		return err
	}

	// Create the destination through the normal store, so it gets exactly the
	// schema the current code makes - including no CHECK on files.
	st, err := sqlite.NewSqliteStore(dstPath)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dstPath, err)
	}
	if ok, err := st.SupportsHashlessFiles(); err != nil {
		st.Close()
		return err
	} else if !ok {
		st.Close()
		return fmt.Errorf("%s still constrains file rows to carry a hash; it is not a fresh database", dstPath)
	}
	st.Close()

	// Read-only, and immutable so SQLite does not try to recover a hot journal
	// on a database another process may still be writing.
	src, err := sql.Open("sqlite", "file:"+srcPath+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return fmt.Errorf("opening %s: %w", srcPath, err)
	}
	defer src.Close()

	dst, err := sql.Open("sqlite", "file:"+dstPath+
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return fmt.Errorf("opening %s: %w", dstPath, err)
	}
	defer dst.Close()

	snaps, err := readSnapshots(src)
	if err != nil {
		return err
	}
	if len(snaps) == 0 {
		return fmt.Errorf("%s holds no snapshots", srcPath)
	}

	fileCols, err := columnsOf(src, "files")
	if err != nil {
		return err
	}
	shared := intersect(fileCols, []string{
		"snapshot_id", "path", "filename", "size_bytes", "mod_time", "sha1", "md5",
	})

	fmt.Printf("Converting %s -> %s\n", srcPath, dstPath)
	fmt.Printf("%d snapshot(s); copying file columns: %s\n\n", len(snaps), strings.Join(shared, ", "))

	start := time.Now()
	var totalRows int64
	for _, s := range snaps {
		n, skipped, err := copySnapshot(src, dst, s, shared, batchSize)
		if err != nil {
			return fmt.Errorf("copying %s: %w", s.name, err)
		}
		totalRows += n
		switch {
		case skipped:
			fmt.Printf("  %-44s already present, skipped\n", s.name)
		case s.newRoot != s.rootPath:
			fmt.Printf("  %-44s %d rows   root %s -> %s\n", s.name, n, s.rootPath, s.newRoot)
		default:
			fmt.Printf("  %-44s %d rows\n", s.name, n)
		}
	}

	fmt.Printf("\nDone: %d snapshot(s), %d file rows, in %s.\n",
		len(snaps), totalRows, time.Since(start).Round(time.Second))
	fmt.Printf("The source database was not modified.\n")
	return nil
}

// checkRoom warns when the destination filesystem looks too small to hold a
// copy. A conversion that dies two hours in with ENOSPC is a bad way to find out.
func checkRoom(srcPath, dstPath string) error {
	info, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", srcPath, err)
	}
	dir := filepath.Dir(dstPath)
	free, err := util.GetFSAvail(dir)
	if err != nil {
		return nil // not being able to ask is not a reason to refuse
	}
	fmt.Printf("Source is %s; %s free at %s.\n",
		util.FormatBytes(info.Size()), util.FormatBytes(int64(free)), dir)
	if int64(free) < info.Size() {
		return fmt.Errorf("not enough room: the copy needs roughly %s and %s has %s free. "+
			"Point --out at a filesystem with space",
			util.FormatBytes(info.Size()), dir, util.FormatBytes(int64(free)))
	}
	return nil
}

// readSnapshots loads every snapshot with whatever columns the source has, and
// works out the corrected root for each.
func readSnapshots(src *sql.DB) ([]snapshot, error) {
	cols, err := columnsOf(src, "snapshots")
	if err != nil {
		return nil, err
	}
	rows, err := src.Query(`SELECT ` + strings.Join(quoteAll(cols), ", ") + ` FROM snapshots ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("reading snapshots: %w", err)
	}
	defer rows.Close()

	var out []snapshot
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		s := snapshot{cols: map[string]any{}}
		for i, c := range cols {
			s.cols[c] = vals[i]
		}
		s.id = asInt(s.cols["id"])
		s.name = asString(s.cols["name"])
		s.rootPath = asString(s.cols["root_path"])
		s.newRoot = correctedRoot(s.rootPath, s.name)
		s.cols["root_path"] = s.newRoot
		out = append(out, s)
	}
	return out, rows.Err()
}

// correctedRoot replaces a zfs-scan temporary mount with the dataset name the
// snapshot is called after. Anything else is left exactly as it is - a root that
// is merely unfamiliar is not a root that is wrong.
func correctedRoot(root, name string) string {
	if !isZFSScanTempMount(root) || !usableAsRoot(name) {
		return root
	}
	return name
}

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

// copySnapshot copies one snapshot and all its files. It reports whether the
// snapshot was already fully present and therefore skipped.
//
// Whole-or-nothing per snapshot is what makes an interrupted run resumable
// without tracking progress anywhere: a partial copy is discarded and redone,
// and a complete one is left alone.
func copySnapshot(src, dst *sql.DB, s snapshot, fileCols []string, batchSize int) (int64, bool, error) {
	var want int64
	if err := src.QueryRow(`SELECT count(*) FROM files WHERE snapshot_id = ?`, s.id).Scan(&want); err != nil {
		return 0, false, err
	}

	var have int64
	var exists int
	if err := dst.QueryRow(`SELECT count(*) FROM snapshots WHERE id = ?`, s.id).Scan(&exists); err != nil {
		return 0, false, err
	}
	if exists > 0 {
		if err := dst.QueryRow(`SELECT count(*) FROM files WHERE snapshot_id = ?`, s.id).Scan(&have); err != nil {
			return 0, false, err
		}
		if have == want {
			return have, true, nil
		}
		// Partial: start it over rather than trying to work out where it stopped.
		if _, err := dst.Exec(`DELETE FROM files WHERE snapshot_id = ?`, s.id); err != nil {
			return 0, false, err
		}
		if _, err := dst.Exec(`DELETE FROM snapshots WHERE id = ?`, s.id); err != nil {
			return 0, false, err
		}
	}

	if err := insertSnapshot(dst, s); err != nil {
		return 0, false, err
	}

	oldPrefix := strings.TrimSuffix(s.rootPath, "/") + "/"
	newPrefix := strings.TrimSuffix(s.newRoot, "/") + "/"
	rebase := s.newRoot != s.rootPath

	rows, err := src.Query(`SELECT `+strings.Join(quoteAll(fileCols), ", ")+
		` FROM files WHERE snapshot_id = ? ORDER BY id`, s.id)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()

	insert := `INSERT INTO files (` + strings.Join(quoteAll(fileCols), ", ") + `) VALUES (` +
		strings.TrimSuffix(strings.Repeat("?, ", len(fileCols)), ", ") + `)`
	pathIdx := indexOf(fileCols, "path")

	var copied int64
	for {
		tx, err := dst.Begin()
		if err != nil {
			return copied, false, err
		}
		stmt, err := tx.Prepare(insert)
		if err != nil {
			tx.Rollback()
			return copied, false, err
		}

		n := 0
		for n < batchSize && rows.Next() {
			vals := make([]any, len(fileCols))
			ptrs := make([]any, len(fileCols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				stmt.Close()
				tx.Rollback()
				return copied, false, err
			}
			if rebase && pathIdx >= 0 {
				p := asString(vals[pathIdx])
				if strings.HasPrefix(p, oldPrefix) {
					vals[pathIdx] = newPrefix + p[len(oldPrefix):]
				} else if p == strings.TrimSuffix(s.rootPath, "/") {
					vals[pathIdx] = s.newRoot
				}
			}
			if _, err := stmt.Exec(vals...); err != nil {
				stmt.Close()
				tx.Rollback()
				return copied, false, fmt.Errorf("inserting file row: %w", err)
			}
			n++
		}
		stmt.Close()
		if err := rows.Err(); err != nil {
			tx.Rollback()
			return copied, false, err
		}
		if err := tx.Commit(); err != nil {
			return copied, false, err
		}
		copied += int64(n)
		if n > 0 {
			fmt.Printf("\r  %-44s %d/%d rows...", s.name, copied, want)
		}
		if n < batchSize {
			break
		}
	}
	if copied > 0 {
		fmt.Print("\r\033[K")
	}
	return copied, false, nil
}

func insertSnapshot(dst *sql.DB, s snapshot) error {
	dstCols, err := columnsOf(dst, "snapshots")
	if err != nil {
		return err
	}
	var cols []string
	var vals []any
	for _, c := range dstCols {
		if v, ok := s.cols[c]; ok {
			cols = append(cols, c)
			vals = append(vals, v)
		}
	}
	q := `INSERT INTO snapshots (` + strings.Join(quoteAll(cols), ", ") + `) VALUES (` +
		strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ") + `)`
	_, err = dst.Exec(q, vals...)
	return err
}

func columnsOf(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("reading %s columns: %w", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("table %s has no columns - is this a fluxion database?", table)
	}
	return out, nil
}

// intersect keeps the wanted columns the source actually has, in the given
// order. A source missing one of them is an older database, not an error: the
// destination column keeps its default.
func intersect(have, want []string) []string {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	var out []string
	for _, w := range want {
		if set[w] {
			out = append(out, w)
		}
	}
	return out
}

func indexOf(cols []string, name string) int {
	for i, c := range cols {
		if c == name {
			return i
		}
	}
	return -1
}

func quoteAll(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = `"` + strings.ReplaceAll(c, `"`, `""`) + `"`
	}
	return out
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return ""
	}
}

func asInt(v any) int64 {
	if n, ok := v.(int64); ok {
		return n
	}
	return 0
}
