package deploy

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	nomad "github.com/hashicorp/nomad/api"
)

// Everything that reads Nomad's state reads it here: the CLI through a
// connection carried over SSH, the status page from on the machine. Both
// arrive at the same JobState, AllocState and DeploymentState, so Summarize
// judges a service identically wherever it is asked.

// fanOut is how many requests one read makes at a time. A read of every job
// is a request per job, and made one after another it would cost a round trip
// each.
const fanOut = 8

// each calls fn for every index below n, fanOut at a time, and returns the
// first error.
func each(n int, fn func(i int) error) error {
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
		slots = make(chan struct{}, fanOut)
	)
	for i := range n {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			if err := fn(i); err != nil {
				once.Do(func() { first = err })
			}
		})
	}
	wg.Wait()
	return first
}

// ReadJobs returns every orca-managed job the cluster knows about, keyed by
// job ID.
//
// It fails rather than answering short. A read that lost a job would say
// "not deployed" about it: the answer every caller acts on, so status would
// report it gone and apply would plan to create it.
func ReadJobs(ctx context.Context, c *nomad.Client) (map[string]JobState, error) {
	q := (&nomad.QueryOptions{}).WithContext(ctx)

	// The listing carries each job's metadata, so only the jobs orca owns
	// are read in full.
	stubs, _, err := c.Jobs().ListOptions(&nomad.JobListOptions{Fields: &nomad.JobListFields{Meta: true}}, q)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	var ids []string
	for _, stub := range stubs {
		// A periodic job's past runs are listed as jobs of their own.
		if stub.ParentID == "" && stub.Meta[MetaManaged] == "true" {
			ids = append(ids, stub.ID)
		}
	}

	jobs := make([]*nomad.Job, len(ids))
	err = each(len(ids), func(i int) error {
		job, _, err := c.Jobs().Info(ids[i], q)
		if err != nil {
			return fmt.Errorf("read job %s: %w", ids[i], err)
		}
		jobs[i] = job
		return nil
	})
	if err != nil {
		return nil, err
	}

	states := make(map[string]JobState, len(jobs))
	for _, job := range jobs {
		if s, ok := JobStateFromNomad(job); ok {
			states[s.ID] = s
		}
	}
	return states, nil
}

