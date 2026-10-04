package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/pkg/deploy"
	"github.com/unitoftime/orca/pkg/manifest"
	"github.com/unitoftime/orca/pkg/statuspage"
)

// Cluster talks to Nomad by running its CLI on the machine over SSH.
//
// There is no HTTP client here on purpose: Nomad binds loopback on a
// single-machine cluster, so it has no network listener to connect to. SSH is
// the transport, which is also why every operation below is written to need one
// round trip rather than one per job. An SSH round trip is a tenth of a second
// and a CI apply should not spend ten of them discovering that nothing changed.
type Cluster struct {
	node Node
}

func NewCluster(n Node) *Cluster { return &Cluster{node: n} }

// NomadAddr is where Nomad's HTTP API answers on the machine. It is always
// loopback: orca runs commands on the box over SSH rather than connecting to a
// listener, so this does not change when a second machine gives bind_addr a
// real IP.
const NomadAddr = "http://127.0.0.1:4646"

// listJobsScript asks Nomad for every job, keeps the ones orca owns, and
// projects them down to the few fields the plan needs. The status page makes
// the same projection from Nomad's own types, in deploy.JobStateFromNomad; a
// rule changed here must change there too. The filtering happens on the
// machine so the reply stays small.
//
// It fails rather than answering short. Without pipefail an unreachable Nomad
// would produce an empty list and a zero exit, which reads as "nothing is
// deployed": the answer every caller acts on, so status would report an empty
// cluster and apply would plan to create everything. Periodic children are
// left out of the fetch: they are not services, and their IDs carry a slash.
//
// curl rather than `nomad operator api`, which infers a write method when its
// stdin is not a terminal (over SSH it never is) and gets back "Invalid
// method".
const listJobsScript = `set -eo pipefail
IDS=$(curl -sf --max-time 10 ` + NomadAddr + `/v1/jobs | jq -r '.[] | select((.ParentID // "") == "") | .ID')
for id in $IDS; do
  curl -sf --max-time 10 "` + NomadAddr + `/v1/job/$id"
done | jq -s '` + jobsJQ + `'`

// jobsJQ projects a stream of full jobs (read with jq -s).
const jobsJQ = `[ .[]
  | select(.Meta != null and .Meta["orca.managed"] == "true")
  | {ID, Stop, Meta, Version, Count: (.TaskGroups[0].Count // 1),
     System: (.Type == "system"),
     Periodic: (.Periodic != null and (.Periodic.Enabled // false))} ]`

type jobStateWire struct {
	ID       string            `json:"ID"`
	Stop     bool              `json:"Stop"`
	Meta     map[string]string `json:"Meta"`
	Version  uint64            `json:"Version"`
	Count    int               `json:"Count"`
	System   bool              `json:"System"`
	Periodic bool              `json:"Periodic"`
}

// Jobs returns every orca-managed job the cluster knows about, keyed by job ID.
func (c *Cluster) Jobs(ctx context.Context) (map[string]deploy.JobState, error) {
	out, err := c.node.RunOutput(ctx, listJobsScript)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}

	return parseJobs(out)
}

// parseJobs reads listJobsScript's output.
func parseJobs(out string) (map[string]deploy.JobState, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return map[string]deploy.JobState{}, nil
	}

	var wire []jobStateWire
	if err := json.Unmarshal([]byte(out), &wire); err != nil {
		return nil, fmt.Errorf("parse job list: %w", err)
	}

	states := make(map[string]deploy.JobState, len(wire))
	for _, w := range wire {
		// A periodic job's children inherit its metadata, so every past
		// backup run would otherwise appear as a service of its own.
		if deploy.IsPeriodicChild(w.ID) {
			continue
		}
		states[w.ID] = deploy.JobState{
			ID:       w.ID,
			App:      w.Meta[deploy.MetaApp],
			Service:  w.Meta[deploy.MetaService],
			Image:    w.Meta[deploy.MetaImage],
			ImageRef: w.Meta[deploy.MetaImageRef],
			Version:  w.Version,
			Stopped:  w.Stop,
			Count:    w.Count,
			System:   w.System,
			Periodic: w.Periodic,
			AuthHash: w.Meta[deploy.MetaAuth],
		}
	}
	return states, nil
}

// Alive reports whether this machine can answer for the cluster: reachable
// over SSH, with a Nomad agent that knows who the leader is.
func (c *Cluster) Alive(ctx context.Context) bool {
	out, err := c.node.RunOutput(ctx, "curl -sf --max-time 5 "+NomadAddr+"/v1/status/leader")
	return err == nil && strings.Contains(out, ":")
}

