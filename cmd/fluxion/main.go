package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"fluxion/internal/app"
	"fluxion/internal/consts"
	"fluxion/internal/diff"
	"fluxion/internal/util"

	"github.com/sirupsen/logrus"
)

func init() {
	logrus.SetOutput(os.Stderr)
	logrus.SetFormatter(&logrus.TextFormatter{
		DisableTimestamp: true,
	})
	logrus.SetLevel(logrus.InfoLevel)
}

func main() {
	// Architecture Note:
	// This main function acts as a thin wrapper (CLI Layer).
	// It handles argument parsing using the `flag` package and delegates logical execution
	// to the `internal/app` package. Each subcommand has a corresponding `Run<Command>` function
	// in `internal/app` that accepts a configuration struct.

	// Parse subcommand
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "snapshot", "snap", "scan", "s":
		runSnapshot(os.Args[2:])
	case "list", "l":
		runList(os.Args[2:])
	case "delete", "x":
		runDelete(os.Args[2:])
	case "diff", "d":
		runDiff(os.Args[2:])
	case "import", "i":
		runImportDB(os.Args[2:])
	case "import-legacy":
		runImportLegacy(os.Args[2:])
	case "export-legacy":
		runExportLegacy(os.Args[2:])
	case "dupes", "z":
		runDupes(os.Args[2:])
	case "merge", "m":
		runMerge(os.Args[2:])
	case "version", "v":
		runVersion()
	case "size":
		runSize(os.Args[2:])
	case "find":
		runFind(os.Args[2:])
	case "coverage", "c":
		runCoverage(os.Args[2:])
	default:
		fmt.Printf("Unknown subcommand: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Usage: fluxion <subcommand> [flags]")
	fmt.Println("Subcommands:")
	fmt.Println("  snapshot (s)    Scan a directory")
	fmt.Println("  list (l)        List snapshots")
	fmt.Println("  diff (d)        Compare snapshots")
	fmt.Println("  delete (x)      Delete a snapshot")
	fmt.Println("  import (i)      Import from another DB")
	fmt.Println("  import-legacy   Import legacy format")
	fmt.Println("  export-legacy   Export to legacy format")
	fmt.Println("  dupes (z)       Find duplicates within a snapshot")
	fmt.Println("  merge (m)       Merge multiple snapshots into one")
	fmt.Println("  size            Report size of a snapshot")
	fmt.Println("  find            Find files in a snapshot by name pattern")
	fmt.Println("  coverage (c)    Check every file of one snapshot exists, by content, in others")
	fmt.Println("  version (v)     Print version")
}

func runVersion() {
	fmt.Printf("fluxion version %s\n", consts.Version)
}

