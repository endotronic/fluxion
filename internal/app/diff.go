package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fluxion/internal/diff"
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"
	"fluxion/internal/util"

	"github.com/schollz/progressbar/v3"
	"github.com/sirupsen/logrus"
)

type DiffConfig struct {
	DBPath string
	// OldQueries and NewQueries each name one or more snapshots. A single
	// entry is the plain case this has always supported. Several are combined
	// into one virtual union for that side - see multiSnapshotIter - which
	// only works when that side's snapshots have pairwise disjoint roots;
	// anything else is refused rather than guessed at.
	OldQueries []string
	NewQueries []string
	UpdateMode bool

	// MaxLinesPerDir caps how many lines one directory may contribute; 0 shows
	// everything. See diff.DefaultMaxLinesPerDir.
	MaxLinesPerDir int
	Excludes       []string
	NoCopies       bool
	NoMoves        bool
	ShowUnchanged  bool

	// Engine selects the diff implementation; see diff.Engine. The zero value
	// streams when it can and builds the tree when it cannot.
	Engine diff.Engine

	// TempDir is where the streaming engine's move/copy matching puts its
	// intermediates once they outgrow memory; empty means the system default.
	TempDir string
}

func RunDiff(cfg DiffConfig) error {
	if cfg.DBPath == "" {
		return fmt.Errorf("DB path is required")
	}

	// Open DB
	var dbStore store.Store
	var err error
	dbStore, err = sqlite.NewSqliteStore(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("error opening DB: %w", err)
	}
	defer dbStore.Close()

	// 1. Find Snapshots
	if len(cfg.OldQueries) == 0 || len(cfg.NewQueries) == 0 {
		return fmt.Errorf("at least one 'old' and one 'new' snapshot are required")
	}

	snapsA, err := resolveSnapshots(dbStore, cfg.OldQueries)
	if err != nil {
		return fmt.Errorf("could not resolve 'old' snapshot(s): %w", err)
	}
	snapsB, err := resolveSnapshots(dbStore, cfg.NewQueries)
	if err != nil {
		return fmt.Errorf("could not resolve 'new' snapshot(s): %w", err)
	}

	if len(snapsA) > 1 {
		logrus.Infof("Combining %d snapshots for the 'old' side: %s", len(snapsA), snapshotNames(snapsA))
	}
	if len(snapsB) > 1 {
		logrus.Infof("Combining %d snapshots for the 'new' side: %s", len(snapsB), snapshotNames(snapsB))
	}

	// Determine Hash Strategy
	hasSHA1A, hasMD5A := sideHashes(snapsA)
	hasSHA1B, hasMD5B := sideHashes(snapsB)

	commonSHA1 := hasSHA1A && hasSHA1B
	commonMD5 := hasMD5A && hasMD5B

	var strategy string
	if commonSHA1 {
		strategy = "sha1"
	} else if commonMD5 {
		strategy = "md5"
	} else {
		logrus.Errorf("Error: Incompatible hash types.\n")
		logrus.Errorf("'old' side hashes: sha1=%v md5=%v\n", hasSHA1A, hasMD5A)
		logrus.Errorf("'new' side hashes: sha1=%v md5=%v\n", hasSHA1B, hasMD5B)
		return fmt.Errorf("snapshots must share at least one common hash algorithm")
	}

	logrus.Infof("Comparing using strategy: %s", strings.ToUpper(strategy))

	// 2. Compare (Streaming)
	logrus.Info("Computing Diff...")

	// Setup Progress Bar
	countA := sumFileCounts(dbStore, snapsA)
	countB := sumFileCounts(dbStore, snapsB)
	totalExpected := countA + countB

	// The streaming engine needs DFS-key order, which costs a temp b-tree sort;
	// there is no reason to pay for it when the tree engine is going to run
	// anyway. Since Phase 3 that is the only reason left - streaming answers
	// move/copy detection too. Getting this wrong is not a correctness problem -
	// CompareSnapshots detects unusable ordering and falls back - only a wasted
	// sort or a missed opportunity to stream.
	streamable := cfg.Engine != diff.EngineTree

	// Advise on temp space before starting rather than after an hour of work.
	// This only advises: the hard stop lives in the engine, which statfs's as it
	// writes and refuses while there is still room to refuse in. An up-front
	// estimate has to be pessimistic enough that gating on it would turn away
	// runs that would have fitted.
	if streamable && !(cfg.NoMoves && cfg.NoCopies) {
		reportTempSpace(cfg.TempDir, totalExpected)
	}

	barDiff := progressbar.Default(totalExpected)

	iterA, rootA := buildSideIter(dbStore, snapsA, streamable, cfg.Excludes)
	iterB, rootB := buildSideIter(dbStore, snapsB, streamable, cfg.Excludes)

	results, err := diff.CompareSnapshots(
		iterA,
		iterB,
		diff.Options{
			RootA:          rootA,
			RootB:          rootB,
			HashType:       strategy,
			NoCopies:       cfg.NoCopies,
			NoMoves:        cfg.NoMoves,
			ShowUnchanged:  cfg.ShowUnchanged,
			MaxLinesPerDir: cfg.MaxLinesPerDir,
			TempDir:        cfg.TempDir,
			Engine:         cfg.Engine,
			OnProgress: func(curr int) {
				barDiff.Set(curr)
			},
		},
	)
	if err != nil {
		return fmt.Errorf("error during diff: %w", err)
	}
	logrus.Println()

	// 4. Print Results
	if len(results) == 0 {
		fmt.Println("No differences found.")
		return nil
	}

	for _, res := range results {
		// Update Mode Filtering
		if cfg.UpdateMode {
			// Ignore Added (Not in A)
			if res.Status == diff.StatusAdded {
				continue
			}
			// Ignore Move/Copy (Content exists in B)
			if res.Status == diff.StatusMove || res.Status == diff.StatusCopy {
				continue
			}
			// Show: StatusRemoved (Missing in B), StatusModified (Changed in B)
		}

		// A truncation summary stands in for lines that were not printed, so it
		// survives update-mode filtering: what it hides may well be exactly what
		// update mode is looking for.
		if res.Status == diff.StatusTruncated {
			where := res.RelPath
			if where == "." || where == "" {
				where = fmtRootPath(res.Root)
			}
			fmt.Printf("... %d more under %s%s\n", res.HiddenCount, where, countSummary(res))
			continue
		}

		symbol := "?"
		switch res.Status {
		case diff.StatusAdded:
			symbol = "[+]"
		case diff.StatusRemoved:
			symbol = "[-]"
		case diff.StatusModified:
			symbol = "[M]"
		case diff.StatusMove:
			symbol = "[>]"
		case diff.StatusCopy:
			symbol = "[C]"
		case diff.StatusMixed:
			symbol = "   " // Context line
		}

		fmtRoot := func(r string) string {
			if r != "" && !strings.HasSuffix(r, string(filepath.Separator)) {
				return r + string(filepath.Separator)
			}
			return r
		}

		displayPath := res.RelPath
		if res.Status == diff.StatusMove || res.Status == diff.StatusCopy {
			// [C] [/root/A/ -> /root/B/] relativeA/foo -> relativeB/foo
			displayPath = fmt.Sprintf("%s -> %s", res.SourceRelPath, res.RelPath)
		}

		// Add trailing slash for directories if not present
		// This applies to both StatusMixed context lines and normal directory changes
		// Since mixed nodes are directories by definition logic (containing children), we ensure trailing slash.
		// DiffResult.Path usually has trailing slash for dirs if from core logic.
		// But let's be safe for display consistency.
		if res.Status == diff.StatusMixed && !strings.HasSuffix(displayPath, string(filepath.Separator)) {
			// Wait, displayPath ends with res.RelPath.
			// If RelPath doesn't have it, we might want to add it.
		}

		// Construct summary
		var parts []string
		if res.AddedCount > 0 {
			parts = append(parts, fmt.Sprintf("%d added", res.AddedCount))
		}
		if res.RemovedCount > 0 {
			parts = append(parts, fmt.Sprintf("%d removed", res.RemovedCount))
		}
		if res.ModifiedCount > 0 {
			parts = append(parts, fmt.Sprintf("%d modified", res.ModifiedCount))
		}
		if res.CopyCount > 0 {
			parts = append(parts, fmt.Sprintf("%d copied", res.CopyCount))
		}
		if res.MoveCount > 0 {
			parts = append(parts, fmt.Sprintf("%d moved", res.MoveCount))
		}
		if res.UnchangedDirCount > 0 || res.UnchangedFileCount > 0 {
			subParts := []string{}
			if res.UnchangedDirCount > 0 {
				subParts = append(subParts, fmt.Sprintf("%d directories", res.UnchangedDirCount))
			}
			if res.UnchangedFileCount > 0 {
				subParts = append(subParts, fmt.Sprintf("%d files", res.UnchangedFileCount))
			}
			parts = append(parts, fmt.Sprintf("%s unchanged", strings.Join(subParts, ", ")))
			displayPath = fmt.Sprintf("%s (%s)", displayPath, strings.Join(parts, ", "))
		} else if len(parts) > 0 {
			if len(parts) > 1 {
				// Mixed changes (Added + Removed, etc). Roots might differ. Omit context.
				displayPath = fmt.Sprintf("%s (%s)", displayPath, strings.Join(parts, ", "))
			} else {
				displayPath = fmt.Sprintf("%s (%s in %s)", displayPath, strings.Join(parts, ", "), fmtRoot(res.Root))
			}
		}

		fmt.Printf("%s %s\n", symbol, displayPath)
	}
	return nil
}