// planJobsScript asks Nomad to plan each job on its stdin, one per line as
// "<id> <request>", and prints each answer on a line of its own in the same
// order. One round trip for every job, however many there are.
const planJobsScript = `set -eo pipefail
while read -r id body; do
  if ! out=$(printf '%s' "$body" | curl -sS --fail-with-body --max-time 30 -X POST --data-binary @- "` + NomadAddr + `/v1/job/$id/plan"); then
    echo "plan $id: $out" >&2
    exit 1
  fi
  printf '%s\n' "$out"
done`

// PlanJobs asks Nomad what submitting each job would do, without submitting
// anything. A job Nomad rejects outright fails here, before anything has
// changed, rather than halfway through an apply.
func (c *Cluster) PlanJobs(ctx context.Context, jobs []*nomad.Job) (map[string]deploy.JobPlan, error) {
	var in bytes.Buffer
	for _, j := range jobs {
		body, err := json.Marshal(nomad.JobPlanRequest{Job: j, Diff: true})
		if err != nil {
			return nil, fmt.Errorf("marshal job %s: %w", *j.ID, err)
		}
		fmt.Fprintf(&in, "%s %s\n", *j.ID, body)
	}

	out, err := c.node.RunStdin(ctx, planJobsScript, in.Bytes())
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != len(jobs) {
		return nil, fmt.Errorf("plan: asked about %d jobs and got %d answers", len(jobs), len(lines))
	}
	plans := make(map[string]deploy.JobPlan, len(jobs))
	for i, j := range jobs {
		var r nomad.JobPlanResponse
		if err := json.Unmarshal([]byte(lines[i]), &r); err != nil {
			return nil, fmt.Errorf("plan %s: %w", *j.ID, err)
		}
		plans[*j.ID] = deploy.JobPlanFromNomad(&r)
	}
	return plans, nil
}

// Submit registers a job, provided it is still at modifyIndex, the version its
// plan was made against (zero: provided it does not exist). Anything else
// having changed it since, another apply included, refuses the submit rather
// than overwriting a deploy nobody here has seen.
func (c *Cluster) Submit(ctx context.Context, job *nomad.Job, modifyIndex uint64) error {
	body, err := json.Marshal(nomad.JobRegisterRequest{Job: job, EnforceIndex: true, JobModifyIndex: modifyIndex})
	if err != nil {
		return fmt.Errorf("marshal job %s: %w", *job.ID, err)
	}

	script := fmt.Sprintf(`curl -sS --fail-with-body --max-time 30 -X PUT --data-binary @- %s/v1/job/%s`, NomadAddr, *job.ID)
	out, err := c.node.RunStdin(ctx, script, body)
	if err != nil {
		if strings.Contains(out, "Enforcing job modify index") {
			return fmt.Errorf("submit %s: it changed after it was planned; run apply again", *job.ID)
		}
		return fmt.Errorf("submit %s: %w: %s", *job.ID, err, strings.TrimSpace(out))
	}
	return nil
}

