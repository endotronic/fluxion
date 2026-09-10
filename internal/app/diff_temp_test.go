package app

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

// The advisory has two jobs and both are easy to lose silently: say how much
// temp space a big diff may want, and say when the temp directory is memory.
// On most Linux systems the default /tmp is a tmpfs, so a diff that "spills to
// disk" there is not spilling at all - the bound the streaming engine exists to
// provide quietly stops holding, with nothing on screen to say so.
func TestReportTempSpace(t *testing.T) {
	capture := func(tempDir string, nodes int64) string {
		var buf bytes.Buffer
		old := logrus.StandardLogger().Out
		oldLevel := logrus.GetLevel()
		logrus.SetOutput(&buf)
		logrus.SetLevel(logrus.DebugLevel)
		defer func() {
			logrus.SetOutput(old)
			logrus.SetLevel(oldLevel)
		}()
		reportTempSpace(tempDir, nodes)
		return buf.String()
	}

	t.Run("silent for a diff that never leaves memory", func(t *testing.T) {
		if out := capture(t.TempDir(), 1000); out != "" {
			t.Errorf("expected no output for a tiny diff, got:\n%s", out)
		}
	})

	t.Run("reports free space once the estimate matters", func(t *testing.T) {
		restore := tempSpaceWorthMentioning
		tempSpaceWorthMentioning = 1
		defer func() { tempSpaceWorthMentioning = restore }()

		out := capture(t.TempDir(), 1000)
		if !strings.Contains(out, "Temp space:") {
			t.Errorf("expected a temp-space line, got:\n%s", out)
		}
	})

	t.Run("warns when the temp directory is memory-backed", func(t *testing.T) {
		restore := tempSpaceWorthMentioning
		tempSpaceWorthMentioning = 1
		defer func() { tempSpaceWorthMentioning = restore }()

		// /dev/shm is a tmpfs wherever this can be tested at all.
		if _, err := os.Stat("/dev/shm"); err != nil {
			t.Skip("no /dev/shm to test a memory-backed filesystem against")
		}
		out := capture("/dev/shm", 1000)
		if !strings.Contains(out, "which is memory") {
			t.Errorf("expected a tmpfs warning for /dev/shm, got:\n%s", out)
		}
		if !strings.Contains(out, "--temp-dir") {
			t.Errorf("warning does not say what to do about it:\n%s", out)
		}
	})
}
