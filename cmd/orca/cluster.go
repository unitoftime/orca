package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/internal/deploy"
	"github.com/unitoftime/orca/internal/manifest"
	"github.com/unitoftime/orca/internal/statuspage"
)

// Cluster is Nomad's API on one of the cluster's servers, reached over SSH.
//
// Nomad binds loopback on a single-machine cluster, so it has no listener to
// connect to from here. Each request instead rides a connection opened to it
// from the machine (see Node.Dial), on the SSH connection everything else
// shares: a request is a round trip, not a new session, and a read of every
// job is a few of them at once rather than one per job.
type Cluster struct {
	node Node

	// http makes requests from the machine's point of view.
	http *http.Client

	once   sync.Once
	api    *nomad.Client
	apiErr error
}

func newCluster(n Node) *Cluster { return &Cluster{node: n, http: n.HTTPClient()} }

// nomadAddr is where Nomad's HTTP API answers on the machine. It is always
// loopback: orca reaches it from the machine itself rather than connecting to
// a listener, so this does not change when a second machine gives bind_addr a
// real IP.
const nomadAddr = "http://127.0.0.1:4646"

// tokenPath is where each machine keeps the token Nomad's API is asked with:
// a file only root can read, written by bootstrap.
const tokenPath = "/etc/orca/nomad.token"

// requestTimeout bounds one request to Nomad. None of them wait on anything:
// what takes time (a deploy becoming healthy) is watched by asking again.
const requestTimeout = 30 * time.Second

// client is Nomad's API with the cluster's token, connected on first use.
func (c *Cluster) client(ctx context.Context) (*nomad.Client, error) {
	c.once.Do(func() {
		// Read with a command of its own, which also opens the shared SSH
		// connection the requests then ride.
		token, err := c.node.RunOutput(ctx, "cat "+tokenPath)
		if unreachable(err) {
			c.apiErr = err
			return
		}
		if err != nil {
			c.apiErr = fmt.Errorf("read the cluster's token (`orca bootstrap` writes it): %w", err)
			return
		}
		api := *c.http
		api.Timeout = requestTimeout
		c.api, c.apiErr = nomad.NewClient(&nomad.Config{
			Address:    nomadAddr,
			SecretID:   strings.TrimSpace(token),
			HttpClient: &api,
		})
	})
	return c.api, c.apiErr
}

// Jobs returns every orca-managed job the cluster knows about, keyed by job ID.
func (c *Cluster) Jobs(ctx context.Context) (map[string]deploy.JobState, error) {
	api, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	return deploy.ReadJobs(ctx, api)
}

// Alive reports whether this machine can answer for the cluster: reachable
// over SSH, with a Nomad agent that knows who the leader is.
func (c *Cluster) Alive(ctx context.Context) bool {
	api, err := c.client(ctx)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var leader string
	_, err = api.Raw().Query("/v1/status/leader", &leader, (&nomad.QueryOptions{}).WithContext(ctx))
	return err == nil && strings.Contains(leader, ":")
}

// PlanJobs asks Nomad what submitting each job would do, without submitting
// anything.
func (c *Cluster) PlanJobs(ctx context.Context, jobs []*nomad.Job) (map[string]deploy.JobPlan, error) {
	api, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	return deploy.PlanJobs(ctx, api, jobs)
}

// Submit registers a job, provided it is still at modifyIndex, the version its
// plan was made against (zero: provided it does not exist). Anything else
// having changed it since, another apply included, refuses the submit rather
// than overwriting a deploy nobody here has seen.
func (c *Cluster) Submit(ctx context.Context, job *nomad.Job, modifyIndex uint64) error {
	api, err := c.client(ctx)
	if err != nil {
		return err
	}
	if _, _, err := api.Jobs().EnforceRegister(job, modifyIndex, (&nomad.WriteOptions{}).WithContext(ctx)); err != nil {
		if strings.Contains(err.Error(), "Enforcing job modify index") {
			return fmt.Errorf("submit %s: it changed after it was planned; run apply again", *job.ID)
		}
		return fmt.Errorf("submit %s: %w", *job.ID, err)
	}
	return nil
}

// Stop stops a job and removes it from Nomad's state. It never touches data
// on disk, which only `orca purge` does.
func (c *Cluster) Stop(ctx context.Context, jobID string) error {
	api, err := c.client(ctx)
	if err != nil {
		return err
	}
	if _, _, err := api.Jobs().Deregister(jobID, true, (&nomad.WriteOptions{}).WithContext(ctx)); err != nil {
		return fmt.Errorf("stop %s: %w", jobID, err)
	}
	return nil
}