// Stop stops a job. purge additionally removes it from Nomad's state; it never
// touches data on disk, which only `orca purge` does.
func (c *Cluster) Stop(ctx context.Context, jobID string, purge bool) error {
	cmd := "nomad job stop -detach"
	if purge {
		cmd += " -purge"
	}
	if err := c.node.RunQuiet(ctx, cmd+" "+jobID); err != nil {
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
// Both streams are captured rather than passed through: the Nomad CLI narrates
// every action ("==> View this job in the Web UI", evaluation ids) and that
// noise buries orca's own one-line-per-change output. On failure everything it
// said is included in the error, which is the moment it is worth reading.
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

// runtimeScript fetches allocations and deployments in two calls, regardless of
// how many jobs exist. deploy.AllocStateFromNomad and DeploymentStateFromNomad
// are the same projections, for the status page. The projections happen on the
// machine because an allocation's raw event list is kilobytes per task and
// only its last line is wanted.
const runtimeScript = `set -eo pipefail
printf '{"allocs":'
curl -sf --max-time 10 ` + NomadAddr + `/v1/allocations | jq -c '` + allocsJQ + `'
printf ',"deployments":'
curl -sf --max-time 10 ` + NomadAddr + `/v1/deployments | jq -c '` + deploymentsJQ + `'
printf '}'`

// allocsJQ projects Nomad's allocation list.
const allocsJQ = `[ .[] | {
  ID, JobID, JobVersion, ClientStatus, DesiredStatus, NodeName, CreateTime,
  Tasks: ((.TaskStates // {}) | to_entries | map({
    Name: .key,
    State: .value.State,
    Failed: .value.Failed,
    Restarts: .value.Restarts,
    StartedAt: .value.StartedAt,
    LastRestart: .value.LastRestart,
    Last: ((.value.Events // []) | last | .DisplayMessage // ""),
    Fail: ((.value.Events // []) | map(select(.Type == "Terminated" or .Type == "Driver Failure" or .Type == "Killing")) | last | .DisplayMessage // "")
  }))
} ]`

// deploymentsJQ projects Nomad's deployment list.
const deploymentsJQ = `[ .[] | . as $d | (.TaskGroups // {} | to_entries | first | .value) as $g | {
  JobID, JobVersion, Status, StatusDescription, ModifyIndex,
  Desired: ($g.DesiredTotal // 0),
  Healthy: ($g.HealthyAllocs // 0),
  Unhealthy: ($g.UnhealthyAllocs // 0),
  Placed: ($g.PlacedAllocs // 0)
} ]`

type runtimeWire struct {
	Allocs []struct {
		ID            string `json:"ID"`
		JobID         string `json:"JobID"`
		JobVersion    uint64 `json:"JobVersion"`
		CreateTime    int64  `json:"CreateTime"`
		ClientStatus  string `json:"ClientStatus"`
		DesiredStatus string `json:"DesiredStatus"`
		NodeName      string `json:"NodeName"`
		Tasks         []struct {
			Name        string    `json:"Name"`
			State       string    `json:"State"`
			Failed      bool      `json:"Failed"`
			Restarts    int       `json:"Restarts"`
			StartedAt   time.Time `json:"StartedAt"`
			LastRestart time.Time `json:"LastRestart"`
			Last        string    `json:"Last"`
			Fail        string    `json:"Fail"`
		} `json:"Tasks"`
	} `json:"allocs"`
	Deployments []struct {
		JobID             string `json:"JobID"`
		JobVersion        uint64 `json:"JobVersion"`
		Status            string `json:"Status"`
		StatusDescription string `json:"StatusDescription"`
		ModifyIndex       uint64 `json:"ModifyIndex"`
		Desired           int    `json:"Desired"`
		Healthy           int    `json:"Healthy"`
		Unhealthy         int    `json:"Unhealthy"`
		Placed            int    `json:"Placed"`
	} `json:"deployments"`
}

// Runtime returns the live allocation and deployment state.
func (c *Cluster) Runtime(ctx context.Context) ([]deploy.AllocState, []deploy.DeploymentState, error) {
	out, err := c.node.RunOutput(ctx, runtimeScript)
	if err != nil {
		return nil, nil, fmt.Errorf("read cluster state: %w", err)
	}
	return parseRuntime(out)
}

// parseRuntime reads runtimeScript's output.
func parseRuntime(out string) ([]deploy.AllocState, []deploy.DeploymentState, error) {
	var wire runtimeWire
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &wire); err != nil {
		return nil, nil, fmt.Errorf("parse cluster state: %w", err)
	}

	allocs := make([]deploy.AllocState, 0, len(wire.Allocs))
	for _, a := range wire.Allocs {
		as := deploy.AllocState{
			ID: a.ID, JobID: a.JobID, JobVersion: a.JobVersion, CreateTime: a.CreateTime, ClientStatus: a.ClientStatus,
			DesiredStatus: a.DesiredStatus, NodeName: a.NodeName,
		}
		for _, t := range a.Tasks {
			as.Tasks = append(as.Tasks, deploy.TaskState{
				Name: t.Name, State: t.State, Failed: t.Failed,
				Restarts: t.Restarts, StartedAt: t.StartedAt, LastRestart: t.LastRestart, Last: t.Last, Fail: t.Fail,
			})
		}
		allocs = append(allocs, as)
	}

	deps := make([]deploy.DeploymentState, 0, len(wire.Deployments))
	for _, d := range wire.Deployments {
		deps = append(deps, deploy.DeploymentState{
			JobID: d.JobID, JobVersion: d.JobVersion, Status: d.Status, Description: d.StatusDescription,
			ModifyIndex: d.ModifyIndex, Desired: d.Desired,
			Healthy: d.Healthy, Unhealthy: d.Unhealthy, Placed: d.Placed,
		})
	}
	return allocs, deps, nil
}

// PlacementFailure explains why a job could not be scheduled anywhere. The
// status page's version is deploy.PlacementFailureFromNomad.
//
// This costs a call per job, so it runs only when something has actually failed
// to place. The reason lives in the evaluation rather than anywhere a user
// would think to look: the task has no logs, because it never started.
func (c *Cluster) PlacementFailure(ctx context.Context, jobID string) string {
	script := fmt.Sprintf(`curl -sf --max-time 10 %s/v1/job/%s/evaluations | jq -r '%s' 2>/dev/null`,
		NomadAddr, jobID, placementJQ)

	out, err := c.node.RunOutput(ctx, script)
	if err != nil {
		return ""
	}
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return ""
	}
	return out
}

