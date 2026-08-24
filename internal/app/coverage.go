package app

import (
	"fmt"
	"os"
	"path"
	"strings"

	"fluxion/internal/models"
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"
	"fluxion/internal/util"

	"github.com/schollz/progressbar/v3"
	"github.com/sirupsen/logrus"
)

// DefaultCoverageLimit is how many uncovered entries are listed before the rest
// are summarised. The totals are always complete; only the listing is capped.
const DefaultCoverageLimit = 50

type CoverageConfig struct {
	DBPath string

	// CandidateQuery names the snapshot being considered for deletion.
	CandidateQuery string
	// KeeperQueries name the snapshots that would survive it.
	KeeperQueries []string

	// MinSize skips files below this many bytes entirely. They are counted as
	// skipped, never as covered.
	MinSize int64

	// Limit caps listed entries; 0 lists everything.
	Limit int

	// ByDir aggregates the listing to one line per containing directory.
	ByDir bool

	Excludes []string
}

// CoverageResult is the answer, separated from how it was printed.
type CoverageResult struct {
	TotalFiles, TotalBytes         int64
	UncoveredFiles, UncoveredBytes int64
	NoHashFiles, NoHashBytes       int64
	ExcludedFiles                  int64
	HashType                       string
}

// Covered reports whether every file that was checked has its content present
// in one of the keeper snapshots.
//
// Files with no comparable hash count against coverage. They are not evidence
// of absence, but they are not evidence of presence either, and goals.md is
// unambiguous about which way that has to fall.
func (r CoverageResult) Covered() bool {
	return r.UncoveredFiles == 0 && r.NoHashFiles == 0
}

// RunCoverage answers "is every file in the candidate snapshot present, by
// content, in at least one of the keeper snapshots?"
//
// It is the cheap form of `diff --update`: the same question, minus the parts of
// a diff that a delete decision does not need. Paths are never compared, so a
// tree that was reorganised beyond recognition still reads as covered, and no
// diff tree is built - see knowledge/diff-memory.md.
func RunCoverage(cfg CoverageConfig) (CoverageResult, error) {
	var res CoverageResult

	if cfg.DBPath == "" {
		return res, fmt.Errorf("DB path is required")
	}
	if cfg.CandidateQuery == "" {
		return res, fmt.Errorf("a snapshot to check is required")
	}
	if len(cfg.KeeperQueries) == 0 {
		return res, fmt.Errorf("at least one snapshot to check against is required")
	}

	var dbStore store.Store
	dbStore, err := sqlite.NewSqliteStore(cfg.DBPath)
	if err != nil {
		return res, fmt.Errorf("error opening DB: %w", err)
	}
	defer dbStore.Close()

	candidate, err := dbStore.FindSnapshot(cfg.CandidateQuery)
	if err != nil {
		return res, fmt.Errorf("could not find snapshot '%s': %w", cfg.CandidateQuery, err)
	}

	keepers := make([]*models.Snapshot, 0, len(cfg.KeeperQueries))
	keeperIDs := make([]int64, 0, len(cfg.KeeperQueries))
	for _, q := range cfg.KeeperQueries {
		k, err := dbStore.FindSnapshot(q)
		if err != nil {
			return res, fmt.Errorf("could not find snapshot '%s': %w", q, err)
		}
		if k.ID == candidate.ID {
			return res, fmt.Errorf("snapshot '%s' is being checked against itself", k.Name)
		}
		keepers = append(keepers, k)
		keeperIDs = append(keeperIDs, k.ID)
	}

	hashType, err := commonHash(candidate, keepers)
	if err != nil {
		return res, err
	}
	res.HashType = hashType

	// An incomplete scan is missing files it never managed to read. On the
	// candidate side that understates what would be lost; on a keeper side it
	// can only make coverage look worse, so it is reported but not fatal.
	warnIncomplete(candidate, "candidate")
	for _, k := range keepers {
		warnIncomplete(k, "keeper")
	}

	res.TotalFiles, res.TotalBytes, err = dbStore.SnapshotTotals(candidate.ID, cfg.MinSize)
	if err != nil {
		return res, fmt.Errorf("error counting files: %w", err)
	}

	keeperNames := make([]string, len(keepers))
	for i, k := range keepers {
		keeperNames[i] = k.Name
	}
	fmt.Printf("Checking %s at %s (%s, %s)\n",
		candidate.Name, candidate.RootPath, plural(res.TotalFiles, "file"), util.FormatBytes(res.TotalBytes))
	fmt.Printf("against:  %s\n", strings.Join(keeperNames, ", "))
	fmt.Printf("by:       %s content hash\n\n", strings.ToUpper(hashType))

	// The bar counts rows the query returned, which are exactly the uncovered
	// files - SQLite does the filtering. It shares the terminal with the listing
	// below, so it needs ANSI clearing to avoid overwriting it; and since those
	// escape codes are noise in a pipe, the whole bar is switched off when
	// stderr is not a terminal.
	tty := isTerminal(os.Stderr)
	bar := progressbar.NewOptions64(-1,
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionSetDescription("Uncovered so far"),
		progressbar.OptionClearOnFinish(),
		progressbar.OptionShowCount(),
		progressbar.OptionUseANSICodes(true),
		progressbar.OptionSetVisibility(tty),
	)

	listed := 0
	printed := func() bool { return cfg.Limit == 0 || listed < cfg.Limit }

	// By-directory aggregation. Results arrive in path order, so a directory is
	// complete as soon as a file with a different parent shows up: one line per
	// directory, no state beyond the one being accumulated.
	var curDir string
	var curFiles, curBytes int64
	flushDir := func() {
		if curFiles == 0 {
			return
		}
		if printed() {
			bar.Clear()
			fmt.Printf("  %-8s %10s  %s/\n", util.Comma(curFiles), util.FormatBytes(curBytes), curDir)
			listed++
		}
		curFiles, curBytes = 0, 0
	}

	err = dbStore.IterateUncovered(candidate.ID, keeperIDs, hashType, cfg.MinSize,
		func(f models.FileRecord) error {
			bar.Add(1)

			if isExcluded(f.Path, candidate.RootPath, cfg.Excludes) {
				res.ExcludedFiles++
				return nil
			}

			if hashOf(f, hashType) == "" {
				res.NoHashFiles++
				res.NoHashBytes += f.SizeBytes
				return nil
			}

			res.UncoveredFiles++
			res.UncoveredBytes += f.SizeBytes

			if cfg.ByDir {
				dir := path.Dir(f.Path)
				if dir != curDir {
					flushDir()
					curDir = dir
				}
				curFiles++
				curBytes += f.SizeBytes
				return nil
			}

			if printed() {
				bar.Clear()
				fmt.Printf("  %10s  %s\n", util.FormatBytes(f.SizeBytes), f.Path)
				listed++
			}
			return nil
		})
	flushDir()
	bar.Finish()

	if err != nil {
		return res, fmt.Errorf("error checking coverage: %w", err)
	}

	unlisted := res.UncoveredFiles - int64(listed)
	if cfg.ByDir {
		unlisted = 0 // listed counts directories, not files; the summary carries the truth
	}
	if cfg.Limit > 0 && unlisted > 0 {
		fmt.Printf("  ... %s more (--limit 0 to list all)\n", util.Comma(unlisted))
	}

	printCoverageSummary(res, cfg)
	return res, nil
}

