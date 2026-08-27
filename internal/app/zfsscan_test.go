package app

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"fluxion/internal/models"
	"fluxion/internal/store/sqlite"
	"fluxion/internal/zfsutil"
)

// TestMain shrinks unmountWithRetry's delay for the whole package's test run
// so a test that deliberately always fails unmount (e.g.
// TestCleanupMounts_UnmountFailureLeavesDirBehind) doesn't actually spend
// several real seconds asleep.
func TestMain(m *testing.M) {
	unmountRetryDelay = time.Millisecond
	os.Exit(m.Run())
}

// fakeZFS backs a small in-memory dataset table with a Runner, so
// RunZFSScan's orchestration can be tested without a real `zfs` binary or
// root (there is neither in this project's build environment).
type fakeZFS struct {
	rows         [][6]string // name, mountpoint, mounted, canmount, type, used
	failMountFor map[string]bool
	mountCalls   []string          // dataset names successfully mounted via MountAt
	unmountCalls []string          // paths successfully unmounted via UnmountPath
	mountAtPaths map[string]string // dataset -> temp path it was mounted at
	listCalls    int
}

func (f *fakeZFS) runner() zfsutil.Runner {
	return func(name string, args ...string) (string, error) {
		switch name {
		case "zfs":
			switch args[0] {
			case "list":
				f.listCalls++
				var out string
				for _, r := range f.rows {
					out += fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\n", r[0], r[1], r[2], r[3], r[4], r[5])
				}
				return out, nil
			default:
				return "", fmt.Errorf("unexpected zfs subcommand %q", args[0])
			}
		case "mount":
			// zfsutil.MountAt: `mount -t zfs -o zfsutil <dataset> <path>`.
			// zfs-scan uses this for every dataset it scans, mounted
			// elsewhere already or not.
			if len(args) != 6 || args[0] != "-t" || args[1] != "zfs" || args[2] != "-o" || args[3] != "zfsutil" {
				return "", fmt.Errorf("unexpected mount invocation: %v", args)
			}
			ds, path := args[4], args[5]
			if f.failMountFor[ds] {
				return "mount.zfs: permission denied", fmt.Errorf("exit status 1")
			}
			f.mountCalls = append(f.mountCalls, ds)
			if f.mountAtPaths == nil {
				f.mountAtPaths = map[string]string{}
			}
			f.mountAtPaths[ds] = path
			return "", nil
		case "umount":
			// zfsutil.UnmountPath: `umount <path>`, targeting the exact
			// mount instance zfs-scan created (not the dataset name, which
			// would be ambiguous if the dataset was already mounted
			// elsewhere too).
			if len(args) != 1 {
				return "", fmt.Errorf("unexpected umount invocation: %v", args)
			}
			f.unmountCalls = append(f.unmountCalls, args[0])
			return "", nil
		default:
			return "", fmt.Errorf("unexpected command %q", name)
		}
	}
}

