package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"fluxion/internal/store/sqlite"
	"fluxion/internal/zfsutil"
)

// fakeZFS backs a small in-memory dataset table with a Runner, so
// RunZFSScan's orchestration can be tested without a real `zfs` binary or
// root (there is neither in this project's build environment).
type fakeZFS struct {
	rows         [][5]string // name, mountpoint, mounted, canmount, type
	failMountFor map[string]bool
	mountCalls   []string
	unmountCalls []string
	listCalls    int
}

func (f *fakeZFS) runner() zfsutil.Runner {
	return func(name string, args ...string) (string, error) {
		switch args[0] {
		case "list":
			f.listCalls++
			var out string
			for _, r := range f.rows {
				out += fmt.Sprintf("%s\t%s\t%s\t%s\t%s\n", r[0], r[1], r[2], r[3], r[4])
			}
			return out, nil
		case "mount":
			ds := args[1]
			if f.failMountFor[ds] {
				return "permission denied", fmt.Errorf("exit status 1")
			}
			f.mountCalls = append(f.mountCalls, ds)
			return "", nil
		case "unmount":
			f.unmountCalls = append(f.unmountCalls, args[1])
			return "", nil
		default:
			return "", fmt.Errorf("unexpected zfs subcommand %q", args[0])
		}
	}
}

func mustDirWithFile(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunZFSScan_MountsScansAndUnmounts(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	alreadyMountedDir := mustDirWithFile(t, "already")
	needsMountDir := mustDirWithFile(t, "needsmount")

	fz := &fakeZFS{
		rows: [][5]string{
			{"pool", alreadyMountedDir, "yes", "on", "filesystem"},
			{"pool/needs_mount", needsMountDir, "no", "on", "filesystem"},
			{"pool/container", "/pool/container", "no", "off", "filesystem"},
			{"pool/vol", "-", "no", "-", "volume"},
			{"pool/mount_fails", "/some/path", "no", "on", "filesystem"},
			{"pool/legacy_unmounted", "legacy", "no", "on", "filesystem"},
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

	wantScanned := []string{"pool", "pool/needs_mount"}
	sort.Strings(res.Scanned)
	if fmt.Sprint(res.Scanned) != fmt.Sprint(wantScanned) {
		t.Errorf("Scanned = %v, want %v", res.Scanned, wantScanned)
	}

	wantSkipped := []string{"pool/container", "pool/legacy_unmounted", "pool/vol"}
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

	// Only the dataset that actually needed mounting should have been
	// mounted, and only it should have been unmounted afterward.
	if len(fz.mountCalls) != 1 || fz.mountCalls[0] != "pool/needs_mount" {
		t.Errorf("mountCalls = %v, want [pool/needs_mount]", fz.mountCalls)
	}
	if len(fz.unmountCalls) != 1 || fz.unmountCalls[0] != "pool/needs_mount" {
		t.Errorf("unmountCalls = %v, want [pool/needs_mount]", fz.unmountCalls)
	}
}

func TestRunZFSScan_RerunIsIdempotent(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	dir := mustDirWithFile(t, "data")
	fz := &fakeZFS{
		rows: [][5]string{
			{"pool", dir, "yes", "on", "filesystem"},
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
		t.Error("re-running an already-mounted, already-scanned dataset should mount/unmount nothing")
	}
	if res.HadFailures {
		t.Error("re-running a clean prior scan should not report failures")
	}
}

func TestRunZFSScan_DryRunTouchesNothing(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	dir := mustDirWithFile(t, "data")
	fz := &fakeZFS{
		rows: [][5]string{
			{"pool", dir, "no", "on", "filesystem"},
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
