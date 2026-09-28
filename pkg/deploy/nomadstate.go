package deploy

import (
	"fmt"
	"sort"
	"strings"

	nomad "github.com/hashicorp/nomad/api"
)

// The status page reads Nomad's API directly, where the CLI reads it over SSH
// through the jq projections in cmd/orca/cluster.go. These are the same
// projections, from Nomad's own types, so both arrive at the same JobState,
// AllocState and DeploymentState and Summarize judges them identically. A rule
// changed on one side has to change on the other.

// JobStateFromNomad projects a job the way listJobsScript does. ok is false
// for a job orca does not own, and for a periodic job's child: its runs
// inherit the parent's metadata, and are not services.
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
		Version:  deref(j.Version),
		Stopped:  j.Stop != nil && *j.Stop,
		Count:    count,
		System:   j.Type != nil && *j.Type == "system",
		Periodic: j.Periodic != nil && j.Periodic.Enabled != nil && *j.Periodic.Enabled,
		AuthHash: j.Meta[MetaAuth],
	}, true
}

// AllocStateFromNomad projects an allocation the way runtimeScript does.
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
			Name:      name,
			State:     ts.State,
			Failed:    ts.Failed,
			Restarts:  int(ts.Restarts),
			StartedAt: ts.StartedAt,
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

// DeploymentStateFromNomad projects a deployment the way runtimeScript does:
// by its first task group, which is its only one for everything orca deploys.
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
// evaluations, the way Cluster.PlacementFailure does: the most recent
// evaluation that failed to place anything, and what ruled the machines out.
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

// firstKey is jq's `to_entries | first` over a map Nomad serialised: Go writes
// map keys sorted, so the first entry is the smallest key.
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