// placementJQ explains a job's most recent failure to place, from its
// evaluations.
const placementJQ = `
  [ .[] | select(.FailedTGAllocs != null) ] | sort_by(.ModifyIndex) | last
  | (.FailedTGAllocs // {}) | to_entries | first | .value
  | [ (.ConstraintFiltered // {} | to_entries | map("\(.key) (\(.value) nodes)"))
    , (.DimensionExhausted // {} | keys | map("no capacity: \(.)"))
    , (.ClassFiltered // {} | keys | map("class filtered: \(.)"))
    ] | flatten | join("; ")`

// SecretPaths lists the variable paths orca owns, as a set.
//
// Paths only: the Nomad list endpoint returns metadata without item values, so
// finding out which secrets are set never pulls a single plaintext value off
// the machine. That is the reason a secret is one variable rather than one key
// inside a per-group variable.
func (c *Cluster) SecretPaths(ctx context.Context) (map[string]bool, error) {
	// Fails closed. An unreachable Nomad must not read as "no secrets are
	// set", because the caller that decides whether to generate a database
	// password acts on exactly that answer.
	script := fmt.Sprintf(`set -o pipefail
curl -sf --max-time 10 "%s/v1/vars?prefix=%s/" | jq -r '.[].Path'`,
		NomadAddr, deploy.SecretPrefix)

	out, err := c.node.RunOutput(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}

	set := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			set[line] = true
		}
	}
	return set, nil
}

// PutSecret writes one secret into Nomad's variable store.
//
// The whole request body travels on stdin, so the value never appears in an
// argument list: a command line is visible in the process table for as long
// as the command runs, on both ends.
func (c *Cluster) PutSecret(ctx context.Context, group, name, value string) error {
	if err := c.putVariable(ctx, deploy.SecretPath(group, name), deploy.SecretItemKey, value); err != nil {
		return fmt.Errorf("set secret %s/%s: %w", group, name, err)
	}
	return nil
}

// putVariable writes a one-item variable, replacing any there.
func (c *Cluster) putVariable(ctx context.Context, path, key, value string) error {
	body, err := variableBody(path, map[string]string{key: value})
	if err != nil {
		return err
	}
	script := fmt.Sprintf(`curl -sf --max-time 10 -X PUT --data-binary @- "%s/v1/var/%s" > /dev/null`, NomadAddr, path)
	_, err = c.node.RunStdin(ctx, script, body)
	return err
}

// CreateSecret writes a secret only if it does not exist yet, and reports
// whether it did.
//
// Nomad's check-and-set with index 0 means "create, never overwrite", so the
// guarantee a generated password depends on (made once, never replaced) is
// enforced by the store itself rather than by a list read a moment earlier
// that may be stale or may have failed.
func (c *Cluster) CreateSecret(ctx context.Context, group, name, value string) (bool, error) {
	created, err := c.createVariable(ctx, deploy.SecretPath(group, name), deploy.SecretItemKey, value)
	if err != nil {
		return false, fmt.Errorf("create secret %s/%s: %w", group, name, err)
	}
	return created, nil
}

// createVariable writes a one-item variable only if it does not exist yet, and
// reports whether it did. The body travels on stdin, as in PutSecret.
func (c *Cluster) createVariable(ctx context.Context, path, key, value string) (bool, error) {
	body, err := variableBody(path, map[string]string{key: value})
	if err != nil {
		return false, err
	}
	script := fmt.Sprintf(`CODE=$(curl -s --max-time 10 -o /dev/null -w '%%{http_code}' -X PUT --data-binary @- "%s/v1/var/%s?cas=0")
case "$CODE" in
  200) echo created ;;
  409) echo exists ;;
  *) echo "nomad answered HTTP $CODE" >&2; exit 1 ;;
esac`, NomadAddr, path)

	out, err := c.node.RunStdin(ctx, script, body)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "created", nil
}

// variableBody is the request that writes one variable.
func variableBody(path string, items map[string]string) ([]byte, error) {
	return json.Marshal(map[string]any{"Path": path, "Items": items})
}

