package app

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// CaptureStdout captures stdout output for testing
func captureStdout(f func() error) (string, error) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := f()

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String(), err
}

func TestRunSize_Formatted(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	// 10 files of 100 bytes = 1000 bytes.
	// The paths must be distinct: a snapshot holds one row per path, so ten
	// records for "/file" would collapse to a single 100-byte file.
	files := make([]string, 10)
	for i := 0; i < 10; i++ {
		files[i] = fmt.Sprintf("/file%d", i)
	}
	createDummySnapshot(t, dbPath, "snap1", "", files)

	cfg := SizeConfig{
		DBPath:     dbPath,
		SnapQuery:  "snap1",
		TotalBytes: false,
	}

	out, err := captureStdout(func() error {
		return RunSize(cfg)
	})
	if err != nil {
		t.Fatalf("RunSize failed: %v", err)
	}

	expected := "1000 B"
	if strings.TrimSpace(out) != expected {
		t.Errorf("expected '%s', got '%s'", expected, strings.TrimSpace(out))
	}
}

func TestRunSize_TotalBytes(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	// 10 files of 100 bytes = 1000 bytes.
	// The paths must be distinct: a snapshot holds one row per path, so ten
	// records for "/file" would collapse to a single 100-byte file.
	files := make([]string, 10)
	for i := 0; i < 10; i++ {
		files[i] = fmt.Sprintf("/file%d", i)
	}
	createDummySnapshot(t, dbPath, "snap1", "", files)

	cfg := SizeConfig{
		DBPath:     dbPath,
		SnapQuery:  "snap1",
		TotalBytes: true,
	}

	out, err := captureStdout(func() error {
		return RunSize(cfg)
	})
	if err != nil {
		t.Fatalf("RunSize failed: %v", err)
	}

	expected := "1000"
	if strings.TrimSpace(out) != expected {
		t.Errorf("expected '%s', got '%s'", expected, strings.TrimSpace(out))
	}
}
