package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os/exec"
	"path"
	"slices"
	"sort"
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

// Stop stops a job. purge additionally removes it from Nomad's state; it never
// touches data on disk, which only `orca purge` does.
func (c *Cluster) Stop(ctx context.Context, jobID string, purge bool) error {
	api, err := c.client(ctx)
	if err != nil {
		return err
	}
	if _, _, err := api.Jobs().Deregister(jobID, purge, (&nomad.WriteOptions{}).WithContext(ctx)); err != nil {
		return fmt.Errorf("stop %s: %w", jobID, err)
	}
	return nil
}

// VolumeDir is a volume directory and the user that must own it.
type VolumeDir struct {
	Path  string
	Owner int

	// Node is the machine the volume lives on, empty when placement is left
	// to Nomad.
	Node string

	// VersionFile is a file in it naming the version its data was written
	// by, empty when it has none. See manifest.TemplateSpec.VersionFile.
	VersionFile string
}

// VolumeFacts is what a volume directory holds, as far as deploying over it
// is concerned.
type VolumeFacts struct {
	// HasData is a directory that exists and is not empty.
	HasData bool

	// Version is its VersionFile's contents, empty without one.
	Version string

	// Used is how much it holds, in bytes; -1 when it was not measured or
	// the measuring took too long.
	Used int64
}

// InspectVolumes reports what each volume directory on this machine holds,
// keyed by path. It only reads. measure adds how much each one holds, which
// walks every file in it, so it is asked for only where it is shown.
func (c *Cluster) InspectVolumes(ctx context.Context, dirs []VolumeDir, measure bool) (map[string]VolumeFacts, error) {
	if len(dirs) == 0 {
		return map[string]VolumeFacts{}, nil
	}
	out, err := c.node.RunOutput(ctx, inspectVolumesScript(dirs, measure))
	if err != nil {
		return nil, fmt.Errorf("inspect volumes: %w", err)
	}
	return parseVolumeFacts(out), nil
}

// inspectVolumesScript prints one line per directory:
// <path> TAB <has data 0|1> TAB <used KiB, or -1> TAB <version>.
func inspectVolumesScript(dirs []VolumeDir, measure bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, `inspect() {
  data=0; used=-1; ver=
  if [ -d "$1" ] && [ -n "$(ls -A "$1" 2>/dev/null)" ]; then
    data=1
    [ -n "$2" ] && ver=$(head -c 64 "$1/$2" 2>/dev/null | tr -d '\t\n' || true)
    if [ %t = true ]; then used=$(timeout 10 du -sk "$1" 2>/dev/null | cut -f1) || used=-1; fi
  fi
  printf '%%s\t%%s\t%%s\t%%s\n' "$1" "$data" "${used:--1}" "$ver"
}
`, measure)
	for _, d := range dirs {
		fmt.Fprintf(&b, "inspect %s %s\n", shQuote(d.Path), shQuote(d.VersionFile))
	}
	return b.String()
}

func parseVolumeFacts(out string) map[string]VolumeFacts {
	facts := map[string]VolumeFacts{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(line, "\t", 4)
		if len(f) < 3 {
			continue
		}
		used, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || used < 0 {
			used = -1
		} else {
			used *= 1024
		}
		v := VolumeFacts{HasData: f[1] == "1", Used: used}
		if len(f) == 4 {
			v.Version = strings.TrimSpace(f[3])
		}
		facts[f[0]] = v
	}
	return facts
}

// EnsureDirs creates host directories for volumes before the jobs that bind
// them start.
//
// Docker would create a missing bind-mount source itself, but as root and at
// whatever path was asked for. Creating them deliberately keeps every byte
// orca writes under the data directory. The ownership matters for the same
// reason: a bind mount keeps the host's ownership, so an image that drops
// privileges cannot write to a directory created as root, and fails at
// startup with a permission error that says nothing about volumes.
//
// Only a directory orca has just created is given an owner. Changing the
// ownership of one that already holds data would be a surprising thing to do
// to a database.
func (c *Cluster) EnsureDirs(ctx context.Context, dirs []VolumeDir) error {
	if len(dirs) == 0 {
		return nil
	}
	if err := c.node.RunQuiet(ctx, ensureDirsScript(dirs)); err != nil {
		return fmt.Errorf("create volume directories: %w", err)
	}
	return nil
}