func TestRunZFSScan_MountsScansAndUnmounts(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	fz := &fakeZFS{
		rows: [][6]string{
			{"pool", "/pool", "yes", "on", "filesystem", "0"},                        // already mounted elsewhere
			{"pool/needs_mount", "/pool/needs_mount", "no", "on", "filesystem", "0"}, // not currently mounted
			{"pool/container", "/pool/container", "no", "off", "filesystem", "0"},    // canmount=off
			{"pool/vol", "-", "no", "-", "volume", "0"},                              // zvol
			{"pool/mount_fails", "/some/path", "no", "on", "filesystem", "0"},        // our mount fails
			{"pool/legacy_unmounted", "legacy", "no", "on", "filesystem", "0"},       // no live legacy mount, mounted fresh anyway
			{"pool/no_mountpoint", "none", "no", "on", "filesystem", "0"},            // no mountpoint property, mounted fresh anyway
		},
		failMountFor: map[string]bool{"pool/mount_fails": true},
	}

	res, err := RunZFSScan(ZFSScanConfig{
		DBPath:  dbPath,
		Roots:   []string{"pool"},
		Threads: 1,
		Runner:  fz.runner(),
	})
	if err != nil {
		t.Fatalf("RunZFSScan: %v", err)
	}

	wantScanned := []string{"pool", "pool/legacy_unmounted", "pool/needs_mount", "pool/no_mountpoint"}
	sort.Strings(res.Scanned)
	if fmt.Sprint(res.Scanned) != fmt.Sprint(wantScanned) {
		t.Errorf("Scanned = %v, want %v", res.Scanned, wantScanned)
	}

	wantSkipped := []string{"pool/container", "pool/vol"}
	sort.Strings(res.Skipped)
	if fmt.Sprint(res.Skipped) != fmt.Sprint(wantSkipped) {
		t.Errorf("Skipped = %v, want %v", res.Skipped, wantSkipped)
	}

	if len(res.Failed) != 1 || res.Failed[0] != "pool/mount_fails" {
		t.Errorf("Failed = %v, want [pool/mount_fails]", res.Failed)
	}
	if !res.HadFailures {
		t.Error("HadFailures should be true when a mount fails")
	}

	// Every scanned dataset - including "pool", which was already mounted
	// at its usual location - gets its own fresh mount and unmount, since
	// zfs-scan always mounts a dedicated, isolated copy.
	wantMounted := []string{"pool", "pool/legacy_unmounted", "pool/needs_mount", "pool/no_mountpoint"}
	sort.Strings(fz.mountCalls)
	if fmt.Sprint(fz.mountCalls) != fmt.Sprint(wantMounted) {
		t.Errorf("mountCalls = %v, want %v", fz.mountCalls, wantMounted)
	}

	var wantUnmountPaths []string
	for _, ds := range wantMounted {
		path, ok := fz.mountAtPaths[ds]
		if !ok {
			t.Fatalf("expected %s to have been mounted via MountAt", ds)
		}
		wantUnmountPaths = append(wantUnmountPaths, path)
	}
	sort.Strings(wantUnmountPaths)
	gotUnmountPaths := append([]string(nil), fz.unmountCalls...)
	sort.Strings(gotUnmountPaths)
	if fmt.Sprint(gotUnmountPaths) != fmt.Sprint(wantUnmountPaths) {
		t.Errorf("unmountCalls = %v, want %v", gotUnmountPaths, wantUnmountPaths)
	}

	// Every temporary mountpoint should be cleaned up (os.Remove, not left
	// behind) once its dataset is unmounted.
	for _, ds := range wantMounted {
		dir := fz.mountAtPaths[ds]
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("expected temporary mount dir %s (%s) to be removed after unmount, stat err = %v", dir, ds, err)
		}
	}
}

func TestRunZFSScan_MountFailureIsAFailureNotASkip(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	// Even a dataset that's already mounted at its usual location still
	// gets a fresh mount attempt, which can still fail.
	fz := &fakeZFS{
		rows: [][6]string{
			{"pool/already_mounted", "/pool/already_mounted", "yes", "on", "filesystem", "0"},
		},
		failMountFor: map[string]bool{"pool/already_mounted": true},
	}

	res, err := RunZFSScan(ZFSScanConfig{DBPath: dbPath, Roots: []string{"pool"}, Threads: 1, Runner: fz.runner()})
	if err != nil {
		t.Fatalf("RunZFSScan: %v", err)
	}

	if len(res.Failed) != 1 || res.Failed[0] != "pool/already_mounted" {
		t.Errorf("Failed = %v, want [pool/already_mounted]", res.Failed)
	}
	if !res.HadFailures {
		t.Error("HadFailures should be true when a mount fails")
	}
	if len(res.Skipped) != 0 {
		t.Errorf("a mount failure is a failure, not a skip; Skipped = %v", res.Skipped)
	}
}

