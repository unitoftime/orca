package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every ssh orca runs shares one connection per host: a handshake per command
// is a chance for a machine's sshd, busy with bots, to drop it.
func TestSSHSharesAConnection(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	sshControlPath = onceControlPath()

	for _, args := range [][]string{sshArgs("root@h", "true"), sshArgsStdin("root@h", "true")} {
		joined := strings.Join(args, " ")
		for _, want := range []string{"ControlMaster=auto", "ControlPersist=60s", "ControlPath=", "ConnectTimeout=10"} {
			if !strings.Contains(joined, want) {
				t.Errorf("ssh args missing %s: %v", want, args)
			}
		}
		if got := args[len(args)-2:]; !slices.Equal(got, []string{"root@h", "true"}) {
			t.Errorf("host and command must come last: %v", args)
		}
	}
	if sshArgs("h", "c")[0] != "-n" || sshArgsStdin("h", "c")[0] == "-n" {
		t.Error("only piped commands keep stdin")
	}
	if e := rsyncSSH(); !strings.HasPrefix(e, "ssh ") || !strings.Contains(e, "ControlPath=") {
		t.Errorf("rsync should ride the same connection: %s", e)
	}
}

// A socket path too long for the kernel makes ssh refuse to connect at all,
// so orca connects without sharing instead.
func TestSSHWithoutSharing(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("x", 100))
	t.Setenv("XDG_CACHE_HOME", long)
	sshControlPath = onceControlPath()
	defer func() { sshControlPath = onceControlPath() }()

	args := strings.Join(sshArgs("h", "c"), " ")
	if _, err := os.Stat(filepath.Join(long, "orca", "ssh")); err != nil {
		t.Fatalf("the directory should be creatable, so the length is what is tested: %v", err)
	}
	if strings.Contains(args, "Control") {
		t.Errorf("no usable socket path should mean no sharing: %s", args)
	}
}