// ensureDirsScript is EnsureDirs' script, separate so it can be asserted on
// without a machine.
func ensureDirsScript(dirs []VolumeDir) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, d := range dirs {
		fmt.Fprintf(&b, "if [ ! -d %s ]; then\n", shQuote(d.Path))
		fmt.Fprintf(&b, "  mkdir -p %s\n", shQuote(d.Path))
		if d.Owner != 0 {
			fmt.Fprintf(&b, "  chown %d:%d %s\n", d.Owner, d.Owner, shQuote(d.Path))
		}
		b.WriteString("fi\n")
	}
	return b.String()
}

// RunStdin runs a remote command with data on its stdin.
//
// Both streams are captured rather than passed through, so what a command
// narrates does not bury orca's own one-line-per-change output. On failure
// everything it said is included in the error, which is the moment it is
// worth reading.
func (n Node) RunStdin(ctx context.Context, cmd string, stdin []byte) (string, error) {
	var out, errOut bytes.Buffer
	c := exec.CommandContext(ctx, "ssh", sshArgsStdin(n.Host, cmd)...)
	c.Stdin = bytes.NewReader(stdin)
	c.Stdout = &out
	c.Stderr = &errOut
	if err := c.Run(); err != nil {
		return out.String(), fmt.Errorf("ssh %s: %w: %s", n.Host, err, strings.TrimSpace(errOut.String()))
	}
	return out.String(), nil
}

