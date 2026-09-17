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

// DefaultRollupDetailMax is how few not-covered-or-no-hash files a --rollup
// subtree may hold before it stops recursing through directory structure and
// lists their actual paths instead.
const DefaultRollupDetailMax = 3

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

	// RollupDetailMax: a --rollup subtree whose not-covered-or-no-hash count
	// is at or below this stops recursing and lists the actual file paths
	// instead of drilling into directory structure to reach them. 0 disables
	// (never show files, only directory-level counts). Ignored outside Rollup.
	RollupDetailMax int

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

	// An empty candidate has nothing to lose - trivially "fully covered", not an
	// error. This can't be folded into commonHash the way an empty keeper is
	// (above): an empty keeper still leaves other keepers to negotiate a hash
	// type against, but an empty candidate leaves nothing to negotiate for at
	// all, so there is no hash type to report either. Checked by file count
	// rather than by candidate.Hashes being empty, because those mean different
	// things: zero files (a canmount=off ZFS container dataset, say) really is
	// nothing to lose, but a --no-hash scan with real files also has empty
	// Hashes and must still refuse - see knowledge/cli.md's --no-hash section -
	// so it has to fall through to commonHash's error below like it always has.
	candidateFileCount, err := dbStore.GetFileCount(candidate.ID)
	if err != nil {
		return res, fmt.Errorf("error counting candidate files: %w", err)
	}
	if candidateFileCount == 0 {
		fmt.Printf("Checking %s at %s (0 files) - empty, nothing to lose by deleting it.\n",
			candidate.Name, candidate.RootPath)
		return CoverageResult{}, nil
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

	// details buffers every not-covered/no-hash file under this subtree, but
	// only for as long as that count might still end at or under
	// RollupDetailMax - counts only grow as more files/children are folded
	// in, so the moment it's exceeded, it can never come back down, and the
	// buffer is dropped (detailsDropped) to bound memory. Whether the final
	// count actually stayed at or under the limit is decided in render().
	details        []detailEntry
	detailsDropped bool
}

// detailEntry is one buffered not-covered/no-hash file, for printing directly
// instead of drilling through directory structure to reach it.
type detailEntry struct {
	path   string
	size   int64
	status models.CoverageStatus
}

func (f *rollupFrame) add(status models.CoverageStatus, path string, size int64, detailMax int) {
	switch status {
	case models.CoverageCovered:
		f.coveredFiles++
		f.coveredBytes += size
		return
	case models.CoverageUncovered:
		f.uncoveredFiles++
		f.uncoveredBytes += size
	case models.CoverageNoHash:
		f.noHashFiles++
		f.noHashBytes += size
	}
	f.addDetail(detailEntry{path, size, status}, detailMax)
}

