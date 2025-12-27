package util

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

// GetMountPoints returns a list of all absolute paths where filesystems are mounted.
func GetMountPoints() ([]string, error) {
	if runtime.GOOS == "linux" {
		return getMountPointsLinux()
	} else if runtime.GOOS == "darwin" {
		return getMountPointsDarwin()
	}
	return nil, fmt.Errorf("unsupported OS: %s", runtime.GOOS)
}

func getMountPointsLinux() ([]string, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var mounts []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			mounts = append(mounts, fields[1])
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return mounts, nil
}

func getMountPointsDarwin() ([]string, error) {
	cmd := exec.Command("mount")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	// Output format: /dev/disk1s1 on / (apfs, local, journaled)
	// Regex: ` on (.+) \(`
	re := regexp.MustCompile(` on (.+) \(`)
	
	var mounts []string
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		matches := re.FindStringSubmatch(line)
		if len(matches) > 1 {
			mounts = append(mounts, matches[1])
		}
	}
	return mounts, nil
}