func printCoverageSummary(res CoverageResult, cfg CoverageConfig) {
	covered := res.TotalFiles - res.UncoveredFiles - res.NoHashFiles - res.ExcludedFiles
	coveredBytes := res.TotalBytes - res.UncoveredBytes - res.NoHashBytes

	fmt.Println()
	fmt.Println("Summary")
	fmt.Printf("  covered:      %12s  %10s\n", plural(covered, "file"), util.FormatBytes(coveredBytes))
	fmt.Printf("  NOT covered:  %12s  %10s\n", plural(res.UncoveredFiles, "file"), util.FormatBytes(res.UncoveredBytes))
	if res.NoHashFiles > 0 {
		fmt.Printf("  no %-4s hash: %12s  %10s  (cannot be compared - counted as not covered)\n",
			res.HashType, plural(res.NoHashFiles, "file"), util.FormatBytes(res.NoHashBytes))
	}
	if res.ExcludedFiles > 0 {
		fmt.Printf("  excluded:     %12s\n", plural(res.ExcludedFiles, "file"))
	}
	if cfg.MinSize > 0 {
		fmt.Printf("  (files below %s were not checked)\n", util.FormatBytes(cfg.MinSize))
	}

	fmt.Println()
	if res.Covered() {
		fmt.Println("Every file checked has its content present in the snapshots above.")
		fmt.Println("Content only - this says nothing about paths, and nothing about which side is newer.")
	} else {
		fmt.Println("NOT fully covered. Deleting the candidate would lose the content listed above.")
	}
}

// commonHash picks an algorithm that every snapshot involved actually carries.
func commonHash(candidate *models.Snapshot, keepers []*models.Snapshot) (string, error) {
	for _, want := range []string{"sha1", "md5"} {
		if !hasHash(candidate, want) {
			continue
		}
		ok := true
		for _, k := range keepers {
			if !hasHash(k, want) {
				ok = false
				break
			}
		}
		if ok {
			return want, nil
		}
	}

	logrus.Errorf("%s has: %v", candidate.Name, candidate.Hashes)
	for _, k := range keepers {
		logrus.Errorf("%s has: %v", k.Name, k.Hashes)
	}
	return "", fmt.Errorf("snapshots share no common hash algorithm")
}

func hasHash(s *models.Snapshot, want string) bool {
	for _, h := range s.Hashes {
		if h == want {
			return true
		}
	}
	return false
}

// isTerminal reports whether f is attached to a terminal, so that progress
// output can be suppressed when it would only pollute a pipe.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// plural renders a count with its noun, separated and pluralised.
func plural(n int64, noun string) string {
	if n == 1 {
		return util.Comma(n) + " " + noun
	}
	return util.Comma(n) + " " + noun + "s"
}

func hashOf(f models.FileRecord, hashType string) string {
	if hashType == "md5" {
		return f.MD5
	}
	return f.SHA1
}

func warnIncomplete(s *models.Snapshot, role string) {
	if s.Status != models.StatusCompleted {
		logrus.Warnf("%s snapshot '%s' has status %q - it may be missing files.", role, s.Name, s.Status)
	}
	if s.ErrorCount > 0 {
		logrus.Warnf("%s snapshot '%s' failed to read %d file(s); anything it missed is not accounted for here.",
			role, s.Name, s.ErrorCount)
	}
}
