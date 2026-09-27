package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Pruning keeps the build being deployed and the two newest others — the
// previous job, which a failed rollout reverts to, names one of them — even
// when the build being deployed is the oldest file there, as it is when going
// back to an earlier orca.
func TestPruneStatusBinaries(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	names := []string{"orca-0001", "orca-0002", "orca-0003", "orca-0004", "orca-0005"}
	for i, n := range names {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatal(err)
		}
		// orca-0001 oldest, orca-0005 newest.
		mt := now.Add(time.Duration(i-len(names)) * time.Minute)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "unrelated"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	script := pruneStatusBinariesScript(filepath.Join(dir, "orca-0001"))
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}

	entries, _ := os.ReadDir(dir)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	sort.Strings(left)
	want := "orca-0001 orca-0004 orca-0005 unrelated"
	if got := strings.Join(left, " "); got != want {
		t.Errorf("left %q, want %q", got, want)
	}
}

// A dynamically linked orca carries the C library of the machine that built
// it, which the status image does not have.
func TestRunsOnMachineRefusesDynamicBinaries(t *testing.T) {
	if runsOnMachine("/bin/sh") {
		t.Error("/bin/sh is dynamically linked and should not be shipped")
	}
	if runsOnMachine(filepath.Join(t.TempDir(), "missing")) {
		t.Error("a missing file cannot run anywhere")
	}
}