// variablePrefixes are everything orca keeps in the store that is a secret of
// some kind: what a manifest references, the registry logins, and the
// dashboard password. The apply lock is the one thing left out.
var variablePrefixes = []string{deploy.SecretPrefix + "/", deploy.RegistryPrefix + "/", deploy.AdminPasswordPath, deploy.CertPrefix + "/"}

// Variables reads every secret the cluster holds, values included.
//
// One round trip however many there are: the listing and every read happen on
// the machine, and what comes back is one line per variable. This is the only
// call that pulls plaintext off the machine wholesale, and exporting or
// editing the cluster's secrets is the only reason to make it.
func (c *Cluster) Variables(ctx context.Context) (variables, error) {
	vars, err := c.readVariables(ctx, variablePrefixes, "{Path, Items}")
	if err != nil {
		return nil, fmt.Errorf("read the cluster's secrets: %w", err)
	}
	return vars, nil
}

// Certificates reads every certificate record, without its private key:
// what there is to know about a certificate (who asked, when it expires, why
// it failed) is all in the rest, so the key has no reason to leave the
// machine.
func (c *Cluster) Certificates(ctx context.Context) (variables, error) {
	vars, err := c.readVariables(ctx, []string{deploy.CertPrefix + "/"},
		fmt.Sprintf("{Path, Items: (.Items | del(.%s))}", deploy.CertKeyKey))
	if err != nil {
		return nil, fmt.Errorf("read the cluster's certificates: %w", err)
	}
	return vars, nil
}

// readVariables reads every variable under the prefixes in one round trip.
// project is the jq that shapes each one into a {Path, Items} line.
func (c *Cluster) readVariables(ctx context.Context, prefixes []string, project string) (variables, error) {
	// Fails closed, like SecretPaths: a listing or a read that fails must not
	// pass for a store with less in it, since an export would then be a
	// backup quietly missing secrets.
	script := fmt.Sprintf(`set -o pipefail
for prefix in %[2]s; do
  curl -sf --max-time 10 "%[1]s/v1/vars?prefix=$prefix" | jq -r '.[].Path' || exit 1
done | while IFS= read -r path; do
  curl -sf --max-time 10 "%[1]s/v1/var/$path" | jq -c '%[3]s' || exit 1
done`, NomadAddr, strings.Join(prefixes, " "), project)

	out, err := c.node.RunOutput(ctx, script)
	if err != nil {
		return nil, err
	}

	vars := variables{}
	dec := json.NewDecoder(strings.NewReader(out))
	for dec.More() {
		var v struct {
			Path  string
			Items map[string]string
		}
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		vars[v.Path] = v.Items
	}
	return vars, nil
}

// PutVariables writes a set of variables, replacing any there, in one round
// trip.
//
// Every body travels on stdin, one to a line, so no value is in an argument
// list on either machine. Paths are the caller's to have checked: each goes
// into a URL on the machine.
func (c *Cluster) PutVariables(ctx context.Context, vars variables) error {
	var bodies bytes.Buffer
	for _, path := range slices.Sorted(maps.Keys(vars)) {
		body, err := variableBody(path, vars[path])
		if err != nil {
			return err
		}
		bodies.Write(body)
		bodies.WriteByte('\n')
	}

	// Stops at the first failure and names it: what was written before it
	// stays written, and running the same import again writes the rest.
	script := fmt.Sprintf(`while IFS= read -r body; do
  path=$(jq -r .Path <<<"$body") || exit 1
  curl -sf --max-time 10 -X PUT --data-binary @- "%s/v1/var/$path" <<<"$body" > /dev/null || { echo "could not write $path" >&2; exit 1; }
done`, NomadAddr)
	if _, err := c.node.RunStdin(ctx, script, bodies.Bytes()); err != nil {
		return fmt.Errorf("write secrets: %w", err)
	}
	return nil
}