func fmtRootPath(r string) string {
	if r != "" && !strings.HasSuffix(r, string(filepath.Separator)) {
		return r + string(filepath.Separator)
	}
	return r
}

// countSummary renders the change counts a line carries, in parentheses, or "".
func countSummary(res diff.DiffResult) string {
	var parts []string
	if res.AddedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d added", res.AddedCount))
	}
	if res.RemovedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d removed", res.RemovedCount))
	}
	if res.ModifiedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d modified", res.ModifiedCount))
	}
	if res.CopyCount > 0 {
		parts = append(parts, fmt.Sprintf("%d copied", res.CopyCount))
	}
	if res.MoveCount > 0 {
		parts = append(parts, fmt.Sprintf("%d moved", res.MoveCount))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func isExcluded(path, root string, excludes []string) bool {
	if len(excludes) == 0 {
		return false
	}
	// Normalize path
	path = filepath.Clean(path)

	for _, excl := range excludes {
		if strings.TrimSpace(excl) == "" {
			// An empty exclude joined with the root is the root, which would
			// exclude the entire snapshot and report a diff of nothing. Same
			// severity-1 direction as over-excluding, so it matches nothing.
			continue
		}

		// Every comparison here goes through pathHasPrefix rather than
		// strings.HasPrefix: an exclude must match at a path boundary or not at
		// all. See the comment there for why over-excluding is a severity-1 bug
		// and not a cosmetic one.
		if filepath.IsAbs(excl) {
			// 1. Absolute exclude: matches the path directly.
			if pathHasPrefix(path, excl) {
				return true
			}
		} else {
			// 2. Relative exclude, anchored at the snapshot root when we have
			// one: "node_modules" means "$ROOT/node_modules".
			if root != "" {
				if pathHasPrefix(path, filepath.Join(root, excl)) {
					return true
				}
			}

			// 3. Or the path is itself relative, as it is on the second call in
			// createIter: "node_modules" matches "node_modules/foo".
			if pathHasPrefix(path, excl) {
				return true
			}

			// 4. Also check for path component match? (e.g. "foo/node_modules/bar")
			// Requirement says: "If relative, it is applied to each snapshot from the snapshot's root path."
			// So "node_modules" means "$ROOT/node_modules". It does NOT mean "anywhere/node_modules".
		}
	}
	return false
}

// assumedAvgPathLen is what the temp-space estimate charges per path when it has
// no cheap way to measure. Deliberately generous: fleet paths
// (knowledge/fleet.md) run to well over a hundred bytes, and the estimate's only
// job is to not be optimistic.
const assumedAvgPathLen = 120

// tempSpaceWorthMentioning is the estimate below which none of this is worth a
// line of output: the intermediates for a diff that small never leave memory at
// all, so free space and filesystem type are both irrelevant. A var so a test
// can reach the advisory without a million-file snapshot.
var tempSpaceWorthMentioning int64 = 256 << 20

// reportTempSpace tells the user what the diff's intermediates may cost and
// whether the temp directory can take it, before the run rather than after.
//
// Two things are worth saying out loud. Free space, because the fleet's pools
// sit at 94-96% and tens of gigabytes of intermediates are not free. And the
// filesystem type, because on most Linux systems the default temp directory is a
// tmpfs - which is RAM, so a diff that spills there is not spilling at all, and
// the memory bound the streaming engine exists to provide quietly stops holding.
func reportTempSpace(tempDir string, nodes int64) {
	where := tempDir
	if where == "" {
		where = os.TempDir()
	}

	need := diff.EstimateTempBytes(nodes, assumedAvgPathLen)
	if need < tempSpaceWorthMentioning {
		// A diff of a few thousand files never leaves memory. Saying anything
		// about temp space here would be noise on every ordinary run.
		return
	}

	if fsType, err := util.FSTypeAt(where); err == nil && util.IsMemoryBackedFS(fsType) {
		logrus.Warnf("Temp directory %s is a %s, which is memory - the diff's intermediates "+
			"will not leave RAM. Pass --temp-dir to put them on real storage.", where, fsType)
	}

	free, err := util.GetFSAvail(where)
	if err != nil {
		logrus.Debugf("Could not check free space on %s: %v", where, err)
		return
	}
	logrus.Infof("Temp space: %s free at %s; this diff may use up to %s (typically far less)",
		util.FormatBytes(int64(free)), where, util.FormatBytes(need))
	if int64(free) < need {
		logrus.Warnf("That may not be enough. The diff will stop rather than fill %s; "+
			"pass --temp-dir to use a filesystem with more room.", where)
	}
}