// Runtime returns the live allocation and deployment state.
func (c *Cluster) Runtime(ctx context.Context) ([]deploy.AllocState, []deploy.DeploymentState, error) {
	api, err := c.client(ctx)
	if err != nil {
		return nil, nil, err
	}
	return deploy.ReadRuntime(ctx, api, false)
}

// PlacementFailure explains why a job could not be scheduled anywhere.
func (c *Cluster) PlacementFailure(ctx context.Context, jobID string) string {
	api, err := c.client(ctx)
	if err != nil {
		return ""
	}
	return deploy.ReadPlacement(ctx, api, jobID)
}

// serviceAddr resolves one of orca's own services from the catalog, as
// host:port, reporting false for one that is not registered.
//
// Not a constant: a store binds the container bridge on one machine and the
// private network on several, and moves with the machine it is pinned to. The
// catalog is the only thing that knows where it actually is.
func (c *Cluster) serviceAddr(ctx context.Context, name string) (string, bool, error) {
	api, err := c.client(ctx)
	if err != nil {
		return "", false, err
	}
	regs, _, err := api.Services().Get(deploy.CatalogName(manifest.ReservedGroup, name), (&nomad.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return "", false, err
	}
	if len(regs) == 0 {
		return "", false, nil
	}
	return net.JoinHostPort(regs[0].Address, strconv.Itoa(regs[0].Port)), true, nil
}

// get makes one request to an address on the machine and returns the answer
// once it is known to be a success.
func (c *Cluster) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

// StatusSummary reads the status page's summary. From the machine, like the
// log store, so `orca top` depends on neither ingress nor a password, and
// works when the front door is what is broken.
func (c *Cluster) StatusSummary(ctx context.Context) (statuspage.Summary, error) {
	var s statuspage.Summary
	addr, ok, err := c.serviceAddr(ctx, "status")
	if err != nil {
		return s, fmt.Errorf("read the status page: %w", err)
	}
	if !ok {
		return s, errors.New("the status page is not running: `orca apply orca` starts it, and `orca status orca` says why it is not")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := c.get(ctx, "http://"+addr+"/api/summary")
	if err != nil {
		return s, fmt.Errorf("read the status page: %w", err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return s, fmt.Errorf("read the status page: %w", err)
	}
	return s, nil
}

// logStore is the log store's address.
func (c *Cluster) logStore(ctx context.Context) (string, error) {
	addr, ok, err := c.serviceAddr(ctx, "victorialogs")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("the log store is not registered; is the logs capability enabled and healthy?")
	}
	return addr, nil
}

// logLines hands each line of a log store's answer to fn.
func logLines(r io.Reader, fn func([]byte)) error {
	scanner := bufio.NewScanner(r)
	// A log line can be long; the default 64K token limit would truncate a
	// stack trace mid-way and look like corruption.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if line := bytes.TrimSpace(scanner.Bytes()); len(line) > 0 {
			fn(line)
		}
	}
	return scanner.Err()
}

// QueryLogs runs one LogsQL query and hands each line to fn.
func (c *Cluster) QueryLogs(ctx context.Context, query string, limit int, fn func([]byte)) error {
	addr, err := c.logStore(ctx)
	if err != nil {
		return fmt.Errorf("query logs: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	resp, err := c.get(ctx, "http://"+addr+logsQueryURL("/select/logsql/query", query, limit))
	if err != nil {
		return fmt.Errorf("query logs: %w", err)
	}
	defer resp.Body.Close()

	// Newest first from the store; printed oldest first, because a log read
	// top to bottom is a story and backwards it is a puzzle.
	var lines [][]byte
	if err := logLines(resp.Body, func(line []byte) { lines = append(lines, bytes.Clone(line)) }); err != nil {
		return fmt.Errorf("query logs: %w", err)
	}
	for _, line := range slices.Backward(lines) {
		fn(line)
	}
	return nil
}

// TailLogs streams matching lines until the context is cancelled.
func (c *Cluster) TailLogs(ctx context.Context, query string, fn func([]byte)) error {
	addr, err := c.logStore(ctx)
	if err != nil {
		return fmt.Errorf("tail logs: %w", err)
	}
	resp, err := c.get(ctx, "http://"+addr+logsQueryURL("/select/logsql/tail", query, 0))
	if err != nil {
		return fmt.Errorf("tail logs: %w", err)
	}
	defer resp.Body.Close()

	err = logLines(resp.Body, fn)
	// A tail ends when you stop it, which is not a failure.
	if ctx.Err() != nil {
		return nil
	}
	// A read that failed part way through is a different thing from a tail
	// that ended: the stream stopped for a reason nobody asked for, and
	// without this it is reported as a quiet service.
	if err != nil {
		return fmt.Errorf("tail logs: %w", err)
	}
	return nil
}
