package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Dial opens a connection to addr as the machine sees it, carried over SSH.
//
// This is how orca reaches what listens only on the machine: Nomad's API on
// loopback, the log store on the container bridge. Nothing is opened on this
// side, no port and no socket: the connection is an ssh process's stdin and
// stdout, on the shared connection every other command rides.
//
// The machine's sshd has to allow it (AllowTcpForwarding, on unless someone
// turned it off). One that does not says so on the first read.
func (n Node) Dial(ctx context.Context, addr string) (net.Conn, error) {
	// Not CommandContext: the context is the dial's, and the connection is
	// kept and reused long after the request that opened it.
	cmd := exec.Command("ssh", append(sshOptions(), "-W", addr, "--", n.Host)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &sshConn{cmd: cmd, w: stdin, r: stdout, host: n.Host, addr: addr}
	cmd.Stderr = &c.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ssh %s: %w", n.Host, err)
	}
	return c, nil
}

// sshConn is one `ssh -W`: what is written goes to the address on the
// machine, and what it answers is read back.
type sshConn struct {
	cmd    *exec.Cmd
	w      io.WriteCloser
	r      io.ReadCloser
	stderr bytes.Buffer
	host   string
	addr   string
	close  sync.Once
}

// Read reports why ssh gave up, where it did: a refused forward or an
// unreachable machine ends the stream, and the reason is on ssh's stderr.
func (c *sshConn) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err != nil && n == 0 {
		// Wait, so that everything ssh had to say has been written.
		c.Close()
		if msg := strings.TrimSpace(c.stderr.String()); msg != "" {
			return 0, fmt.Errorf("ssh %s to %s: %s", c.host, c.addr, msg)
		}
	}
	return n, err
}

func (c *sshConn) Write(p []byte) (int, error) { return c.w.Write(p) }

func (c *sshConn) Close() error {
	c.close.Do(func() {
		c.w.Close()
		c.cmd.Process.Kill()
		c.cmd.Wait()
	})
	return nil
}

func (c *sshConn) LocalAddr() net.Addr  { return sshAddr(c.host) }
func (c *sshConn) RemoteAddr() net.Addr { return sshAddr(c.addr) }

// A pipe has no deadlines. Nothing here sets one: requests are bounded by
// their contexts, which close the connection.
func (c *sshConn) SetDeadline(time.Time) error      { return nil }
func (c *sshConn) SetReadDeadline(time.Time) error  { return nil }
func (c *sshConn) SetWriteDeadline(time.Time) error { return nil }

type sshAddr string

func (a sshAddr) Network() string { return "ssh" }
func (a sshAddr) String() string  { return string(a) }

// tunnelConns is how many connections one HTTP client keeps open to an
// address on the machine. Each is a channel on the shared SSH connection, so
// a handful costs nothing and lets a read of every job be one round trip's
// wait rather than one per job. Without a shared connection each would be an
// SSH handshake of its own, so there is one.
func tunnelConns() int {
	if sshControlPath() == "" {
		return 1
	}
	return 8
}

// HTTPClient makes requests from the machine's point of view: a URL's host
// is resolved and connected to there, not here.
func (n Node) HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return n.Dial(ctx, addr)
		},
		MaxConnsPerHost:     tunnelConns(),
		MaxIdleConnsPerHost: tunnelConns(),
		IdleConnTimeout:     30 * time.Second,
	}}
}