// ReadRuntime returns the live allocation and deployment state, in two
// requests however many jobs exist. resources adds what each allocation has
// claimed, which is more to send and wanted only where it is shown.
func ReadRuntime(ctx context.Context, c *nomad.Client, resources bool) ([]AllocState, []DeploymentState, error) {
	q := (&nomad.QueryOptions{}).WithContext(ctx)
	allocQ := q
	if resources {
		allocQ = (&nomad.QueryOptions{Params: map[string]string{"resources": "true"}}).WithContext(ctx)
	}

	var (
		allocs []*nomad.AllocationListStub
		deps   []*nomad.Deployment
	)
	err := each(2, func(i int) (err error) {
		if i == 0 {
			if allocs, _, err = c.Allocations().List(allocQ); err != nil {
				return fmt.Errorf("list allocations: %w", err)
			}
			return nil
		}
		if deps, _, err = c.Deployments().List(q); err != nil {
			return fmt.Errorf("list deployments: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	allocStates := make([]AllocState, 0, len(allocs))
	for _, a := range allocs {
		allocStates = append(allocStates, AllocStateFromNomad(a))
	}
	depStates := make([]DeploymentState, 0, len(deps))
	for _, d := range deps {
		depStates = append(depStates, DeploymentStateFromNomad(d))
	}
	return allocStates, depStates, nil
}

// ReadPlacement explains why a job could not be scheduled anywhere, or says
// nothing when that cannot be found out.
//
// This costs a request per job, so it is made only for a job that has failed
// to place. The reason lives in the evaluation rather than anywhere a user
// would think to look: the task has no logs, because it never started.
func ReadPlacement(ctx context.Context, c *nomad.Client, jobID string) string {
	evals, _, err := c.Jobs().Evaluations(jobID, (&nomad.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return ""
	}
	return PlacementFailureFromNomad(evals)
}

// PlanJobs asks Nomad what submitting each job would do, without submitting
// anything, keyed by job ID. A job Nomad rejects outright fails here, before
// anything has changed, rather than halfway through an apply.
func PlanJobs(ctx context.Context, c *nomad.Client, jobs []*nomad.Job) (map[string]JobPlan, error) {
	w := (&nomad.WriteOptions{}).WithContext(ctx)
	plans := make([]JobPlan, len(jobs))
	err := each(len(jobs), func(i int) error {
		r, _, err := c.Jobs().Plan(jobs[i], true, w)
		if err != nil {
			return fmt.Errorf("plan %s: %w", *jobs[i].ID, err)
		}
		plans[i] = JobPlanFromNomad(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	byID := make(map[string]JobPlan, len(jobs))
	for i, j := range jobs {
		byID[*j.ID] = plans[i]
	}
	return byID, nil
}

// ReadVariables reads every variable under the prefixes, values included,
// keyed by path.
//
// It fails closed: a listing or a read that fails must not pass for a store
// with less in it, since an export would then be a backup quietly missing
// secrets.
func ReadVariables(ctx context.Context, c *nomad.Client, prefixes []string) (map[string]map[string]string, error) {
	q := (&nomad.QueryOptions{}).WithContext(ctx)

	var paths []string
	for _, prefix := range prefixes {
		metas, _, err := c.Variables().List((&nomad.QueryOptions{Prefix: prefix}).WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, m := range metas {
			paths = append(paths, m.Path)
		}
	}

	items := make([]map[string]string, len(paths))
	err := each(len(paths), func(i int) error {
		v, _, err := c.Variables().Read(paths[i], q)
		if err != nil {
			return fmt.Errorf("read %s: %w", paths[i], err)
		}
		items[i] = v.Items
		return nil
	})
	if err != nil {
		return nil, err
	}

	vars := make(map[string]map[string]string, len(paths))
	for i, p := range paths {
		vars[p] = items[i]
	}
	return vars, nil
}

// JobStateFromNomad projects a job. ok is false for a job orca does not own,
// and for a periodic job's child: its runs inherit the parent's metadata, and
// are not services.
func JobStateFromNomad(j *nomad.Job) (JobState, bool) {
	if j == nil || j.ID == nil || (j.ParentID != nil && *j.ParentID != "") || IsPeriodicChild(*j.ID) {
		return JobState{}, false
	}
	if j.Meta[MetaManaged] != "true" {
		return JobState{}, false
	}
	count := 1
	if len(j.TaskGroups) > 0 && j.TaskGroups[0].Count != nil {
		count = *j.TaskGroups[0].Count
	}
	return JobState{
		ID:       *j.ID,
		App:      j.Meta[MetaApp],
		Service:  j.Meta[MetaService],
		Image:    j.Meta[MetaImage],
		ImageRef: j.Meta[MetaImageRef],
		Version:  deref(j.Version),
		Stopped:  j.Stop != nil && *j.Stop,
		Count:    count,
		System:   j.Type != nil && *j.Type == "system",
		Periodic: j.Periodic != nil && j.Periodic.Enabled != nil && *j.Periodic.Enabled,
		AuthHash: j.Meta[MetaAuth],
	}, true
}

// AllocStateFromNomad projects an allocation.
func AllocStateFromNomad(a *nomad.AllocationListStub) AllocState {
	out := AllocState{
		ID:            a.ID,
		JobID:         a.JobID,
		JobVersion:    a.JobVersion,
		CreateTime:    a.CreateTime,
		ClientStatus:  a.ClientStatus,
		DesiredStatus: a.DesiredStatus,
		NodeName:      a.NodeName,
	}
	if r := a.AllocatedResources; r != nil {
		for _, t := range r.Tasks {
			if t != nil {
				out.CPUMHz += t.Cpu.CpuShares
				out.MemoryMB += t.Memory.MemoryMB
			}
		}
	}
	names := make([]string, 0, len(a.TaskStates))
	for name := range a.TaskStates {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ts := a.TaskStates[name]
		if ts == nil {
			continue
		}
		t := TaskState{
			Name:        name,
			State:       ts.State,
			Failed:      ts.Failed,
			Restarts:    int(ts.Restarts),
			StartedAt:   ts.StartedAt,
			LastRestart: ts.LastRestart,
		}
		if n := len(ts.Events); n > 0 && ts.Events[n-1] != nil {
			t.Last = ts.Events[n-1].DisplayMessage
		}
		for _, e := range ts.Events {
			if e != nil && terminalEvent(e.Type) {
				t.Fail = e.DisplayMessage
			}
		}
		out.Tasks = append(out.Tasks, t)
	}
	return out
}

// terminalEvent is an event that says why a task ended: the "Exit Code: 3" a
// crash-looping task's latest "restarting in 16s" does not.
func terminalEvent(typ string) bool {
	switch typ {
	case "Terminated", "Driver Failure", "Killing":
		return true
	}
	return false
}

// DeploymentStateFromNomad projects a deployment by its first task group,
// which is its only one for everything orca deploys.
func DeploymentStateFromNomad(d *nomad.Deployment) DeploymentState {
	out := DeploymentState{
		JobID:       d.JobID,
		JobVersion:  d.JobVersion,
		Status:      d.Status,
		Description: d.StatusDescription,
		ModifyIndex: d.ModifyIndex,
	}
	if g := d.TaskGroups[firstKey(d.TaskGroups)]; g != nil {
		out.Desired = g.DesiredTotal
		out.Healthy = g.HealthyAllocs
		out.Unhealthy = g.UnhealthyAllocs
		out.Placed = g.PlacedAllocs
	}
	return out
}

// PlacementFailureFromNomad explains why a job could not be placed, from its
// evaluations: the most recent one that failed to place anything, and what
// ruled the machines out.
func PlacementFailureFromNomad(evals []*nomad.Evaluation) string {
	var latest *nomad.Evaluation
	for _, e := range evals {
		if e == nil || e.FailedTGAllocs == nil {
			continue
		}
		if latest == nil || e.ModifyIndex > latest.ModifyIndex {
			latest = e
		}
	}
	if latest == nil {
		return ""
	}
	return describePlacement(latest.FailedTGAllocs[firstKey(latest.FailedTGAllocs)])
}

// describePlacement says what ruled every machine out for one task group, or
// "" when nothing did.
func describePlacement(m *nomad.AllocationMetric) string {
	if m == nil {
		return ""
	}
	var parts []string
	for _, k := range sortedKeys(m.ConstraintFiltered) {
		parts = append(parts, fmt.Sprintf("%s (%d nodes)", k, m.ConstraintFiltered[k]))
	}
	for _, k := range sortedKeys(m.DimensionExhausted) {
		parts = append(parts, "no capacity: "+k)
	}
	for _, k := range sortedKeys(m.ClassFiltered) {
		parts = append(parts, "class filtered: "+k)
	}
	return strings.Join(parts, "; ")
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// firstKey is the smallest key of a map, so that "the first task group" is
// the same one every time it is asked for.
func firstKey[V any](m map[string]V) string {
	keys := sortedKeys(m)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