// readVariable returns one item of a variable, and whether the variable
// exists. Missing is an answer, not an error; a Nomad that cannot be asked is.
func (c *Cluster) readVariable(ctx context.Context, path, key string) (string, bool, error) {
	// The body comes back on stdout with the status after it, on a line of
	// its own: a 404 is an answer here, so curl cannot be left to fail on it.
	script := fmt.Sprintf(`curl -s --max-time 10 -w '\n%%{http_code}' "%s/v1/var/%s"`, NomadAddr, path)
	out, err := c.node.RunOutput(ctx, script)
	if err != nil {
		return "", false, err
	}
	i := strings.LastIndex(out, "\n")
	if i < 0 {
		return "", false, fmt.Errorf("read %s: no answer from nomad", path)
	}
	body, code := out[:i], strings.TrimSpace(out[i+1:])
	switch code {
	case "200":
	case "404":
		return "", false, nil
	default:
		return "", false, fmt.Errorf("read %s: nomad answered HTTP %s", path, code)
	}
	var v struct{ Items map[string]string }
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	return v.Items[key], true, nil
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
	if err := c.putVariable(ctx, deploy.AdminPasswordPath, deploy.AdminPasswordKey, value); err != nil {
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

// deleteVariable removes one variable. One that is already gone is not an
// error: Nomad answers a delete of nothing with success.
func (c *Cluster) deleteVariable(ctx context.Context, path string) error {
	return c.node.RunQuiet(ctx, fmt.Sprintf(`curl -sf --max-time 10 -X DELETE "%s/v1/var/%s" > /dev/null`, NomadAddr, path))
}

// DeleteSecret removes one secret.
func (c *Cluster) DeleteSecret(ctx context.Context, group, name string) error {
	path := deploy.SecretPath(group, name)
	script := fmt.Sprintf(`curl -sf --max-time 10 -X DELETE "%s/v1/var/%s" > /dev/null`, NomadAddr, path)
	if err := c.node.RunQuiet(ctx, script); err != nil {
		return fmt.Errorf("remove secret %s/%s (is it set?): %w", group, name, err)
	}
	return nil
}

// RegistryHosts lists the registries the cluster holds credentials for.
//
// Paths only, like SecretPaths: the host is in the path, so finding out which
// registries are logged in to never pulls a token off the machine.
func (c *Cluster) RegistryHosts(ctx context.Context) (map[string]bool, error) {
	script := fmt.Sprintf(`set -o pipefail
curl -sf --max-time 10 "%s/v1/vars?prefix=%s/" | jq -r '.[].Path'`,
		NomadAddr, deploy.RegistryPrefix)

	out, err := c.node.RunOutput(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("list registry credentials: %w", err)
	}

	hosts := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if host, ok := deploy.RegistryHost(strings.TrimSpace(line)); ok {
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
//
// The whole request body travels on stdin, so neither the token nor the
// username is ever in an argument list on either machine.
func (c *Cluster) PutRegistry(ctx context.Context, host, username, password string) error {
	path := deploy.RegistryPath(host)
	body, err := variableBody(path, registryItems(username, password))
	if err != nil {
		return err
	}

	script := fmt.Sprintf(`curl -sf --max-time 10 -X PUT --data-binary @- "%s/v1/var/%s" > /dev/null`, NomadAddr, path)
	if _, err := c.node.RunStdin(ctx, script, body); err != nil {
		return fmt.Errorf("store credentials for %s: %w", host, err)
	}
	return nil
}

// DeleteRegistry removes one registry's credentials.
func (c *Cluster) DeleteRegistry(ctx context.Context, host string) error {
	script := fmt.Sprintf(`curl -sf --max-time 10 -X DELETE "%s/v1/var/%s" > /dev/null`, NomadAddr, deploy.RegistryPath(host))
	if err := c.node.RunQuiet(ctx, script); err != nil {
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

// FirewallPath is where the machine keeps the ruleset orca generates.
const FirewallPath = "/etc/orca/firewall.nft"

// ApplyFirewall installs the ruleset, and reports whether anything changed.
//
// The new ruleset is loaded *before* it replaces the file on disk, so a
// ruleset nftables rejects never becomes the one applied at boot. The public
// interface is resolved on the machine for the same reason it is not in
// cluster.yaml: orca needs to know which interface to filter, not what it is
// called.
func (c *Cluster) ApplyFirewall(ctx context.Context, ruleset string) (bool, error) {
	script := fmt.Sprintf(`set -e
mkdir -p /etc/orca
cat > %[1]s.new

PUB=$(ip route get 1.1.1.1 2>/dev/null | grep -oP 'dev \K\S+' | head -1)
if [ -z "$PUB" ]; then
  echo "could not resolve the default-route interface" >&2
  rm -f %[1]s.new
  exit 1
fi
sed -i "s|__PUBLIC_IFACE__|${PUB}|g" %[1]s.new

# Unchanged content and a table that is actually loaded means there is nothing
# to do. Checking the table too makes this self-healing: if the rules were
# flushed by hand or lost, they come back on the next apply.
if cmp -s %[1]s.new %[1]s && nft list table inet orca >/dev/null 2>&1; then
  rm -f %[1]s.new
  echo unchanged
  exit 0
fi

nft -f %[1]s.new
mv %[1]s.new %[1]s
echo changed`, FirewallPath)

	out, err := c.node.RunStdin(ctx, script, []byte(ruleset))
	if err != nil {
		return false, fmt.Errorf("apply firewall: %w", err)
	}
	// Compared exactly, not with Contains: "unchanged" contains "changed".
	return strings.TrimSpace(out) == "changed", nil
}

// logStoreAddr resolves the log store's address from the service catalog.
//
// Not a constant: the store binds the container bridge on one machine and the
// private network on several, and moves with the machine it is pinned to. The
// catalog is the only thing that knows where it actually is.
var logStoreAddr = `ADDR=$(curl -sf --max-time 10 ` + NomadAddr + `/v1/service/` + deploy.CatalogName(deploy.OrcaApp, "victorialogs") +
	` | jq -r '.[0] | "\(.Address):\(.Port)"' 2>/dev/null)
if [ -z "$ADDR" ] || [ "$ADDR" = "null:null" ]; then
  echo "the log store is not registered; is the logs capability enabled and healthy?" >&2
  exit 1
fi`

// statusSummaryScript finds the status page in the catalog and asks it for
// the summary. Over SSH, like the log store, so `orca top` depends on neither
// ingress nor a password, and works when the front door is what is broken.
//
// Not being registered exits with its own code and says nothing: the message
// is written in Go, where a backtick is not a command substitution.
var statusSummaryScript = fmt.Sprintf(`ADDR=$(curl -sf --max-time 10 %s/v1/service/%s | jq -r '.[0] | "\(.Address):\(.Port)"' 2>/dev/null)
if [ -z "$ADDR" ] || [ "$ADDR" = "null:null" ]; then
  exit %d
fi
curl -sf --max-time 20 "http://$ADDR/api/summary"`,
	NomadAddr, deploy.CatalogName(deploy.OrcaApp, "status"), statusNotRegistered)

// statusNotRegistered is statusSummaryScript's exit code for a status page
// that is not in the catalog.
const statusNotRegistered = 3

// StatusSummary reads the status page's summary.
func (c *Cluster) StatusSummary(ctx context.Context) (statuspage.Summary, error) {
	out, err := c.node.RunOutput(ctx, statusSummaryScript)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == statusNotRegistered {
		return statuspage.Summary{}, errors.New("the status page is not running: `orca apply orca` starts it, and `orca status orca` says why it is not")
	}
	if err != nil {
		return statuspage.Summary{}, fmt.Errorf("read the status page: %w", err)
	}
	var s statuspage.Summary
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		return statuspage.Summary{}, fmt.Errorf("read the status page: %w", err)
	}
	return s, nil
}

// QueryLogs runs one LogsQL query and hands each line to fn.
func (c *Cluster) QueryLogs(ctx context.Context, query string, limit int, fn func([]byte)) error {
	script := fmt.Sprintf("%s\ncurl -sf --max-time 30 %q", logStoreAddr,
		"http://$ADDR"+logsQueryURL("/select/logsql/query", query, limit))

	out, err := c.node.RunOutput(ctx, script)
	if err != nil {
		return fmt.Errorf("query logs: %w", err)
	}

	// Newest first from the store; printed oldest first, because a log read
	// top to bottom is a story and backwards it is a puzzle.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			fn([]byte(line))
		}
	}
	return nil
}

// TailLogs streams matching lines until the context is cancelled.
func (c *Cluster) TailLogs(ctx context.Context, query string, fn func([]byte)) error {
	script := fmt.Sprintf("%s\nexec curl -sN --no-buffer %q", logStoreAddr,
		"http://$ADDR"+logsQueryURL("/select/logsql/tail", query, 0))

	cmd := exec.CommandContext(ctx, "ssh", sshArgs(c.node.Host, script)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("tail logs: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	// A log line can be long; the default 64K token limit would truncate a
	// stack trace mid-way and look like corruption.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	streamed := false
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			streamed = true
			fn([]byte(line))
		}
	}

	readErr := scanner.Err()
	err = cmd.Wait()

	// A tail ends when you stop it, which is not a failure. Interrupting the
	// command can kill ssh before the signal cancels the context, so the exit
	// status is not a reliable signal on its own.
	if ctx.Err() != nil {
		return nil
	}
	// A read that failed part way through is a different thing from a tail
	// that ended: the stream stopped for a reason nobody asked for, and
	// without this it is reported as a quiet service.
	if readErr != nil {
		return fmt.Errorf("tail logs: %w", readErr)
	}
	// Otherwise a tail that produced output and then ended is one that worked.
	if streamed {
		return nil
	}
	return err
}

// Volume identifies one app volume on disk.
type Volume struct {
	Group   string
	Service string
}

// Path is where the volume lives on its machine.
func (v Volume) Path() string { return deploy.VolumePath(DataDir, v.Group, v.Service) }

// VolumeListing is every app volume one machine holds.
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
fi`, shQuote(deploy.VolumeRoot(DataDir)))

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
		fmt.Fprintf(&b, "rmdir --ignore-fail-on-non-empty %s\n", shQuote(path.Join(deploy.VolumeRoot(DataDir), g)))
	}
	return b.String(), nil
}

// rcloneEnvScript writes the S3 credentials to a private file on the machine
// and prints its path.
//
// A file rather than command arguments: an argument list is visible in the
// process table for as long as the command runs. The file is created with a
// restrictive mode and removed by the caller.
func rcloneEnvScript(spec deploy.BackupSpec) string {
	return fmt.Sprintf(`ENVFILE=$(mktemp)
chmod 600 "$ENVFILE"
S3_ENDPOINT=%[5]s
S3_REGION=%[6]s
KEY=$(curl -sf "%[1]s/v1/var/%[2]s" | jq -r '.Items.value')
SEC=$(curl -sf "%[1]s/v1/var/%[3]s" | jq -r '.Items.value')
if [ -z "$KEY" ] || [ "$KEY" = "null" ] || [ -z "$SEC" ] || [ "$SEC" = "null" ]; then
  echo "backup credentials are not set; run: orca secret set %[4]s" >&2
  rm -f "$ENVFILE"; exit 1
fi
{
  echo "RCLONE_CONFIG_STORE_TYPE=s3"
  echo "RCLONE_CONFIG_STORE_PROVIDER=Other"
  echo "RCLONE_CONFIG_STORE_ENDPOINT=$S3_ENDPOINT"
  echo "RCLONE_CONFIG_STORE_REGION=$S3_REGION"
  echo "RCLONE_CONFIG_STORE_ACCESS_KEY_ID=$KEY"
  echo "RCLONE_CONFIG_STORE_SECRET_ACCESS_KEY=$SEC"
} > "$ENVFILE"`,
		NomadAddr,
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
	script := fmt.Sprintf(`set -eu
%[1]s
BUCKET=%[3]s
PREFIX=%[4]s
set +e
OUT=$(docker run --rm --network host --env-file "$ENVFILE" %[2]s lsf "store:$BUCKET/$PREFIX/" 2>&1)
CODE=$?
set -e
rm -f "$ENVFILE"
if [ $CODE -ne 0 ]; then
  echo "$OUT" >&2
  exit $CODE
fi
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
SVC=$(curl -sf "%[7]s/v1/service/%[9]s")
DBHOST=$(printf '%%s' "$SVC" | jq -r '.[0].Address // empty')
DBPORT=$(printf '%%s' "$SVC" | jq -r '.[0].Port // empty')
if [ -z "$DBHOST" ] || [ -z "$DBPORT" ]; then
  echo "database %[9]s is not registered; is it running?" >&2
  exit 1
fi

# The connection goes in a file, not the argument list, which is visible in the
# host's process table for as long as the restore runs.
{
  printf 'PGPASSWORD=%%s\n' "$(curl -sf "%[7]s/v1/var/%[8]s" | jq -r '.Items.value')"
  printf 'PGHOST=%%s\nPGPORT=%%s\nPGUSER=postgres\n' "$DBHOST" "$DBPORT"
} > "$PGENV"

cat > "$WORK/restore.sh" <<'RESTORE'
%[10]s
RESTORE

echo "restoring into $DBHOST:$DBPORT"
docker run --rm --network host -v "$WORK:/work" --env-file "$PGENV" \
  -e NAME="$NAME" -e STAMP="$(date -u +%%Y%%m%%dT%%H%%M%%SZ)" %[5]s sh /work/restore.sh`,
		rcloneEnvScript(spec), shQuote(spec.Image), shQuote(spec.Bucket), shQuote(spec.Prefix),
		shQuote(pgImage), shQuote(name),
		NomadAddr,
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

// PreRestorePrefix is where a restore leaves the data it replaced, suffixed
// with the time of the restore. Outside the volume root, so it is never
// mistaken for a volume of its own. It is kept rather than deleted, because
// deleting data is purge's job and nothing else's.
func PreRestorePrefix(group, service string) string {
	return path.Join(DataDir, "pre-restore", group+"-"+service)
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
  LIVE=$(curl -sf "%[10]s/v1/job/$JOB/allocations" | jq '[.[] | select(.ClientStatus == "running" or .ClientStatus == "pending")] | length')
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
		shQuote(deploy.VolumePath(DataDir, group, service)),
		shQuote(PreRestorePrefix(group, service)),
		NomadAddr)
}
