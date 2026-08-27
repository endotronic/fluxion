package app

import (
	"strings"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"fluxion/internal/models"
	"fluxion/internal/store/sqlite"
)

// TestRunSnapshot_StopChReturnsInterruptedAndLeavesInProgress proves that an
// already-closed StopCh (standing in for zfs-scan's SIGINT case) makes
// RunSnapshot stop and return ErrScanInterrupted without marking the
// snapshot completed or failed - it must stay in_progress so a later run's
// ResumeFrom can pick it back up, per the severity rule in
// knowledge/goals.md: a snapshot that looks "completed" but is actually
// missing most of the tree is a false-safe answer.
func TestRunSnapshot_StopChReturnsInterruptedAndLeavesInProgress(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, "f")
		p = p + string(rune('0'+i))
		if err := os.WriteFile(p, []byte("hello"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}

	stopCh := make(chan struct{})
	close(stopCh)

	err := RunSnapshot(SnapshotConfig{
		TargetDir: dir,
		DBPath:    dbPath,
		Name:      "interrupted-ds",
		Threads:   1,
		StopCh:    stopCh,
	})
	if !errors.Is(err, ErrScanInterrupted) {
		t.Fatalf("RunSnapshot error = %v, want ErrScanInterrupted", err)
	}

	st, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	snap, err := st.FindSnapshot("interrupted-ds")
	if err != nil {
		t.Fatalf("FindSnapshot: %v", err)
	}
	if snap == nil {
		t.Fatal("expected the snapshot row to exist even though the scan was interrupted")
	}
	if snap.Status != models.StatusInProgress {
		t.Errorf("snapshot status = %s, want %s (interrupted scans must stay resumable, not look completed or failed)", snap.Status, models.StatusInProgress)
	}
}

func TestTruncateLine(t *testing.T) {
	long := "Scanning (Found 123456/999.9 TB) [100% Saturated]... (999.9 GB/s avg, ETA ~999h59m59s)  16% |###############| (700 GB/4.2 TB) [12m3s]"

	cases := []struct {
		name  string
		line  string
		width int
		want  string
	}{
		{"unknown width leaves line untouched", strings.Repeat("x", 200), 0, strings.Repeat("x", 200)},
		{"short line untouched", "hello", 80, "hello"},
		{"exact width untouched", strings.Repeat("x", 80), 80, strings.Repeat("x", 80)},
		{"one char over is truncated with an ellipsis", strings.Repeat("x", 81), 80, strings.Repeat("x", 79) + "…"},
		{"a line with ANSI escapes but short visible text is untouched",
			"\x1b[1Ahello\x1b[J", 80, "\x1b[1Ahello\x1b[J"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateLine(tc.line, tc.width); got != tc.want {
				t.Errorf("truncateLine(%q, %d) = %q, want %q", tc.line, tc.width, got, tc.want)
			}
		})
	}

	t.Run("truncated line never exceeds the terminal width", func(t *testing.T) {
		got := truncateLine(long, 80)
		if n := len([]rune(got)); n > 80 {
			t.Errorf("truncateLine result is %d runes wide, want <= 80: %q", n, got)
		}
	})
}