func runSnapshot(args []string) {
	cmd := flag.NewFlagSet("snapshot", flag.ExitOnError)
	dirPtr := cmd.String("dir", "", "Directory to scan (optional if provided as argument)")
	dbPtr := cmd.String("db", "", "Path to sqlite DB (optional, defaults to <dirname>.db)")
	namePtr := cmd.String("name", "", "Name for the snapshot (optional, defaults to <dirname>_<date>)")
	threadsPtr := cmd.Int("threads", runtime.NumCPU(), "Number of threads")
	forceNewPtr := cmd.Bool("new", false, "Force new scan (ignore previous)")
	resumePtr := cmd.String("resume", "", "Name or ID of snapshot to resume (explicit)")
	hostnamePtr := cmd.String("hostname", "", "Computer name (defaults to os.Hostname())")

	crossMountsPtr := cmd.Bool("cross-mounts", true, "Traverse mount points")
	failOnMountPtr := cmd.Bool("fail-on-mount", false, "Fail if mount point encountered")
	md5Ptr := cmd.Bool("md5", false, "Compute MD5 checksums")
	skipEstPtr := cmd.Bool("skip-estimation", false, "Skip filesystem usage estimation")
	estimateOnlyPtr := cmd.Bool("estimate", false, "Estimate scan size only (don't scan)")

	cmd.Parse(args)

	// Check for positional argument if flag is empty
	targetArg := *dirPtr
	if targetArg == "" {
		if cmd.NArg() > 0 {
			targetArg = cmd.Arg(0)
		}
	}

	if targetArg == "" {
		fmt.Println("Error: Directory required (via argument or --dir)")
		cmd.Usage()
		os.Exit(1)
	}

	// Resolve potential mount point source
	resolvedPath, err := util.ResolveMountPoint(targetArg)
	if err != nil {
		fmt.Printf("Error resolving target '%s': %v\n", targetArg, err)
		os.Exit(1)
	}

	absTarget, _ := filepath.Abs(targetArg)
	if resolvedPath != targetArg && resolvedPath != absTarget {
		logrus.Infof("Resolved mount source '%s' to path '%s'", targetArg, resolvedPath)
	}

	cfg := app.SnapshotConfig{
		TargetDir:      resolvedPath,
		DBPath:         *dbPtr,
		Name:           *namePtr,
		Threads:        *threadsPtr,
		ForceNew:       *forceNewPtr,
		ResumeFrom:     *resumePtr,
		Hostname:       *hostnamePtr,
		CrossMounts:    *crossMountsPtr,
		FailOnMount:    *failOnMountPtr,
		ComputeMD5:     *md5Ptr,
		SkipEstimation: *skipEstPtr,
		EstimateOnly:   *estimateOnlyPtr,
	}

	if err := app.RunSnapshot(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runList(args []string) {
	cmd := flag.NewFlagSet("list", flag.ExitOnError)
	dbPtr := cmd.String("db", "", "Path to sqlite DB (optional, defaults to <current_dir>.db if found)")

	cmd.Parse(args)

	dbPath := *dbPtr
	if dbPath == "" {
		matches, _ := filepath.Glob("*.db")
		if len(matches) == 1 {
			dbPath = matches[0]
		}
	}

	if dbPath == "" {
		fmt.Println("Error: --db required (or single .db file in current dir)")
		cmd.Usage()
		os.Exit(1)
	}

	cfg := app.ListConfig{
		DBPath: dbPath,
	}

	if err := app.RunList(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runDelete(args []string) {
	cmd := flag.NewFlagSet("delete", flag.ExitOnError)
	dbPtr := cmd.String("db", "", "Path to sqlite DB (required)")
	var yes bool
	cmd.BoolVar(&yes, "yes", false, "Skip confirmation prompt")
	cmd.BoolVar(&yes, "y", false, "Skip confirmation prompt (shorthand)")

	cmd.Parse(args)

	if *dbPtr == "" {
		fmt.Println("Error: --db is required")
		cmd.Usage()
		os.Exit(1)
	}

	if cmd.NArg() < 1 {
		fmt.Println("Usage: fluxion delete --db <db> <snapshot_id_or_name> [-y]")
		os.Exit(1)
	}

	cfg := app.DeleteConfig{
		DBPath:    *dbPtr,
		SnapQuery: cmd.Arg(0),
		Yes:       yes,
	}

	if err := app.RunDelete(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runDiff(args []string) {
	cmd := flag.NewFlagSet("diff", flag.ExitOnError)
	dbPtr := cmd.String("db", "", "Path to sqlite DB (required)")
	var updateMode bool
	cmd.BoolVar(&updateMode, "update", false, "Report only files from Source (A) missing or modified in Target (B)")
	cmd.BoolVar(&updateMode, "u", false, "Report only files from Source (A) missing or modified in Target (B) (shorthand)")

	var excludes arrayFlags
	cmd.Var(&excludes, "exclude", "Exclude directory from diff (relative or absolute)")
	cmd.Var(&excludes, "e", "Exclude directory from diff (shorthand)")

	var noCopies bool
	cmd.BoolVar(&noCopies, "no-copies", false, "Do not detect copies")

	var noMoves bool
	cmd.BoolVar(&noMoves, "no-moves", false, "Do not detect moves")

	var showUnchanged bool
	cmd.BoolVar(&showUnchanged, "show-unchanged", false, "Show unchanged counts for parent directories of changes")

	maxLines := cmd.Int("max-lines", diff.DefaultMaxLinesPerDir,
		"Maximum lines one directory may print before the rest is summarised as \"... N more\" (0 = no limit)")

	cmd.Parse(args)

	if *dbPtr == "" {
		fmt.Println("Error: --db is required for diff")
		cmd.Usage()
		os.Exit(1)
	}

	tail := cmd.Args()
	if len(tail) != 2 {
		fmt.Println("Usage: fluxion diff --db <db> <old_snapshot_id> <new_snapshot_id>")
		os.Exit(1)
	}

	cfg := app.DiffConfig{
		DBPath:        *dbPtr,
		OldQuery:      tail[0],
		NewQuery:      tail[1],
		UpdateMode:    updateMode,
		Excludes:      excludes,
		NoCopies:      noCopies,
		NoMoves:       noMoves,
		ShowUnchanged: showUnchanged,

		MaxLinesPerDir: *maxLines,
	}

	if err := app.RunDiff(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// arrayFlags to support multiple --exclude options
type arrayFlags []string

func (i *arrayFlags) String() string {
	return fmt.Sprint(*i)
}

func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

func runImportLegacy(args []string) {
	cmd := flag.NewFlagSet("import-legacy", flag.ExitOnError)
	sizesPtr := cmd.String("sizes", "", "Path to sizes file (optional, defaults to inferred from hashes)")
	dbPtr := cmd.String("db", "", "Path to sqlite DB (required)")
	namePtr := cmd.String("name", "", "Name for the snapshot (optional, defaults to filename)")
	rootPtr := cmd.String("root", "/", "Root path for the snapshot (defaults to /)")

	cmd.Parse(args)

	if *dbPtr == "" {
		fmt.Println("Error: --db is required.")
		cmd.Usage()
		os.Exit(1)
	}

	hashesPath := ""
	if cmd.NArg() > 0 {
		hashesPath = cmd.Arg(0)
	}

	if hashesPath == "" {
		fmt.Println("Error: hashes file path is required as positional argument.")
		cmd.Usage()
		os.Exit(1)
	}

	cfg := app.ImportLegacyConfig{
		HashesPath: hashesPath,
		SizesPath:  *sizesPtr,
		DBPath:     *dbPtr,
		Name:       *namePtr,
		RootPath:   *rootPtr,
	}

	if err := app.RunImportLegacy(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runExportLegacy(args []string) {
	cmd := flag.NewFlagSet("export-legacy", flag.ExitOnError)
	dbPtr := cmd.String("db", "", "Path to sqlite DB (required)")
	exportNamePtr := cmd.String("export-name", "", "Name for the export (optional, defaults to snapshot name)")

	cmd.Parse(args)

	if *dbPtr == "" {
		fmt.Println("Error: --db is required.")
		cmd.Usage()
		os.Exit(1)
	}

	if cmd.NArg() < 1 {
		fmt.Println("Error: snapshot ID/name to export is required as positional argument.")
		cmd.Usage()
		os.Exit(1)
	}

	cfg := app.ExportLegacyConfig{
		DBPath:     *dbPtr,
		SnapQuery:  cmd.Arg(0),
		ExportName: *exportNamePtr,
	}

	if err := app.RunExportLegacy(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runImportDB(args []string) {
	cmd := flag.NewFlagSet("import", flag.ExitOnError)
	destDBPtr := cmd.String("db", "", "Path to destination sqlite DB (required)")
	sourceDBPtr := cmd.String("source", "", "Path to source sqlite DB (required)")
	allPtr := cmd.Bool("all", false, "Import all snapshots from source DB")

	cmd.Parse(args)

	if *destDBPtr == "" || *sourceDBPtr == "" {
		fmt.Println("Error: --db and --source are required")
		cmd.Usage()
		os.Exit(1)
	}

	if !*allPtr && cmd.NArg() < 1 {
		fmt.Println("Error: Must specify snapshot ID/name to import, or use --all")
		cmd.Usage()
		os.Exit(1)
	}

	snapQuery := ""
	if !*allPtr {
		snapQuery = cmd.Arg(0)
	}

	cfg := app.ImportDBConfig{
		SourceDBPath:  *sourceDBPtr,
		DestDBPath:    *destDBPtr,
		ImportAll:     *allPtr,
		SnapshotQuery: snapQuery,
	}

	if err := app.RunImportDB(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runDupes(args []string) {
	cmd := flag.NewFlagSet("dupes", flag.ExitOnError)
	dbPtr := cmd.String("db", "", "Path to sqlite DB (required)")
	minSizePtr := cmd.String("min-size", "1MB", "Minimum size (e.g., 10M, 1G) (default 1MB)")
	limitPtr := cmd.Int("limit", 0, "Limit number of results (0 for all)")
	cmd.IntVar(limitPtr, "k", 0, "Limit number of results (shorthand)")

	cmd.Parse(args)

	if *dbPtr == "" {
		fmt.Println("Error: --db is required")
		cmd.Usage()
		os.Exit(1)
	}

	if cmd.NArg() < 1 {
		fmt.Println("Usage: fluxion dupes --db <db> <snapshot_id_or_name>")
		os.Exit(1)
	}

	minSize, err := util.ParseSize(*minSizePtr)
	if err != nil {
		fmt.Printf("Error parsing min-size: %v\n", err)
		os.Exit(1)
	}

	cfg := app.DupesConfig{
		DBPath:    *dbPtr,
		SnapQuery: cmd.Arg(0),
		MinSize:   minSize,
		Limit:     *limitPtr,
	}

	if err := app.RunDupes(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runMerge(args []string) {
	cmd := flag.NewFlagSet("merge", flag.ExitOnError)
	dbPtr := cmd.String("db", "", "Path to sqlite DB (required)")
	namePtr := cmd.String("name", "", "Name for the new merged snapshot (required)")
	hostnamePtr := cmd.String("hostname", "", "Computer name for the snapshot (defaults to current hostname)")

	cmd.Parse(args)

	if *dbPtr == "" {
		fmt.Println("Error: --db is required")
		cmd.Usage()
		os.Exit(1)
	}

	if *namePtr == "" {
		fmt.Println("Error: --name is required for the new snapshot")
		cmd.Usage()
		os.Exit(1)
	}

	if cmd.NArg() < 2 {
		fmt.Println("Usage: fluxion merge --db <db> --name <new_name> [--hostname <name>] <snap1> <snap2> ...")
		os.Exit(1)
	}

	cfg := app.MergeConfig{
		DBPath:    *dbPtr,
		Name:      *namePtr,
		Hostname:  *hostnamePtr,
		Snapshots: cmd.Args(),
	}

	if err := app.RunMerge(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runSize(args []string) {
	cmd := flag.NewFlagSet("size", flag.ExitOnError)
	dbPtr := cmd.String("db", "", "Path to sqlite DB (required)")
	totalBytesPtr := cmd.Bool("total-bytes", false, "Show total bytes without formatting")

	cmd.Parse(args)

	if *dbPtr == "" {
		fmt.Println("Error: --db is required")
		cmd.Usage()
		os.Exit(1)
	}

	if cmd.NArg() < 1 {
		fmt.Println("Usage: fluxion size --db <db> <snapshot_id_or_name> [--total-bytes]")
		os.Exit(1)
	}

	cfg := app.SizeConfig{
		DBPath:     *dbPtr,
		SnapQuery:  cmd.Arg(0),
		TotalBytes: *totalBytesPtr,
	}

	if err := app.RunSize(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runFind(args []string) {
	cmd := flag.NewFlagSet("find", flag.ExitOnError)
	dbPtr := cmd.String("db", "", "Path to sqlite DB (required)")

	var caseSensitive bool
	cmd.BoolVar(&caseSensitive, "case-sensitive", false, "Case sensitive matching")
	cmd.BoolVar(&caseSensitive, "s", false, "Case sensitive matching (shorthand)")

	var isRegex bool
	cmd.BoolVar(&isRegex, "regex", false, "Treat pattern as regular expression")
	cmd.BoolVar(&isRegex, "E", false, "Treat pattern as regular expression (shorthand)")

	cmd.Parse(args)

	if *dbPtr == "" {
		fmt.Println("Error: --db is required")
		cmd.Usage()
		os.Exit(1)
	}

	if cmd.NArg() < 2 {
		fmt.Println("Usage: fluxion find --db <db> [-s] [-E] <snapshot_id_or_name> <pattern>")
		os.Exit(1)
	}

	cfg := app.FindConfig{
		DBPath:        *dbPtr,
		SnapQuery:     cmd.Arg(0),
		Pattern:       cmd.Arg(1),
		CaseSensitive: caseSensitive,
		IsRegex:       isRegex,
	}

	if err := app.RunFind(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// runCoverage answers "is it safe to delete this?" without building a diff tree.
//
// Exit status is meaningful here in a way it is not for `diff`: 0 means fully
// covered, 2 means something would be lost. That makes it usable as a shell
// predicate in front of an actual destroy.
func runCoverage(args []string) {
	cmd := flag.NewFlagSet("coverage", flag.ExitOnError)
	dbPtr := cmd.String("db", "", "Path to sqlite DB (required)")
	minSizePtr := cmd.String("min-size", "0", "Skip files smaller than this (e.g. 1M); they are not checked and not counted as covered")
	limitPtr := cmd.Int("limit", app.DefaultCoverageLimit, "Maximum entries to list; totals are always complete (0 = list all)")
	byDirPtr := cmd.Bool("by-dir", false, "List one line per containing directory instead of one per file")

	var excludes arrayFlags
	cmd.Var(&excludes, "exclude", "Path to exclude (can be repeated)")

	cmd.Parse(args)

	if *dbPtr == "" {
		fmt.Println("Error: --db is required")
		cmd.Usage()
		os.Exit(1)
	}
	if cmd.NArg() < 2 {
		fmt.Println("Usage: fluxion coverage --db <db> <snapshot-to-delete> <snapshot-to-keep> [more-to-keep...]")
		os.Exit(1)
	}

	minSize, err := util.ParseSize(*minSizePtr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid --min-size: %v\n", err)
		os.Exit(1)
	}

	res, err := app.RunCoverage(app.CoverageConfig{
		DBPath:         *dbPtr,
		CandidateQuery: cmd.Arg(0),
		KeeperQueries:  cmd.Args()[1:],
		MinSize:        minSize,
		Limit:          *limitPtr,
		ByDir:          *byDirPtr,
		Excludes:       excludes,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if !res.Covered() {
		os.Exit(2)
	}
}