// RunQuiet runs a remote command, discarding its output unless it fails.
func (n Node) RunQuiet(ctx context.Context, cmd string) error {
	var out, errOut bytes.Buffer
	c := exec.CommandContext(ctx, "ssh", sshArgs(n.Host, cmd)...)
	c.Stdout = &out
	c.Stderr = &errOut
	if err := c.Run(); err != nil {
		return fmt.Errorf("ssh %s: %w: %s", n.Host, err, strings.TrimSpace(out.String()+" "+errOut.String()))
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

// store is the cluster's variable store.
func (c *Cluster) store(ctx context.Context) (varStore, error) {
	api, err := c.client(ctx)
	return varStore{api: api}, err
}

// variablePaths lists the variables under a prefix, by path.
//
// It fails closed. An unreachable Nomad must not read as "nothing is set",
// because the caller that decides whether to generate a database password
// acts on exactly that answer.
func (c *Cluster) variablePaths(ctx context.Context, prefix string) ([]string, error) {
	store, err := c.store(ctx)
	if err != nil {
		return nil, err
	}
	return store.list(ctx, prefix)
}

// SecretPaths lists the variable paths orca owns, as a set.
func (c *Cluster) SecretPaths(ctx context.Context) (map[string]bool, error) {
	paths, err := c.variablePaths(ctx, deploy.SecretPrefix+"/")
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return set, nil
}

// PutSecret writes one secret into Nomad's variable store.
func (c *Cluster) PutSecret(ctx context.Context, group, name, value string) error {
	if err := c.putVariable(ctx, deploy.SecretPath(group, name), map[string]string{deploy.SecretItemKey: value}); err != nil {
		return fmt.Errorf("set secret %s/%s: %w", group, name, err)
	}
	return nil
}

// putVariable writes a variable, replacing any there.
func (c *Cluster) putVariable(ctx context.Context, path string, items map[string]string) error {
	store, err := c.store(ctx)
	if err != nil {
		return err
	}
	return store.put(ctx, path, items)
}

// CreateSecret writes a secret only if it does not exist yet, and reports
// whether it did, which is what a generated password depends on: made once,
// never replaced.
func (c *Cluster) CreateSecret(ctx context.Context, group, name, value string) (bool, error) {
	created, err := c.createVariable(ctx, deploy.SecretPath(group, name), deploy.SecretItemKey, value)
	if err != nil {
		return false, fmt.Errorf("create secret %s/%s: %w", group, name, err)
	}
	return created, nil
}

// createVariable writes a one-item variable only if it does not exist yet, and
// reports whether it did.
func (c *Cluster) createVariable(ctx context.Context, path, key, value string) (bool, error) {
	store, err := c.store(ctx)
	if err != nil {
		return false, err
	}
	return store.putChecked(ctx, nomad.Variable{Path: path, Items: map[string]string{key: value}})
}

// variablePrefixes are everything orca keeps in the store that is a secret of
// some kind: what a manifest references, the registry logins, and the
// dashboard password. The apply lock is the one thing left out.
var variablePrefixes = []string{deploy.SecretPrefix + "/", deploy.RegistryPrefix + "/", deploy.AdminPasswordPath, deploy.CertPrefix + "/"}

// Variables reads every secret the cluster holds, values included. This is
// the only call that pulls plaintext off the machine wholesale, and exporting
// or editing the cluster's secrets is the only reason to make it.
func (c *Cluster) Variables(ctx context.Context) (variables, error) {
	vars, err := c.readVariables(ctx, variablePrefixes)
	if err != nil {
		return nil, fmt.Errorf("read the cluster's secrets: %w", err)
	}
	return vars, nil
}

// Certificates reads every certificate record, without its private key: what
// there is to know about a certificate (who asked, when it expires, why it
// failed) is all in the rest.
func (c *Cluster) Certificates(ctx context.Context) (variables, error) {
	vars, err := c.readVariables(ctx, []string{deploy.CertPrefix + "/"})
	if err != nil {
		return nil, fmt.Errorf("read the cluster's certificates: %w", err)
	}
	for _, items := range vars {
		delete(items, deploy.CertKeyKey)
	}
	return vars, nil
}

func (c *Cluster) readVariables(ctx context.Context, prefixes []string) (variables, error) {
	api, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	return deploy.ReadVariables(ctx, api, prefixes)
}

// PutVariables writes a set of variables, replacing any there.
//
// It stops at the first failure and names it: what was written before it
// stays written, and running the same import again writes the rest.
func (c *Cluster) PutVariables(ctx context.Context, vars variables) error {
	for _, path := range slices.Sorted(maps.Keys(vars)) {
		if err := c.putVariable(ctx, path, vars[path]); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// readVariable returns one item of a variable, and whether the variable
// exists. Missing is an answer, not an error; a Nomad that cannot be asked is.
func (c *Cluster) readVariable(ctx context.Context, path, key string) (string, bool, error) {
	store, err := c.store(ctx)
	if err != nil {
		return "", false, err
	}
	v, ok, err := store.get(ctx, path)
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	return v.Items[key], ok, nil
}

// AdminPassword is the dashboards' generated password, and whether one has
// been generated.
func (c *Cluster) AdminPassword(ctx context.Context) (string, bool, error) {
	v, ok, err := c.readVariable(ctx, deploy.AdminPasswordPath, deploy.AdminPasswordKey)
	if err != nil {
		return "", false, fmt.Errorf("read the dashboard password: %w", err)
	}
	return v, ok, nil
}

// PutAdminPassword replaces the dashboards' password.
func (c *Cluster) PutAdminPassword(ctx context.Context, value string) error {
	if err := c.putVariable(ctx, deploy.AdminPasswordPath, map[string]string{deploy.AdminPasswordKey: value}); err != nil {
		return fmt.Errorf("store the dashboard password: %w", err)
	}
	return nil
}

// CreateAdminPassword stores the dashboards' password unless one is already
// stored, and reports whether it did.
func (c *Cluster) CreateAdminPassword(ctx context.Context, value string) (bool, error) {
	created, err := c.createVariable(ctx, deploy.AdminPasswordPath, deploy.AdminPasswordKey, value)
	if err != nil {
		return false, fmt.Errorf("store the dashboard password: %w", err)
	}
	return created, nil
}

// deleteVariable removes one variable.
func (c *Cluster) deleteVariable(ctx context.Context, path string) error {
	store, err := c.store(ctx)
	if err != nil {
		return err
	}
	return store.delete(ctx, path)
}

// DeleteSecret removes one secret.
func (c *Cluster) DeleteSecret(ctx context.Context, group, name string) error {
	if err := c.deleteVariable(ctx, deploy.SecretPath(group, name)); err != nil {
		return fmt.Errorf("remove secret %s/%s: %w", group, name, err)
	}
	return nil
}

// RegistryHosts lists the registries the cluster holds credentials for.
//
// The host is in the path, so finding out which registries are logged in to
// never pulls a token off the machine.
func (c *Cluster) RegistryHosts(ctx context.Context) (map[string]bool, error) {
	paths, err := c.variablePaths(ctx, deploy.RegistryPrefix+"/")
	if err != nil {
		return nil, fmt.Errorf("list registry credentials: %w", err)
	}
	hosts := map[string]bool{}
	for _, p := range paths {
		if host, ok := deploy.RegistryHost(p); ok {
			hosts[host] = true
		}
	}
	return hosts, nil
}

// The items of a registry's variable. The credential helper on each machine
// reads them by these names.
const (
	registryUsernameKey = "username"
	registryPasswordKey = "password"
)

func registryItems(username, password string) map[string]string {
	return map[string]string{registryUsernameKey: username, registryPasswordKey: password}
}

// PutRegistry stores the credentials for one registry, replacing any there.
func (c *Cluster) PutRegistry(ctx context.Context, host, username, password string) error {
	if err := c.putVariable(ctx, deploy.RegistryPath(host), registryItems(username, password)); err != nil {
		return fmt.Errorf("store credentials for %s: %w", host, err)
	}
	return nil
}

// DeleteRegistry removes one registry's credentials.
func (c *Cluster) DeleteRegistry(ctx context.Context, host string) error {
	if err := c.deleteVariable(ctx, deploy.RegistryPath(host)); err != nil {
		return fmt.Errorf("remove credentials for %s: %w", host, err)
	}
	return nil
}

// HasCredentialHelper reports whether this machine's Nomad will use the
// registry credential helper: the helper installed, and the config naming it.
// Either alone pulls anonymously, which is what a machine bootstrapped by an
// older orca does until it is bootstrapped again.
func (c *Cluster) HasCredentialHelper(ctx context.Context) (bool, error) {
	const script = `if command -v docker-credential-orca >/dev/null && grep -q 'helper *= *"orca"' /etc/nomad.d/nomad.hcl; then echo yes; else echo no; fi`
	out, err := c.node.RunOutput(ctx, script)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "yes", nil
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

// Volume identifies one service's volume on disk.
type Volume struct {
	Group   string
	Service string
}

// Path is where the volume lives on its machine.
func (v Volume) Path() string { return deploy.VolumePath(dataDir, v.Group, v.Service) }

// VolumeListing is every service volume one machine holds.
type VolumeListing struct {
	Volumes []Volume
}

// ListVolumes lists the volumes on this machine.
//
// Listing the one directory everything lives under is what stops "kept" from
// meaning "invisible": data whose service was removed is found by looking,
// so nothing has to be remembered for it to be findable later.
func (c *Cluster) ListVolumes(ctx context.Context) (VolumeListing, error) {
	script := fmt.Sprintf(`set -e
if [ -d %[1]s ]; then
  cd %[1]s && find . -mindepth 2 -maxdepth 2 -type d | sed 's|^\./||'
fi`, shQuote(deploy.VolumeRoot(dataDir)))

	out, err := c.node.RunOutput(ctx, script)
	if err != nil {
		return VolumeListing{}, fmt.Errorf("list volumes: %w", err)
	}
	return parseVolumeListing(out), nil
}

func parseVolumeListing(out string) VolumeListing {
	var l VolumeListing
	for _, line := range strings.Split(out, "\n") {
		g, s, ok := strings.Cut(strings.TrimSpace(line), "/")
		if ok && g != "" && s != "" {
			l.Volumes = append(l.Volumes, Volume{Group: g, Service: s})
		}
	}
	sort.Slice(l.Volumes, func(i, j int) bool {
		if l.Volumes[i].Group != l.Volumes[j].Group {
			return l.Volumes[i].Group < l.Volumes[j].Group
		}
		return l.Volumes[i].Service < l.Volumes[j].Service
	})
	return l
}

// validVolumePart is the guard in front of an rm -rf.
//
// A group or service is one directory name, never a path. The inputs come from
// a listing of the volume root, so this can only fire on a bug, which is
// exactly when a check in front of a recursive delete earns its place.
func validVolumePart(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("refusing to delete a volume with an empty name")
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("refusing to delete volume %q: a name is one directory, not a path", name)
	case strings.Contains(name, ".."):
		return fmt.Errorf("refusing to delete volume %q: contains ..", name)
	case strings.HasPrefix(name, "-"):
		return fmt.Errorf("refusing to delete volume %q: would be read as a flag", name)
	}
	return nil
}

// DeleteVolumes permanently removes volumes, then the group directories they
// leave empty.
func (c *Cluster) DeleteVolumes(ctx context.Context, vols []Volume) error {
	script, err := deleteVolumesScript(vols)
	if err != nil {
		return err
	}
	if err := c.node.RunQuiet(ctx, script); err != nil {
		return fmt.Errorf("delete volumes: %w", err)
	}
	return nil
}

func deleteVolumesScript(vols []Volume) (string, error) {
	var b strings.Builder
	b.WriteString("set -e\n")
	groups := map[string]bool{}
	for _, v := range vols {
		for _, part := range []string{v.Group, v.Service} {
			if err := validVolumePart(part); err != nil {
				return "", err
			}
		}
		fmt.Fprintf(&b, "rm -rf %s\n", shQuote(v.Path()))
		groups[v.Group] = true
	}
	for g := range groups {
		fmt.Fprintf(&b, "rmdir --ignore-fail-on-non-empty %s\n", shQuote(path.Join(deploy.VolumeRoot(dataDir), g)))
	}
	return b.String(), nil
}

// hostNomad starts every script that asks Nomad something from on the
// machine, where the work is next to the data and its secrets need not
// leave: a restore, mostly. It reads the cluster's token for Nomad's own CLI,
// and defines nomad_get, which reads one API path.
//
// The token goes to curl on its stdin, not in its arguments: an argument list
// is visible in the process table for as long as the command runs.
const hostNomad = `NOMAD_TOKEN=$(cat ` + tokenPath + `)
export NOMAD_TOKEN
nomad_get() { printf 'X-Nomad-Token: %s\n' "$NOMAD_TOKEN" | curl -sf --max-time 10 -H @- "` + nomadAddr + `$1"; }
`

// rcloneEnvScript writes the S3 credentials to a private file on the machine
// and prints its path.
//
// A file rather than command arguments, for the reason the token is not one.
// The file is created with a restrictive mode and removed by the caller.
func rcloneEnvScript(spec deploy.BackupSpec) string {
	return hostNomad + fmt.Sprintf(`ENVFILE=$(mktemp)
chmod 600 "$ENVFILE"
S3_ENDPOINT=%[4]s
S3_REGION=%[5]s
KEY=$(nomad_get /v1/var/%[1]s | jq -r '.Items.value')
SEC=$(nomad_get /v1/var/%[2]s | jq -r '.Items.value')
if [ -z "$KEY" ] || [ "$KEY" = "null" ] || [ -z "$SEC" ] || [ "$SEC" = "null" ]; then
  echo "backup credentials are not set; run: orca secret set %[3]s" >&2
  rm -f "$ENVFILE"; exit 1
fi
{
  echo "RCLONE_CONFIG_STORE_TYPE=s3"
  echo "RCLONE_CONFIG_STORE_PROVIDER=Other"
  echo "RCLONE_CONFIG_STORE_ENDPOINT=$S3_ENDPOINT"
  echo "RCLONE_CONFIG_STORE_REGION=$S3_REGION"
  echo "RCLONE_CONFIG_STORE_NO_CHECK_BUCKET=true"
  echo "RCLONE_CONFIG_STORE_ACCESS_KEY_ID=$KEY"
  echo "RCLONE_CONFIG_STORE_SECRET_ACCESS_KEY=$SEC"
} > "$ENVFILE"`,
		deploy.SecretPath(spec.SecretGroup, spec.KeyIDSecret),
		deploy.SecretPath(spec.SecretGroup, spec.SecretKeySecret),
		// The group the credentials actually live in, so the message sends
		// whoever reads it to a path that exists.
		spec.SecretGroup+"/"+spec.KeyIDSecret,
		shQuote(spec.Endpoint), shQuote(spec.Region))
}

// shQuote renders a value as one POSIX shell word, safe to interpolate into a
// script.
//
// Every value orca puts in a remote script goes through this or is built from
// a name the manifest validator already constrained. The one that needs it
// most is a backup's filename: it comes from listing the bucket, so it is
// remote input. Inside a double-quoted string $(...) still runs, so writing a
// file into the backup bucket would be command execution as root on the
// machine, taken at the moment someone runs a restore, when things are
// already going badly.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ListBackups returns the backups held for one database, oldest first.
func (c *Cluster) ListBackups(ctx context.Context, spec deploy.BackupSpec) ([]string, error) {
	// Every non-zero exit is an error, including 3.
	//
	// Checked against rclone rather than assumed: listing an empty prefix in a
	// bucket that exists exits 0 with no output, a missing bucket exits 3, and
	// bad credentials exit 1. So "no backups yet" is exactly the empty exit-0
	// case and nothing else. Treating 3 as empty too would answer "no backups
	// yet for shop/db" to someone whose bucket name is wrong: the
	// reassuring-but-false answer this command exists to avoid giving.
	//
	// Only what rclone prints as the listing is the listing. Everything said
	// on the way to it is kept apart and shown only when it failed: Docker
	// pulling the image the first time, and rclone's own notices, would
	// otherwise each be read as the name of a backup.
	script := fmt.Sprintf(`set -eu
%[1]s
BUCKET=%[3]s
PREFIX=%[4]s
ERR=$(mktemp)
set +e
OUT=$(docker run --rm --network host --env-file "$ENVFILE" %[2]s lsf "store:$BUCKET/$PREFIX/" 2>"$ERR")
CODE=$?
set -e
rm -f "$ENVFILE"
if [ $CODE -ne 0 ]; then
  cat "$ERR" >&2
  rm -f "$ERR"
  exit $CODE
fi
rm -f "$ERR"
printf '%%s\n' "$OUT"`,
		rcloneEnvScript(spec), shQuote(spec.Image), shQuote(spec.Bucket), shQuote(spec.Prefix))

	out, err := c.node.RunOutput(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("list backups in %s/%s: %w", spec.Bucket, spec.Prefix, err)
	}

	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if n := strings.TrimSpace(line); n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// RestoreBackup downloads a backup and loads it into the running database.
//
// It runs where the database is and reaches it the same way anything else
// does, so a restore exercises the same path a normal connection takes rather
// than a special one that only works when someone is watching.
func (c *Cluster) RestoreBackup(ctx context.Context, spec deploy.BackupSpec, group, service, name, pgImage string) error {
	script := restoreScript(spec, group, service, name, pgImage)

	// Streamed, not buffered: a multi-gigabyte download followed by a restore
	// prints nothing for minutes otherwise, and a working restore is
	// indistinguishable from a hang.
	if err := c.node.Run(ctx, script); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	fmt.Printf("restored %s into %s/%s\n", name, group, service)
	return nil
}

// restoreScript builds the restore, separately from running it.
//
// Split out so the most dangerous string orca produces can be asserted on
// without a machine: it interpolates a filename that came from listing a
// bucket, which is the one input here that someone else can choose.
func restoreScript(spec deploy.BackupSpec, group, service, name, pgImage string) string {
	return fmt.Sprintf(`set -eu
%[1]s
BUCKET=%[3]s
PREFIX=%[4]s
NAME=%[6]s

WORK=$(mktemp -d)
PGENV=$(mktemp)
chmod 600 "$PGENV"
trap 'rm -rf "$WORK" "$ENVFILE" "$PGENV"' EXIT
chmod 755 "$WORK"

echo "downloading $NAME"
docker run --rm --network host --env-file "$ENVFILE" -v "$WORK:/work" %[2]s \
  copyto "store:$BUCKET/$PREFIX/$NAME" "/work/$NAME"

# Host and port both come from the catalog. Assuming 5432 is right only while
# the database registers its own address; once a port is published instead,
# the catalog carries a dynamic one and the assumption silently connects to
# nothing.
SVC=$(nomad_get /v1/service/%[8]s)
DBHOST=$(printf '%%s' "$SVC" | jq -r '.[0].Address // empty')
DBPORT=$(printf '%%s' "$SVC" | jq -r '.[0].Port // empty')
if [ -z "$DBHOST" ] || [ -z "$DBPORT" ]; then
  echo "database %[8]s is not registered; is it running?" >&2
  exit 1
fi

# The connection goes in a file, not the argument list, which is visible in the
# host's process table for as long as the restore runs.
{
  printf 'PGPASSWORD=%%s\n' "$(nomad_get /v1/var/%[7]s | jq -r '.Items.value')"
  printf 'PGHOST=%%s\nPGPORT=%%s\nPGUSER=postgres\n' "$DBHOST" "$DBPORT"
} > "$PGENV"

cat > "$WORK/restore.sh" <<'RESTORE'
%[9]s
RESTORE

echo "restoring into $DBHOST:$DBPORT"
docker run --rm --network host -v "$WORK:/work" --env-file "$PGENV" \
  -e NAME="$NAME" -e STAMP="$(date -u +%%Y%%m%%dT%%H%%M%%SZ)" %[5]s sh /work/restore.sh`,
		rcloneEnvScript(spec), shQuote(spec.Image), shQuote(spec.Bucket), shQuote(spec.Prefix),
		shQuote(pgImage), shQuote(name),
		deploy.SecretPath(group, manifest.GeneratedSecret(service, manifest.PasswordSuffix)),
		deploy.CatalogName(group, service),
		pgRestoreScript)
}

// pgRestoreScript runs inside the database's own image, next to the
// downloaded backup in /work, and loads it one database at a time.
//
// Each is restored into a new database first, and only once that has
// succeeded does it take the real one's name, the real one being renamed to
// <name>_before_restore_<time> and kept on the server. So a restore that fails
// part way leaves every database as it was, one that succeeds leaves each
// exactly as the backup had it (not the backup laid over whatever was there),
// and what it replaced is one rename away.
//
// Database names come from the files in the backup, so they are only ever
// handed to psql as variables, which quotes them, never pasted into SQL.
const pgRestoreScript = `set -eu
cd /work
SUFFIX=` + deploy.PreRestoreSuffix + `
# Postgres narrates every IF EXISTS that did not; only warnings are news here.
export PGOPTIONS='-c client_min_messages=warning'

sql() { psql -X -q -v ON_ERROR_STOP=1 -d template1 "$@"; }

case "$NAME" in
  *.tar)
    mkdir x
    tar -xf "$NAME" -C x
    # Roles first, so owners and grants have someone to belong to. One that
    # exists already is only reported, and keeps what it has.
    if [ -f x/globals.sql ]; then
      psql -X -q -d template1 -f x/globals.sql 2>&1 | grep -v 'already exists' || true
    fi
    OPTS=""
    ;;
  *)
    # A single dump of the postgres database, from before backups carried
    # roles: restored without owners and grants, which may name roles this
    # server does not have.
    mkdir x
    mv "$NAME" x/postgres.pgc
    OPTS="--no-owner --no-privileges"
    ;;
esac

# Every database is loaded before any is swapped in, so a backup that fails
# to load leaves the server as it was rather than half restored. Each loads
# into a new database created from template0, as pg_restore expects, which
# also works while this script is connected to template1.
scratch() { echo "orca_restore_$1_$STAMP"; }
N=0
discard() {
  i=1
  while [ "$i" -le "$N" ]; do
    printf 'DROP DATABASE IF EXISTS :"new";\n' | sql -v new="$(scratch "$i")" || true
    i=$((i + 1))
  done
}

for f in x/*.pgc; do
  [ -f "$f" ] || { echo "$NAME holds no databases" >&2; exit 1; }
  N=$((N + 1))
  NEW=$(scratch "$N")
  echo "loading $(basename "$f" .pgc)"
  printf 'DROP DATABASE IF EXISTS :"new";\nCREATE DATABASE :"new" TEMPLATE template0;\n' | sql -v new="$NEW"
  if ! pg_restore --single-transaction $OPTS -d "$NEW" "$f"; then
    discard
    echo "loading $(basename "$f" .pgc) failed; nothing was changed" >&2
    exit 1
  fi
done

N=0
for f in x/*.pgc; do
  N=$((N + 1))
  NEW=$(scratch "$N")
  DB=$(basename "$f" .pgc)
  # Postgres cuts a name at 63 bytes, so the database's own name is what gives
  # way: cutting the suffix could make two databases' copies one name.
  OLD="$(printf '%.30s' "$DB")$SUFFIX$STAMP"

  # The swap. New connections are refused and open ones ended first, or the
  # rename is refused; the applications reconnect to the restored database.
  # Both renames are one transaction, so the name never points at nothing.
  if ! sql -v db="$DB" -v old="$OLD" -v new="$NEW" <<'SQL'
SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = :'db') AS exists \gset
\if :exists
  ALTER DATABASE :"db" WITH ALLOW_CONNECTIONS false;
  SELECT count(pg_terminate_backend(pid, 10000)) FROM pg_stat_activity WHERE datname = :'db' \g /dev/null
\endif
BEGIN;
\if :exists
  ALTER DATABASE :"db" RENAME TO :"old";
\endif
ALTER DATABASE :"new" RENAME TO :"db";
COMMIT;
\if :exists
  \echo '  the database it replaced is kept, closed to connections, as' :old
\endif
SQL
  then
    printf 'ALTER DATABASE :"db" WITH ALLOW_CONNECTIONS true;\n' | sql -v db="$DB" >/dev/null 2>&1 || true
    discard
    echo "swapping in the restored $DB failed; it is as it was, and so is every database after it" >&2
    exit 1
  fi
  echo "restored $DB"
done`

// RestoreRedis replaces a Redis service's data with a backup.
//
// Unlike Postgres there is no loading a dump into a running server, so this
// is the one restore with downtime: the service is stopped, its data swapped,
// and started again. It runs on the machine that holds the volume, which is
// the one place the data can be swapped.
func (c *Cluster) RestoreRedis(ctx context.Context, spec deploy.BackupSpec, group, service, name, image string) error {
	if err := c.node.Run(ctx, redisRestoreScript(spec, group, service, name, image)); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	fmt.Printf("restored %s into %s/%s\n", name, group, service)
	return nil
}

// preRestorePrefix is where a restore leaves the data it replaced, suffixed
// with the time of the restore. Outside the volume root, so it is never
// mistaken for a volume of its own. It is kept rather than deleted, because
// deleting data is purge's job and nothing else's.
func preRestorePrefix(group, service string) string {
	return path.Join(dataDir, "pre-restore", group+"-"+service)
}

// redisRestoreScript builds the Redis restore, separately from running it, for
// the reason restoreScript is: the backup name is remote input.
//
// A snapshot cannot simply be dropped into the volume. With appendonly on,
// Redis loads the append-only file and ignores dump.rdb; finding none, it
// starts empty and writes an empty one, so the restore would appear to work
// and hold nothing. So the snapshot is loaded by a throwaway server with the
// AOF off, which is then switched on: that rewrites the AOF from the loaded
// data, and the service starts from it as it always does.
//
// If anything fails once the service is down, the previous data is put back
// and the service started again, so a failed restore costs a restart rather
// than the database.
func redisRestoreScript(spec deploy.BackupSpec, group, service, name, image string) string {
	return fmt.Sprintf(`set -eu
%[1]s
BUCKET=%[3]s
PREFIX=%[4]s
NAME=%[6]s
IMAGE=%[5]s
JOB=%[7]s
DIR=%[8]s
STAMP=$(date -u +%%Y%%m%%dT%%H%%M%%SZ)
KEEP=%[9]s-$STAMP
TMP=orca-restore-$JOB

WORK=$(mktemp -d)
SPEC=$(mktemp)
STOPPED=0
SWAPPED=0
DONE=0

cleanup() {
  docker rm -f "$TMP" >/dev/null 2>&1 || true
  if [ "$DONE" = 0 ] && [ "$SWAPPED" = 1 ]; then
    echo "restore failed; putting the previous data back" >&2
    rm -rf "$DIR" && mv "$KEEP" "$DIR"
  fi
  if [ "$DONE" = 0 ] && [ "$STOPPED" = 1 ]; then
    echo "starting $JOB again" >&2
    nomad job run -detach -json "$SPEC" >/dev/null || echo "could not start $JOB; run orca apply" >&2
  fi
  rm -rf "$WORK" "$ENVFILE" "$SPEC"
}
trap cleanup EXIT
chmod 755 "$WORK"

echo "downloading $NAME"
docker run --rm --network host --env-file "$ENVFILE" -v "$WORK:/work" %[2]s \
  copyto "store:$BUCKET/$PREFIX/$NAME" "/work/dump.rdb"

# The job exactly as it runs now, to start it again as it was.
nomad job inspect "$JOB" > "$SPEC"

echo "stopping $JOB"
STOPPED=1
nomad job stop -detach "$JOB" >/dev/null
for i in $(seq 1 60); do
  LIVE=$(nomad_get "/v1/job/$JOB/allocations" | jq '[.[] | select(.ClientStatus == "running" or .ClientStatus == "pending")] | length')
  [ "$LIVE" = 0 ] && break
  sleep 2
done
[ "$LIVE" = 0 ] || { echo "$JOB did not stop" >&2; exit 1; }

mkdir -p "$(dirname "$KEEP")"
mv "$DIR" "$KEEP"
SWAPPED=1
mkdir "$DIR"
chown --reference="$KEEP" "$DIR"
cp "$WORK/dump.rdb" "$DIR/dump.rdb"

# No network: nothing but this script can reach it, so it needs no password.
echo "loading $NAME"
docker run -d --name "$TMP" --network none -v "$DIR:/data" "$IMAGE" \
  redis-server --appendonly no --dir /data >/dev/null
until [ "$(docker exec "$TMP" redis-cli PING 2>/dev/null)" = PONG ]; do
  docker inspect -f '{{.State.Running}}' "$TMP" | grep -q true || { docker logs "$TMP" >&2; exit 1; }
  sleep 2
done
KEYS=$(docker exec "$TMP" redis-cli DBSIZE)
echo "loaded $KEYS keys"

docker exec "$TMP" redis-cli CONFIG SET appendonly yes >/dev/null
for i in $(seq 1 300); do
  P=$(docker exec "$TMP" redis-cli INFO persistence | tr -d '\r')
  if printf '%%s\n' "$P" | grep -qx 'aof_rewrite_in_progress:0' &&
     printf '%%s\n' "$P" | grep -qx 'aof_rewrite_scheduled:0' &&
     ls "$DIR"/appendonlydir/*.manifest >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
printf '%%s\n' "$P" | grep -qx 'aof_last_bgrewrite_status:ok' || { echo "writing the append-only file failed" >&2; exit 1; }
docker exec "$TMP" redis-cli SHUTDOWN >/dev/null 2>&1 || true
docker wait "$TMP" >/dev/null
docker rm "$TMP" >/dev/null

echo "starting $JOB"
nomad job run -detach -json "$SPEC" >/dev/null
DONE=1
echo "restored $NAME ($KEYS keys); the data it replaced is at $KEEP"`,
		rcloneEnvScript(spec), shQuote(spec.Image), shQuote(spec.Bucket), shQuote(spec.Prefix),
		shQuote(image), shQuote(name),
		shQuote(deploy.JobID(group, service)),
		shQuote(deploy.VolumePath(dataDir, group, service)),
		shQuote(preRestorePrefix(group, service)))
}
