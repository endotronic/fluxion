package zfsutil

import (
	"fmt"
	"testing"
)

func fakeListOutput() string {
	// Deliberately out of name order, to prove ListDatasets sorts.
	rows := [][6]string{
		{"luna/kevin/archives", "/luna/kevin/archives", "yes", "on", "filesystem", "1073741824"},
		{"luna", "/luna", "yes", "on", "filesystem", "5368709120"},
		{"luna/proxmox", "none", "no", "off", "filesystem", "0"},
		{"luna/proxmox/vm-302-disk-0", "-", "no", "-", "volume", "17179869184"},
		{"luna/kevin", "/luna/kevin", "no", "on", "filesystem", "2147483648"},
		{"luna/legacy-mount", "legacy", "no", "on", "filesystem", "0"},
	}
	var out string
	for _, r := range rows {
		out += fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\n", r[0], r[1], r[2], r[3], r[4], r[5])
	}
	return out
}

func TestListDatasets_ParsesAndSorts(t *testing.T) {
	var gotArgs []string
	run := func(name string, args ...string) (string, error) {
		gotArgs = args
		return fakeListOutput(), nil
	}

	datasets, err := ListDatasets(run, []string{"luna"})
	if err != nil {
		t.Fatalf("ListDatasets: %v", err)
	}

	wantOrder := []string{"luna", "luna/kevin", "luna/kevin/archives", "luna/legacy-mount", "luna/proxmox", "luna/proxmox/vm-302-disk-0"}
	if len(datasets) != len(wantOrder) {
		t.Fatalf("got %d datasets, want %d", len(datasets), len(wantOrder))
	}
	for i, want := range wantOrder {
		if datasets[i].Name != want {
			t.Errorf("datasets[%d].Name = %q, want %q", i, datasets[i].Name, want)
		}
	}

	if datasets[0].Mounted != true || datasets[0].CanMount != "on" || datasets[0].Type != "filesystem" {
		t.Errorf("luna: unexpected fields %+v", datasets[0])
	}
	if datasets[0].Used != 5368709120 {
		t.Errorf("luna: Used = %d, want 5368709120", datasets[0].Used)
	}

	found := false
	for _, d := range datasets {
		if d.Name == "luna/proxmox/vm-302-disk-0" {
			found = true
			if d.Type != "volume" {
				t.Errorf("vm-302-disk-0 Type = %q, want volume", d.Type)
			}
		}
	}
	if !found {
		t.Fatal("volume dataset missing from parsed results")
	}

	if len(gotArgs) == 0 || gotArgs[len(gotArgs)-1] != "luna" {
		t.Errorf("expected roots passed through to `zfs list`, got args %v", gotArgs)
	}
}

func TestListDatasets_CommandFailure(t *testing.T) {
	run := func(name string, args ...string) (string, error) {
		return "cannot open 'nope': dataset does not exist", fmt.Errorf("exit status 1")
	}
	if _, err := ListDatasets(run, []string{"nope"}); err == nil {
		t.Fatal("expected an error when `zfs list` fails")
	}
}

func TestMountAt_InvokesCorrectArgs(t *testing.T) {
	var gotName string
	var gotArgs []string
	run := func(name string, args ...string) (string, error) {
		gotName, gotArgs = name, args
		return "", nil
	}
	if err := MountAt(run, "luna/no-mountpoint", "/tmp/fluxion-abc123"); err != nil {
		t.Fatalf("MountAt: %v", err)
	}
	wantArgs := []string{"-t", "zfs", "-o", "zfsutil", "luna/no-mountpoint", "/tmp/fluxion-abc123"}
	if gotName != "mount" || len(gotArgs) != len(wantArgs) {
		t.Fatalf("MountAt ran %q %v, want mount %v", gotName, gotArgs, wantArgs)
	}
	for i := range wantArgs {
		if gotArgs[i] != wantArgs[i] {
			t.Errorf("MountAt arg[%d] = %q, want %q", i, gotArgs[i], wantArgs[i])
		}
	}
}

func TestMountAt_PropagatesError(t *testing.T) {
	run := func(name string, args ...string) (string, error) {
		return "mount.zfs: dataset does not exist", fmt.Errorf("exit status 1")
	}
	if err := MountAt(run, "luna/no-mountpoint", "/tmp/x"); err == nil {
		t.Fatal("expected MountAt to propagate the runner's error")
	}
}

func TestUnmountPath_InvokesCorrectArgs(t *testing.T) {
	var gotName string
	var gotArgs []string
	run := func(name string, args ...string) (string, error) {
		gotName, gotArgs = name, args
		return "", nil
	}
	if err := UnmountPath(run, "/tmp/fluxion-abc123"); err != nil {
		t.Fatalf("UnmountPath: %v", err)
	}
	if gotName != "umount" || len(gotArgs) != 1 || gotArgs[0] != "/tmp/fluxion-abc123" {
		t.Errorf("UnmountPath ran %q %v, want umount [/tmp/fluxion-abc123]", gotName, gotArgs)
	}
}

func TestUnmountPath_PropagatesError(t *testing.T) {
	run := func(name string, args ...string) (string, error) {
		return "umount: /tmp/x: not mounted", fmt.Errorf("exit status 1")
	}
	if err := UnmountPath(run, "/tmp/x"); err == nil {
		t.Fatal("expected UnmountPath to propagate the runner's error")
	}
}
