package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Node is a remote host reachable over SSH.
type Node struct {
	Host string // "user@ip" or just "ip"
}

// sshOptions are the options every ssh orca runs is given, rsync's included.
//
// A bounded ConnectTimeout keeps any command against a dead box from hanging
// on the default multi-minute TCP timeout.
//
// The rest share one connection per host. orca runs a command per ssh (a
// status is three, an apply dozens, `top -w` one every few seconds), and on
// its own each would be a TCP connection and key exchange. On a machine on
// the public internet, sshd admits only ten connections at a time that have
// not yet logged in, bots trying passwords hold those slots, and past ten it
// drops new arrivals at random, which shows up as
// "kex_exchange_identification: Connection reset by peer" in the middle of a
// status. So the first ssh to a host leaves a connection behind that the rest
// reuse, and it closes itself after a minute unused: a run makes one
// handshake instead of dozens, and each command skips a round trip.
func sshOptions() []string {
	opts := []string{"-o", "ConnectTimeout=10"}
	if path := sshControlPath(); path != "" {
		opts = append(opts,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+path,
			"-o", "ControlPersist=60s",
			// A shared connection whose network went away is noticed within
			// a minute, rather than holding every command after it.
			"-o", "ServerAliveInterval=20",
			"-o", "ServerAliveCountMax=3",
		)
	}
	return opts
}

// sshControlPath is where the shared connections' sockets live: a directory
// only you can open, since whoever can reach a socket can run commands through
// it. Empty when there is nowhere to put one, and each command then connects
// on its own.
var sshControlPath = onceControlPath()

func onceControlPath() func() string { return sync.OnceValue(controlPath) }

func controlPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	dir = filepath.Join(dir, "orca", "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	// %C is a 40-character hash of the connection. A socket's path has to fit
	// in about a hundred bytes, and ssh refuses to connect at all rather than
	// fall back when it does not.
	if len(dir)+1+40 > 100 {
		return ""
	}
	return filepath.Join(dir, "%C")
}

// -n redirects stdin from /dev/null for every command that is not deliberately
// being fed input. Without it the remote command inherits orca's own stdin, and
// a tool that reads stdin when it is not a terminal will block forever waiting
// for input that is never coming.
func sshArgs(host, cmd string) []string {
	return append(append([]string{"-n"}, sshOptions()...), host, cmd)
}

// sshArgsStdin is the same without -n, for commands that are piped input.
func sshArgsStdin(host, cmd string) []string {
	return append(sshOptions(), host, cmd)
}

// unreachable reports an ssh that failed to reach its machine at all, as
// opposed to a command that ran there and failed: ssh exits 255 for its own
// errors and passes the remote command's status through otherwise.
func unreachable(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 255
}

// rsyncSSH is rsync's -e: the same ssh, so an upload rides the shared
// connection too. rsync splits it on spaces and honours quotes.
func rsyncSSH() string {
	parts := []string{"ssh"}
	for _, o := range sshOptions() {
		if strings.ContainsAny(o, " \t'\"") {
			o = "'" + strings.ReplaceAll(o, "'", `'\''`) + "'"
		}
		parts = append(parts, o)
	}
	return strings.Join(parts, " ")
}

// Run executes a command on the remote host, streaming output to the step log.
func (n Node) Run(ctx context.Context, cmd string) error {
	c := exec.CommandContext(ctx, "ssh", sshArgs(n.Host, cmd)...)
	c.Stdout = getStdout(ctx)
	c.Stderr = getStderr(ctx)
	if err := c.Run(); err != nil {
		return fmt.Errorf("ssh %s: %w", n.Host, err)
	}
	return nil
}

// RunOutput executes a command on the remote host and returns its stdout.
// Stderr still streams to the step log so failures stay diagnosable.
func (n Node) RunOutput(ctx context.Context, cmd string) (string, error) {
	var out bytes.Buffer
	c := exec.CommandContext(ctx, "ssh", sshArgs(n.Host, cmd)...)
	c.Stdout = &out
	c.Stderr = getStderr(ctx)
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("ssh %s: %w", n.Host, err)
	}
	return out.String(), nil
}

// UploadFile copies one file to a remote path via rsync, executable. rsync
// writes a temporary file and renames it into place, so nothing ever sees a
// half-written file at the destination.
func (n Node) UploadFile(ctx context.Context, local, remote string) error {
	c := exec.CommandContext(ctx, "rsync", "-e", rsyncSSH(), "-z", "--chmod=F755", local, n.Host+":"+remote)
	var errOut bytes.Buffer
	c.Stderr = &errOut
	if err := c.Run(); err != nil {
		return fmt.Errorf("rsync %s -> %s:%s: %w: %s", local, n.Host, remote, err, strings.TrimSpace(errOut.String()))
	}
	return nil
}

// UploadDir syncs a local directory to a remote path via rsync.
func (n Node) UploadDir(ctx context.Context, localDir, remoteDir string, extraFlags ...string) error {
	args := append([]string{"-e", rsyncSSH(), "-az"}, extraFlags...)
	args = append(args, localDir, n.Host+":"+remoteDir)
	c := exec.CommandContext(ctx, "rsync", args...)
	c.Stdout = getStdout(ctx)
	c.Stderr = getStderr(ctx)
	if err := c.Run(); err != nil {
		return fmt.Errorf("rsync %s -> %s:%s: %w", localDir, n.Host, remoteDir, err)
	}
	return nil
}

// IP returns the address portion of Host, stripping any user@ prefix.
func (n Node) IP() string {
	host := n.Host
	if idx := strings.LastIndex(host, "@"); idx >= 0 {
		host = host[idx+1:]
	}
	return host
}