func TestRunZFSScan_IncludeCanMountOff(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	fz := &fakeZFS{
		rows: [][6]string{
			{"pool/container", "/pool/container", "no", "off", "filesystem", "0"},
		},
	}

	// Default: canmount=off is skipped, not mounted.
	res, err := RunZFSScan(ZFSScanConfig{DBPath: dbPath, Roots: []string{"pool"}, Threads: 1, Runner: fz.runner()})
	if err != nil {
		t.Fatalf("RunZFSScan: %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "pool/container" {
		t.Errorf("Skipped = %v, want [pool/container]", res.Skipped)
	}
	if len(res.Scanned) != 0 || len(fz.mountCalls) != 0 {
		t.Errorf("canmount=off should not be mounted/scanned by default; Scanned = %v, mountCalls = %v", res.Scanned, fz.mountCalls)
	}

	// IncludeCanMountOff: true - the same dataset is now mounted and scanned.
	res, err = RunZFSScan(ZFSScanConfig{DBPath: dbPath, Roots: []string{"pool"}, Threads: 1, Runner: fz.runner(), IncludeCanMountOff: true})
	if err != nil {
		t.Fatalf("RunZFSScan with IncludeCanMountOff: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("Skipped = %v, want none when IncludeCanMountOff is set", res.Skipped)
	}
	if len(res.Scanned) != 1 || res.Scanned[0] != "pool/container" {
		t.Errorf("Scanned = %v, want [pool/container]", res.Scanned)
	}
	if len(fz.mountCalls) != 1 || fz.mountCalls[0] != "pool/container" {
		t.Errorf("mountCalls = %v, want [pool/container]", fz.mountCalls)
	}
}

func TestRunZFSScan_RerunIsIdempotent(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	fz := &fakeZFS{
		rows: [][6]string{
			{"pool", "/pool", "yes", "on", "filesystem", "0"},
		},
	}

	if _, err := RunZFSScan(ZFSScanConfig{DBPath: dbPath, Roots: []string{"pool"}, Threads: 1, Runner: fz.runner()}); err != nil {
		t.Fatalf("first run: %v", err)
	}

	fz.mountCalls, fz.unmountCalls = nil, nil
	res, err := RunZFSScan(ZFSScanConfig{DBPath: dbPath, Roots: []string{"pool"}, Threads: 1, Runner: fz.runner()})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	if len(res.AlreadyDone) != 1 || res.AlreadyDone[0] != "pool" {
		t.Errorf("AlreadyDone = %v, want [pool]", res.AlreadyDone)
	}
	if len(res.Scanned) != 0 {
		t.Errorf("second run should not rescan, got Scanned = %v", res.Scanned)
	}
	if len(fz.mountCalls) != 0 || len(fz.unmountCalls) != 0 {
		t.Error("re-running an already-scanned dataset should mount/unmount nothing - the DB check happens before any mount attempt")
	}
	if res.HadFailures {
		t.Error("re-running a clean prior scan should not report failures")
	}
}

func TestRunZFSScan_DryRunTouchesNothing(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	fz := &fakeZFS{
		rows: [][6]string{
			{"pool", "/pool", "no", "on", "filesystem", "0"},
			{"pool/no_mountpoint", "none", "no", "on", "filesystem", "0"},
		},
	}

	res, err := RunZFSScan(ZFSScanConfig{DBPath: dbPath, Roots: []string{"pool"}, DryRun: true, Runner: fz.runner()})
	if err != nil {
		t.Fatalf("RunZFSScan dry-run: %v", err)
	}
	if len(fz.mountCalls) != 0 || len(fz.unmountCalls) != 0 {
		t.Error("dry-run must not mount or unmount")
	}
	if len(res.Scanned) != 0 {
		t.Error("dry-run must not scan")
	}

	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snaps, err := s.ListSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 0 {
		t.Errorf("dry-run must not write to the DB, found %d snapshots", len(snaps))
	}
}

// TestRunZFSScan_ResumesInterruptedInProgressSnapshot proves the fix for a bug
// where an interrupted zfs-scan dataset could never be retried: RunZFSScan
// now looks the stale in_progress row up by dataset name (not by this run's
// brand-new, always-different temp mount path) and passes it to RunSnapshot
// as ResumeFrom, so the scan resumes the existing snapshot row instead of
// colliding with it.
func TestRunZFSScan_ResumesInterruptedInProgressSnapshot(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a prior zfs-scan run that was killed mid-dataset: a snapshot
	// row already exists, named exactly after the dataset (zfs-scan's
	// naming convention), still in_progress, recorded against that earlier
	// run's own temp mount path - which no longer exists by the time this
	// run starts.
	oldSnap, err := s.CreateSnapshot("/tmp/fluxion-zfsscan-stale", "pool/interrupted", "host")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	fz := &fakeZFS{
		rows: [][6]string{
			{"pool/interrupted", "/pool/interrupted", "no", "on", "filesystem", "0"},
		},
	}

	res, err := RunZFSScan(ZFSScanConfig{DBPath: dbPath, Roots: []string{"pool"}, Threads: 1, Runner: fz.runner()})
	if err != nil {
		t.Fatalf("RunZFSScan: %v", err)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("an interrupted-then-rerun dataset should resume, not fail; Failed = %v", res.Failed)
	}
	if len(res.Scanned) != 1 || res.Scanned[0] != "pool/interrupted" {
		t.Errorf("Scanned = %v, want [pool/interrupted]", res.Scanned)
	}

	s, err = sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.FindSnapshot("pool/interrupted")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != oldSnap.ID {
		t.Errorf("expected the stale in_progress snapshot #%d to be resumed in place, got a different snapshot #%d instead", oldSnap.ID, got.ID)
	}
	if got.Status != models.StatusCompleted {
		t.Errorf("resumed snapshot status = %s, want completed", got.Status)
	}
}

// TestRunZFSScan_ForceNewIgnoresPriorState covers --new: a completed, a
// failed, and a still-in_progress prior snapshot for three different
// datasets should all be bypassed (mounted and scanned fresh) rather than
// skipped/blocked/silently resumed, and the old rows should survive as
// history under a second row with the same name.
func TestRunZFSScan_ForceNewIgnoresPriorState(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	completedOld, err := s.CreateSnapshot("/old/completed", "pool/completed", "host")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteSnapshot(completedOld.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	failedOld, err := s.CreateSnapshot("/old/failed", "pool/failed", "host")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FailSnapshot(failedOld.ID, time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	inProgressOld, err := s.CreateSnapshot("/old/in_progress", "pool/in_progress", "host")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	fz := &fakeZFS{
		rows: [][6]string{
			{"pool/completed", "/pool/completed", "no", "on", "filesystem", "0"},
			{"pool/failed", "/pool/failed", "no", "on", "filesystem", "0"},
			{"pool/in_progress", "/pool/in_progress", "no", "on", "filesystem", "0"},
		},
	}

	res, err := RunZFSScan(ZFSScanConfig{DBPath: dbPath, Roots: []string{"pool"}, Threads: 1, Runner: fz.runner(), ForceNew: true})
	if err != nil {
		t.Fatalf("RunZFSScan with ForceNew: %v", err)
	}

	wantScanned := []string{"pool/completed", "pool/failed", "pool/in_progress"}
	sort.Strings(res.Scanned)
	if fmt.Sprint(res.Scanned) != fmt.Sprint(wantScanned) {
		t.Errorf("Scanned = %v, want %v", res.Scanned, wantScanned)
	}
	if len(res.AlreadyDone) != 0 || len(res.Skipped) != 0 || len(res.Failed) != 0 {
		t.Errorf("ForceNew should bypass every prior-state check; AlreadyDone=%v Skipped=%v Failed=%v", res.AlreadyDone, res.Skipped, res.Failed)
	}

	s, err = sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for name, oldID := range map[string]int64{
		"pool/completed":   completedOld.ID,
		"pool/failed":      failedOld.ID,
		"pool/in_progress": inProgressOld.ID,
	} {
		got, err := s.FindSnapshot(name)
		if err != nil {
			t.Fatalf("FindSnapshot(%s): %v", name, err)
		}
		if got.ID == oldID {
			t.Errorf("%s: expected a new snapshot row distinct from the old one (#%d), FindSnapshot still resolves to it", name, oldID)
		}
		if got.Status != models.StatusCompleted {
			t.Errorf("%s: new snapshot status = %s, want completed", name, got.Status)
		}
	}

	// The old rows must still exist, as history - not overwritten or deleted.
	snaps, err := s.ListSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 6 {
		t.Errorf("expected 6 total snapshot rows (3 old + 3 new), got %d", len(snaps))
	}
}

// TestOverallProgress_Line exercises overallProgress.line()'s math directly,
// rather than only indirectly through a full RunZFSScan run - in particular
// the percentage/ETA guard conditions (zero totalBytes, too-early-to-estimate,
// and done overshooting totalBytes).
func TestOverallProgress_Line(t *testing.T) {
	t.Run("zero totalBytes: no percentage or ETA, just dataset counts", func(t *testing.T) {
		p := &overallProgress{start: time.Now(), totalDatasets: 3}
		got := p.line(1, 0)
		if !strings.Contains(got, "1/3 datasets done") {
			t.Errorf("line = %q, want dataset counts", got)
		}
		if strings.Contains(got, "%") || strings.Contains(got, "ETA") {
			t.Errorf("line = %q, should have no percentage or ETA when totalBytes is 0", got)
		}
	})

	t.Run("too early to estimate: no ETA before 2s elapsed", func(t *testing.T) {
		p := &overallProgress{start: time.Now(), totalDatasets: 2, totalBytes: 1000}
		got := p.line(0, 500)
		if !strings.Contains(got, "50.0%") {
			t.Errorf("line = %q, want a 50.0%% figure", got)
		}
		if strings.Contains(got, "ETA") {
			t.Errorf("line = %q, should have no ETA before 2s elapsed", got)
		}
	})

	t.Run("normal case: computable ETA", func(t *testing.T) {
		p := &overallProgress{start: time.Now().Add(-10 * time.Second), totalDatasets: 2, totalBytes: 1000}
		got := p.line(0, 500)
		if !strings.Contains(got, "50.0%") {
			t.Errorf("line = %q, want a 50.0%% figure", got)
		}
		// done=500 over elapsed~10s -> rate ~50/s -> remaining 500 -> ETA ~10s.
		if !strings.Contains(got, "ETA ~10s") {
			t.Errorf("line = %q, want ETA ~10s", got)
		}
	})

	t.Run("done overshoots totalBytes: remaining clamped to zero", func(t *testing.T) {
		p := &overallProgress{start: time.Now().Add(-10 * time.Second), totalDatasets: 1, totalBytes: 1000}
		got := p.line(0, 1500)
		if !strings.Contains(got, "ETA ~0s") {
			t.Errorf("line = %q, want ETA ~0s when done overshoots totalBytes", got)
		}
	})

	t.Run("datasetDone accumulates bytesDoneBefore", func(t *testing.T) {
		p := newOverallProgress([]zfsutil.Dataset{{Name: "a", Referenced: 100}, {Name: "b", Referenced: 200}})
		if p.totalBytes != 300 {
			t.Fatalf("totalBytes = %d, want 300", p.totalBytes)
		}
		p.datasetDone(zfsutil.Dataset{Name: "a", Referenced: 100})
		got := p.line(1, 0)
		if !strings.Contains(got, "33.3%") {
			t.Errorf("line = %q, want 33.3%% after one 100/300 dataset done", got)
		}
	})
}

func TestCleanupMounts_UnmountsAndRemoves(t *testing.T) {
	fz := &fakeZFS{}
	dirs := []string{t.TempDir(), t.TempDir()}

	cleanupMounts(fz.runner(), dirs)

	gotUnmount := append([]string(nil), fz.unmountCalls...)
	sort.Strings(gotUnmount)
	wantUnmount := append([]string(nil), dirs...)
	sort.Strings(wantUnmount)
	if fmt.Sprint(gotUnmount) != fmt.Sprint(wantUnmount) {
		t.Errorf("unmountCalls = %v, want %v", gotUnmount, wantUnmount)
	}
	for _, dir := range dirs {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed after cleanup, stat err = %v", dir, err)
		}
	}
}

func TestCleanupMounts_UnmountFailureLeavesDirBehind(t *testing.T) {
	var umountCalls int
	run := func(name string, args ...string) (string, error) {
		if name == "umount" {
			umountCalls++
			return "umount: target is busy", fmt.Errorf("exit status 1")
		}
		return "", nil
	}
	dir := t.TempDir()

	cleanupMounts(run, []string{dir})

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dir should be left behind (not removed) when unmount fails: stat err = %v", err)
	}
	if umountCalls != unmountRetries {
		t.Errorf("umount attempts = %d, want %d (exhausting all retries before giving up)", umountCalls, unmountRetries)
	}
}

// TestUnmountWithRetry_SucceedsAfterTransientBusy proves a still-busy mount
// (e.g. right after a scan process exits or is interrupted) gets retried
// rather than immediately reported as a failure.
func TestUnmountWithRetry_SucceedsAfterTransientBusy(t *testing.T) {
	var umountCalls int
	run := func(name string, args ...string) (string, error) {
		if name != "umount" {
			return "", nil
		}
		umountCalls++
		if umountCalls < 3 {
			return "umount: target is busy", fmt.Errorf("exit status 1")
		}
		return "", nil
	}

	if err := unmountWithRetry(run, "/tmp/whatever"); err != nil {
		t.Fatalf("unmountWithRetry: %v", err)
	}
	if umountCalls != 3 {
		t.Errorf("umount attempts = %d, want 3 (fail, fail, succeed)", umountCalls)
	}
}

func TestHandleInterrupt_ExitCodeAndCleanup(t *testing.T) {
	fz := &fakeZFS{}
	dir := t.TempDir()

	code := handleInterrupt(syscall.SIGINT, fz.runner(), []string{dir})
	if code != 130 {
		t.Errorf("exit code = %d, want 130 (128 + SIGINT)", code)
	}
	if len(fz.unmountCalls) != 1 || fz.unmountCalls[0] != dir {
		t.Errorf("unmountCalls = %v, want [%s]", fz.unmountCalls, dir)
	}

	code = handleInterrupt(syscall.SIGTERM, fz.runner(), nil)
	if code != 143 {
		t.Errorf("exit code = %d, want 143 (128 + SIGTERM)", code)
	}
}