func (f *rollupFrame) addDetail(d detailEntry, detailMax int) {
	if f.detailsDropped || detailMax <= 0 {
		return
	}
	f.details = append(f.details, d)
	if int64(len(f.details)) > int64(detailMax) {
		f.details = nil
		f.detailsDropped = true
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
func (f *rollupFrame) absorb(child *rollupFrame, detailMax int) {
	f.coveredFiles += child.coveredFiles
	f.coveredBytes += child.coveredBytes
	f.uncoveredFiles += child.uncoveredFiles
	f.uncoveredBytes += child.uncoveredBytes
	f.noHashFiles += child.noHashFiles
	f.noHashBytes += child.noHashBytes

	// Only a child that actually contributed a not-covered/no-hash file can
	// change f's details verdict - a fully-covered child leaves it untouched
	// either way, so this must not fall through to an unconditional drop.
	if child.uncoveredFiles+child.noHashFiles > 0 {
		if f.detailsDropped || child.detailsDropped {
			f.details = nil
			f.detailsDropped = true
		} else {
			for _, d := range child.details {
				f.addDetail(d, detailMax)
			}
		}
	}

	if child.isFullyCovered() {
		f.boringCoveredDirs++
		f.boringCoveredFiles += child.coveredFiles
		f.boringCoveredBytes += child.coveredBytes
		return
	}
	f.notableLines = append(f.notableLines, child.render(detailMax)...)
}

// render decides this frame's own verdict and returns the line(s) it
// contributes to its parent: one line if homogeneous (fully covered, or
// fully not covered), its own summary plus buffered file paths if mixed but
// small enough (RollupDetailMax), or its summary plus its children's own
// decisions otherwise.
func (f *rollupFrame) render(detailMax int) []string {
	total := f.coveredFiles + f.uncoveredFiles + f.noHashFiles
	if total == 0 {
		return nil
	}

	label := f.dir + "/"

	if f.isFullyCovered() {
		return []string{fmt.Sprintf("  covered: %-12s %10s  %s  [fully covered]",
			util.Comma(f.coveredFiles), util.FormatBytes(f.coveredBytes), label)}
	}

	badCount := f.uncoveredFiles + f.noHashFiles
	if !f.detailsDropped && badCount > 0 && badCount <= int64(detailMax) {
		tag := "[mixed]"
		if f.isFullyNotCovered() {
			tag = "[NOT covered]"
		}
		header := fmt.Sprintf("  covered: %-8s not covered: %-8s",
			util.Comma(f.coveredFiles), util.Comma(f.uncoveredFiles))
		if f.noHashFiles > 0 {
			header += fmt.Sprintf(" no-hash: %-8s", util.Comma(f.noHashFiles))
		}
		header += fmt.Sprintf(" %10s  %s  %s",
			util.FormatBytes(f.coveredBytes+f.uncoveredBytes+f.noHashBytes), label, tag)

		lines := []string{header}
		for _, d := range f.details {
			dtag := "not covered"
			if d.status == models.CoverageNoHash {
				dtag = "no hash"
			}
			lines = append(lines, fmt.Sprintf("      %10s  %s  [%s]", util.FormatBytes(d.size), d.path, dtag))
		}
		return lines
	}

	if f.isFullyNotCovered() {
		detail := plural(f.uncoveredFiles, "file")
		if f.noHashFiles > 0 {
			detail += fmt.Sprintf(" + %s with no hash", util.Comma(f.noHashFiles))
		}
		return []string{fmt.Sprintf("  NOT covered: %-8s %10s  %s  [NOT covered]",
			detail, util.FormatBytes(f.uncoveredBytes+f.noHashBytes), label)}
	}

	// Mixed, and too big for the detail listing above: this frame's own
	// summary, then its children's own decisions, indented one level deeper.
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
// entirely one verdict into a single line instead of drilling into it, and -
// when a mixed subtree's not-covered-or-no-hash count is small enough
// (RollupDetailMax) - stopping the recursion early to list the actual file
// paths instead of drilling through however many ancestor directories stand
// between here and them. A parent frame that already qualifies for this
// always wins over a descendant that also would: rendering happens bottom-up,
// but a frame that takes this branch (or the fully-covered one) never reads
// notableLines, so a child's already-built detail listing is simply discarded
// once an ancestor decides to show its own instead - no special-casing needed
// beyond render() itself deciding independently at every level.
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
			stack[len(stack)-1].absorb(top, cfg.RollupDetailMax)
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
			stack[len(stack)-1].add(status, f.Path, f.SizeBytes, cfg.RollupDetailMax)

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
		for _, l := range root.render(cfg.RollupDetailMax) {
			fmt.Println(l)
		}
	}

	printCoverageSummary(res, cfg)
	return res, nil
}

// commonHash picks an algorithm that every snapshot involved actually carries.
//
// A keeper with no hash data at all - recorded.Hashes empty, whether because it
// has zero files (a canmount=off ZFS container dataset, which zfs-scan records)
// or because it was scanned --no-hash - can never match anything by content
// regardless of which algorithm is negotiated, so it must not be able to veto
// one the rest of the keepers share. Skipping it here changes nothing about
// what it can contribute: it already could never produce a covered match, on
// any algorithm. See knowledge/known-issues.md 2.9 - confirmed sweeping a real
// fleet, where empty container-dataset keepers made every candidate fail
// negotiation in seconds. A keeper with *some* hash data but not the one being
// tried still correctly vetoes that one; only total absence is exempted.
func commonHash(candidate *models.Snapshot, keepers []*models.Snapshot) (string, error) {
	for _, want := range []string{"sha1", "md5"} {
		if !hasHash(candidate, want) {
			continue
		}
		ok := true
		for _, k := range keepers {
			if len(k.Hashes) == 0 {
				continue
			}
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
