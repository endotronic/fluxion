package zfsutil

import (
	"fmt"
	"testing"
)

func fakeListOutput() string {
	// Deliberately out of name order, to prove ListDatasets sorts.
	rows := [][5]string{
		{"luna/kevin/archives", "/luna/kevin/archives", "yes", "on", "filesystem"},
		{"luna", "/luna", "yes", "on", "filesystem"},
		{"luna/proxmox", "none", "no", "off", "filesystem"},
		{"luna/proxmox/vm-302-disk-0", "-", "no", "-", "volume"},
		{"luna/kevin", "/luna/kevin", "no", "on", "filesystem"},
		{"luna/legacy-mount", "legacy", "no", "on", "filesystem"},
	}
	var out string
	for _, r := range rows {
		out += fmt.Sprintf("%s\t%s\t%s\t%s\t%s\n", r[0], r[1], r[2], r[3], r[4])
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

func TestMount_InvokesCorrectArgs(t *testing.T) {
	var gotName string
	var gotArgs []string
	run := func(name string, args ...string) (string, error) {
		gotName, gotArgs = name, args
		return "", nil
	}
	if err := Mount(run, "luna/kevin"); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if gotName != "zfs" || len(gotArgs) != 2 || gotArgs[0] != "mount" || gotArgs[1] != "luna/kevin" {
		t.Errorf("Mount ran %q %v, want zfs [mount luna/kevin]", gotName, gotArgs)
	}
}

func TestMount_PropagatesError(t *testing.T) {
	run := func(name string, args ...string) (string, error) {
		return "cannot mount: permission denied", fmt.Errorf("exit status 1")
	}
	if err := Mount(run, "luna/kevin"); err == nil {
		t.Fatal("expected Mount to propagate the runner's error")
	}
}

func TestUnmount_InvokesCorrectArgs(t *testing.T) {
	var gotArgs []string
	run := func(name string, args ...string) (string, error) {
		gotArgs = args
		return "", nil
	}
	if err := Unmount(run, "luna/kevin"); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "unmount" || gotArgs[1] != "luna/kevin" {
		t.Errorf("Unmount ran args %v, want [unmount luna/kevin]", gotArgs)
	}
}
