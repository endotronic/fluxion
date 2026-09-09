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

	// Rollup reports recursive covered/not-covered/no-hash counts per
	// directory, collapsing any subtree that is entirely covered (or entirely
	// not) into one line instead of drilling into it. Mutually exclusive with
	// ByDir - see RunCoverage.
	Rollup bool

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
	if cfg.Rollup && cfg.ByDir {
		return res, fmt.Errorf("--rollup and --by-dir are mutually exclusive")
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

	if cfg.Rollup {
		return runCoverageRollup(dbStore, candidate, keeperIDs, hashType, cfg)
	}

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

// rollupFrame accumulates one directory's recursive covered/uncovered/no-hash
// totals as files stream past in path order. Fields hold the SUBTREE total,
// not just files directly in dir: closing a child folds its totals into its
// parent's same fields (see absorb), so by the time a frame itself closes its
// counts already cover everything beneath it.
type rollupFrame struct {
	dir string

	coveredFiles, coveredBytes     int64
	uncoveredFiles, uncoveredBytes int64
	noHashFiles, noHashBytes       int64

	// notableLines are already-rendered lines from closed children that were
	// NOT homogeneously covered (mixed, or homogeneously not-covered). These
	// are never merged away - doing so could hide a real loss.
	notableLines []string

	// A homogeneously-covered child is folded into this aggregate instead of
	// getting its own line: merging pure good news is safe (nothing is being
	// hidden), and it is what keeps a tree with thousands of untouched
	// subdirectories from printing thousands of "fully covered" lines.
	boringCoveredDirs, boringCoveredFiles int64
	boringCoveredBytes                    int64
}

func (f *rollupFrame) add(status models.CoverageStatus, size int64) {
	switch status {
	case models.CoverageCovered:
		f.coveredFiles++
		f.coveredBytes += size
	case models.CoverageUncovered:
		f.uncoveredFiles++
		f.uncoveredBytes += size
	case models.CoverageNoHash:
		f.noHashFiles++
		f.noHashBytes += size
	}
}

func (f *rollupFrame) isFullyCovered() bool {
	return f.uncoveredFiles == 0 && f.noHashFiles == 0
}

func (f *rollupFrame) isFullyNotCovered() bool {
	return f.coveredFiles == 0
}

// absorb folds a just-closed child's recursive totals into f, and decides
// whether the child earns its own line or merges into f's boring-covered note.
func (f *rollupFrame) absorb(child *rollupFrame) {
	f.coveredFiles += child.coveredFiles
	f.coveredBytes += child.coveredBytes
	f.uncoveredFiles += child.uncoveredFiles
	f.uncoveredBytes += child.uncoveredBytes
	f.noHashFiles += child.noHashFiles
	f.noHashBytes += child.noHashBytes

	if child.isFullyCovered() {
		f.boringCoveredDirs++
		f.boringCoveredFiles += child.coveredFiles
		f.boringCoveredBytes += child.coveredBytes
		return
	}
	f.notableLines = append(f.notableLines, child.render()...)
}

// render decides this frame's own verdict and returns the line(s) it
// contributes to its parent: one line if homogeneous (fully covered, or
// fully not covered), or its own summary plus its children's detail if mixed.
func (f *rollupFrame) render() []string {
	total := f.coveredFiles + f.uncoveredFiles + f.noHashFiles
	if total == 0 {
		return nil
	}

	label := f.dir + "/"

	if f.isFullyCovered() {
		return []string{fmt.Sprintf("  covered: %-12s %10s  %s  [fully covered]",
			util.Comma(f.coveredFiles), util.FormatBytes(f.coveredBytes), label)}
	}

	if f.isFullyNotCovered() {
		detail := plural(f.uncoveredFiles, "file")
		if f.noHashFiles > 0 {
			detail += fmt.Sprintf(" + %s with no hash", util.Comma(f.noHashFiles))
		}
		return []string{fmt.Sprintf("  NOT covered: %-8s %10s  %s  [NOT covered]",
			detail, util.FormatBytes(f.uncoveredBytes+f.noHashBytes), label)}
	}

	// Mixed: this frame's own summary, then its children's own decisions,
	// indented one level deeper.
	summary := fmt.Sprintf("  covered: %-8s not covered: %-8s",
		util.Comma(f.coveredFiles), util.Comma(f.uncoveredFiles))
	if f.noHashFiles > 0 {
		summary += fmt.Sprintf(" no-hash: %-8s", util.Comma(f.noHashFiles))
	}
	summary += fmt.Sprintf(" %10s  %s  [mixed]",
		util.FormatBytes(f.coveredBytes+f.uncoveredBytes+f.noHashBytes), label)

	lines := []string{summary}
	for _, l := range f.notableLines {
		lines = append(lines, "  "+l)
	}
	if f.boringCoveredDirs > 0 {
		dirWord := "subdirectory"
		if f.boringCoveredDirs != 1 {
			dirWord = "subdirectories"
		}
		lines = append(lines, fmt.Sprintf("    (+ %s %s under %s, %s, fully covered)",
			util.Comma(f.boringCoveredDirs), dirWord, label, plural(f.boringCoveredFiles, "file")))
	}
	return lines
}

// isAncestorOrSelf reports whether dir is ancestor itself or a real path
// descendant of it - a boundary-aware check, unlike the raw strings.HasPrefix
// isExcluded uses (see known-issues.md for why that one is a confirmed bug).
func isAncestorOrSelf(ancestor, dir string) bool {
	return ancestor == dir || strings.HasPrefix(dir, ancestor+"/")
}

// runCoverageRollup implements `coverage --rollup`: recursive covered/
// not-covered/no-hash counts per directory, collapsing any subtree that is
// entirely one verdict into a single line instead of drilling into it.
//
// Memory is O(tree depth), not O(files or directories): exactly one open
// rollupFrame per directory on the current path, exactly as diff-memory.md's
// phase-2 streaming design describes for `diff`, applied here for coverage's
// three-way covered/uncovered/no-hash status instead of diff's five-way one.
// This only works because IterateWithCoverage's ORDER BY f.path gives every
// subtree contiguous rows (confirmed in diff-memory.md's "Fact 2") - it does
// NOT need DFS-safe ordering, because unlike diff's move/copy twin detection,
// a recursive count rollup never needs to relate a leaf file to a same-named
// directory, only to know when one directory's rows have ended.
func runCoverageRollup(dbStore store.Store, candidate *models.Snapshot, keeperIDs []int64, hashType string, cfg CoverageConfig) (CoverageResult, error) {
	var res CoverageResult
	res.HashType = hashType

	var err error
	res.TotalFiles, res.TotalBytes, err = dbStore.SnapshotTotals(candidate.ID, cfg.MinSize)
	if err != nil {
		return res, fmt.Errorf("error counting files: %w", err)
	}

	tty := isTerminal(os.Stderr)
	bar := progressbar.NewOptions64(res.TotalFiles,
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionSetDescription("Rolling up"),
		progressbar.OptionClearOnFinish(),
		progressbar.OptionShowCount(),
		progressbar.OptionUseANSICodes(true),
		progressbar.OptionSetVisibility(tty),
	)

	var stack []*rollupFrame
	var topLevel []*rollupFrame

	closeTop := func() {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if len(stack) > 0 {
			stack[len(stack)-1].absorb(top)
		} else {
			topLevel = append(topLevel, top)
		}
	}

	pushTo := func(dir string) {
		for len(stack) > 0 && !isAncestorOrSelf(stack[len(stack)-1].dir, dir) {
			closeTop()
		}

		var cur string
		var rest []string
		if len(stack) == 0 {
			parts := strings.Split(strings.TrimPrefix(dir, "/"), "/")
			cur, rest = "/"+parts[0], parts[1:]
			stack = append(stack, &rollupFrame{dir: cur})
		} else if top := stack[len(stack)-1]; top.dir != dir {
			cur = top.dir
			suffix := strings.TrimPrefix(dir, top.dir+"/")
			rest = strings.Split(suffix, "/")
		}
		for _, p := range rest {
			cur = cur + "/" + p
			stack = append(stack, &rollupFrame{dir: cur})
		}
	}

	err = dbStore.IterateWithCoverage(candidate.ID, keeperIDs, hashType, cfg.MinSize,
		func(f models.FileRecord, status models.CoverageStatus) error {
			bar.Add(1)

			if isExcluded(f.Path, candidate.RootPath, cfg.Excludes) {
				res.ExcludedFiles++
				return nil
			}

			pushTo(path.Dir(f.Path))
			stack[len(stack)-1].add(status, f.SizeBytes)

			switch status {
			case models.CoverageUncovered:
				res.UncoveredFiles++
				res.UncoveredBytes += f.SizeBytes
			case models.CoverageNoHash:
				res.NoHashFiles++
				res.NoHashBytes += f.SizeBytes
			}
			return nil
		})
	bar.Finish()
	if err != nil {
		return res, fmt.Errorf("error checking coverage: %w", err)
	}

	for len(stack) > 0 {
		closeTop()
	}

	for _, root := range topLevel {
		for _, l := range root.render() {
			fmt.Println(l)
		}
	}

	printCoverageSummary(res, cfg)
	return res, nil
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
