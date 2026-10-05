package deploy

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// AllocState is one allocation, projected down to what status needs.
type AllocState struct {
	// ID is the allocation's own ID. Only the status page reads it, to match
	// usage metrics to the allocations running now rather than ones just
	// replaced.
	ID            string
	JobID         string
	JobVersion    uint64
	CreateTime    int64
	ClientStatus  string // pending | running | complete | failed | lost
	DesiredStatus string // run | stop | evict
	NodeName      string
	Tasks         []TaskState

	// CPUMHz and MemoryMB are what its tasks claimed from the machine: what
	// Nomad placed it by, as opposed to what it uses. Only the status page
	// reads them, to show what fills each machine.
	CPUMHz   int64
	MemoryMB int64
}

// TaskState is one task inside an allocation.
type TaskState struct {
	Name      string
	State     string // pending | running | dead
	Failed    bool
	Restarts  int
	StartedAt time.Time
	// LastRestart is when it was last restarted, zero if never.
	LastRestart time.Time
	Last        string // the most recent event's display message
	// Fail is the most recent terminal event's message. The last event on a
	// crash-looping task is "Task restarting in 16s", which says nothing about
	// why; this holds the "Exit Code: 3" that does.
	Fail string
}

// DeploymentState is a job's rollout.
type DeploymentState struct {
	JobID       string
	JobVersion  uint64
	Status      string // running | successful | failed | cancelled | paused
	Description string
	ModifyIndex uint64
	Desired     int
	Healthy     int
	Unhealthy   int
	Placed      int
}

// Health is what orca concluded about a service.
type Health string

const (
	// HealthOK means every replica is running and the rollout finished.
	HealthOK Health = "running"
	// HealthPending means it is still starting or rolling out. Not a failure
	// yet: an image pull on a cold machine is slow and perfectly fine.
	HealthPending Health = "pending"
	// HealthFailed means it is crash-looping, unhealthy, or the rollout failed.
	HealthFailed Health = "failed"
	// HealthUnplaced means Nomad accepted the job but cannot run it anywhere.
	// This is its own state because it looks identical to "still starting"
	// while being permanent, and the cause is never in the task's logs.
	HealthUnplaced Health = "unplaced"
	// HealthStopped means the job exists but is stopped.
	HealthStopped Health = "stopped"

	// HealthScheduled means a periodic job that has not run yet. Once it has,
	// its health is its last run's (see classifyPeriodic).
	HealthScheduled Health = "scheduled"
)

// ServiceStatus is one service's worth of "is it actually working".
type ServiceStatus struct {
	JobID   string
	App     string
	Service string
	Image   string

	Health   Health
	Desired  int
	Running  int
	Restarts int
	Node     string
	Since    time.Time
	Message  string
}

// Summarize folds the cluster's raw state into one row per service.
//
// It is a pure function of its inputs so the classification, which is the part
// with actual judgement in it, is testable without a machine.
func Summarize(jobs map[string]JobState, allocs []AllocState, deployments []DeploymentState) []ServiceStatus {
	latest := latestDeployments(deployments)

	// A periodic job never owns an allocation: Nomad attributes every run to a
	// dispatched child. Collecting them under the parent is what lets a
	// failing nightly backup be reported as failing, rather than as forever
	// "scheduled". That is how a silent backup failure looks from the
	// outside, and the exact thing backups exist to prevent.
	runsByParent := map[string][]AllocState{}
	for _, a := range allocs {
		if parent, _, ok := strings.Cut(a.JobID, "/periodic-"); ok {
			runsByParent[parent] = append(runsByParent[parent], a)
		}
	}

	byJob := map[string][]AllocState{}
	for _, a := range allocs {
		// Only allocations Nomad still wants running describe the present.
		// Every other one is history: a replaced allocation, a drained one, or
		// one that failed and was already rescheduled. Counting those reports
		// replicas that are on their way out and, worse, reports a service as
		// broken because of a failure it has already recovered from. That last
		// case is the one this rule exists for.
		if a.DesiredStatus != "" && a.DesiredStatus != "run" {
			continue
		}
		if a.ClientStatus == "complete" || a.ClientStatus == "lost" {
			continue
		}
		byJob[a.JobID] = append(byJob[a.JobID], a)
	}

	out := make([]ServiceStatus, 0, len(jobs))
	for id, job := range jobs {
		s := ServiceStatus{
			JobID:   id,
			App:     job.App,
			Service: job.Service,
			Image:   job.Image,
			Desired: job.Count,
		}
		// One copy per machine: a job on three machines reported as "3/1"
		// would read as three times too many, and one crashed on a machine
		// would still read as healthy. Every allocation Nomad still wants
		// running is a machine it should be running on.
		if job.System {
			s.Desired = len(byJob[id])
		}
		if job.Periodic {
			classifyPeriodic(&s, runsByParent[id])
		} else {
			classify(&s, job, byJob[id], latest[id])
		}
		out = append(out, s)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Service < out[j].Service
	})
	return out
}

// classifyPeriodic reports a scheduled job by its most recent run.
//
// A batch job's allocation ends: "complete" is success, not something on its
// way out, so the rules for a long-running service do not apply. What matters
// is whether the last run worked.
func classifyPeriodic(s *ServiceStatus, runs []AllocState) {
	if len(runs) == 0 {
		s.Health = HealthScheduled
		s.Message = "not run yet"
		return
	}

	latest := runs[0]
	for _, r := range runs[1:] {
		if r.CreateTime > latest.CreateTime {
			latest = r
		}
	}

	if !latest.Since().IsZero() {
		s.Since = latest.Since()
	}
	for _, t := range latest.Tasks {
		s.Restarts += t.Restarts
		if t.Failed || t.Last != "" {
			s.Message = t.Last
		}
		if t.Fail != "" && (t.Failed || t.State == "dead") {
			s.Message = t.Fail
		}
	}

	switch {
	case latest.ClientStatus == "failed" || anyTaskFailing([]AllocState{latest}):
		s.Health = HealthFailed
	case latest.ClientStatus == "complete":
		s.Health = HealthOK
		s.Running, s.Desired = 1, 1
		s.Message = "last run succeeded"
	default:
		s.Health = HealthPending
	}
}

func classify(s *ServiceStatus, job JobState, allocs []AllocState, dep *DeploymentState) {
	if job.Stopped {
		s.Health = HealthStopped
		return
	}

	// A periodic job spends almost all of its life with no allocation. Judging
	// it by the rules below would report every backup as unplaced between
	// runs.
	if job.Periodic && len(allocs) == 0 {
		s.Health = HealthScheduled
		return
	}

	var worst TaskState
	for _, a := range allocs {
		if a.NodeName != "" {
			s.Node = a.NodeName
		}
		for _, t := range a.Tasks {
			s.Restarts += t.Restarts
			if t.State == "running" && !t.Failed {
				if s.Since.IsZero() || t.StartedAt.Before(s.Since) {
					s.Since = t.StartedAt
				}
			}
			// Keep the least healthy task's message: that is the one that
			// explains why the service is not working.
			if worst.Name == "" || (t.State != "running" && worst.State == "running") {
				worst = t
			}
		}
		if a.ClientStatus == "running" {
			s.Running++
		}
	}
	s.Message = worst.Last
	if worst.Fail != "" && (worst.State != "running" || worst.Failed || worst.Restarts > 0) {
		s.Message = worst.Fail
	}

	// Nomad accepted the job but placed nothing. The task has no logs and no
	// events, because it never ran, so without naming this state the only
	// symptom is a service that stays "pending" forever.
	if len(allocs) == 0 && (dep == nil || dep.Placed == 0) {
		s.Health = HealthUnplaced
		if s.Message == "" {
			s.Message = "no allocation placed"
		}
		return
	}

	switch {
	case anyTaskFailing(allocs):
		s.Health = HealthFailed
	case dep != nil && dep.Status == "running":
		s.Health = HealthPending

	// The allocations outrank the deployment record. A deployment that failed
	// is a fact about the past and never changes: Nomad leaves it "failed"
	// forever, even after the task it was waiting on starts and stays up. A
	// task that crashed, exhausted its restart attempts, and then recovered
	// once the cause was cleared is running. Reporting it as failed sends you
	// to look at a container that is working.
	//
	// This is below anyTaskFailing, so a deployment that failed *and* left the
	// allocations broken still reports failed; only the recovered case moves.
	case s.Running >= s.Desired && s.Desired > 0:
		s.Health = HealthOK

	case dep != nil && dep.Status == "failed":
		s.Health = HealthFailed
		if s.Message == "" {
			s.Message = dep.Description
		}
	default:
		s.Health = HealthPending
	}

	// A task that crashes every half minute spends half of each cycle
	// running, and every one of those halves reads as healthy. One that came
	// back from a restart moments ago is not yet known to have stayed up, so
	// it is not called running until it has.
	if s.Health == HealthOK {
		if at := lastRestart(allocs); time.Since(at) < RestartSettle {
			s.Health = HealthPending
			s.Message = fmt.Sprintf("restarted %s ago", HumanDuration(time.Since(at)))
			if worst.Fail != "" {
				s.Message += ": " + worst.Fail
			}
			return
		}
	}

	// A past failure explains a service that is not working. Next to one that
	// is, it only misleads: the exit code that sent it into a restart loop is
	// not news once it has come back.
	if s.Health == HealthOK {
		s.Message = ""
	}
}

// RestartSettle is how long a task has to stay up after a restart before it
// is called running again.
const RestartSettle = time.Minute

// lastRestart is the latest restart of any task in allocs, or the zero time.
func lastRestart(allocs []AllocState) time.Time {
	var out time.Time
	for _, a := range allocs {
		for _, t := range a.Tasks {
			if t.LastRestart.After(out) {
				out = t.LastRestart
			}
		}
	}
	return out
}

// Rollout judges a job that was just submitted by what Nomad concluded about
// the version submitted, rather than by whatever happens to be running.
//
// Until the scheduler has acted on the new version, the allocations running
// are the previous version's and the latest deployment is the previous one,
// which succeeded, so judged the ordinary way a deploy that has not started
// yet reads as healthy. And while it is under way, a new copy that restarts
// once reads as failed, although Nomad may well bring it up in time. Nomad's
// deployment is the verdict that counts: it waits for the new copies to pass
// their checks, and fails and rolls back when they do not.
func Rollout(s ServiceStatus, version uint64, allocs []AllocState, deployments []DeploymentState) ServiceStatus {
	var dep *DeploymentState
	for i := range deployments {
		d := &deployments[i]
		if d.JobID == s.JobID && d.JobVersion == version && (dep == nil || d.ModifyIndex > dep.ModifyIndex) {
			dep = d
		}
	}

	if dep != nil {
		switch dep.Status {
		case "successful":
			if s.Health != HealthFailed {
				s.Health, s.Message = HealthOK, ""
			}
		case "failed", "cancelled":
			// Nomad's own words, which say when it has rolled back and to
			// which version.
			s.Health, s.Message = HealthFailed, dep.Description
		default:
			s.Health = HealthPending
			s.Message = fmt.Sprintf("deploying: %d of %d healthy", dep.Healthy, dep.Desired)
		}
		return s
	}

	// No deployment for this version: a change Nomad applied in place without
	// one, or a job that never has one. The ordinary judgement holds once
	// every allocation it wants running is on the new version.
	current := 0
	for _, a := range allocs {
		if a.JobID != s.JobID || (a.DesiredStatus != "" && a.DesiredStatus != "run") ||
			a.ClientStatus == "complete" || a.ClientStatus == "lost" {
			continue
		}
		if a.JobVersion != version {
			s.Health, s.Message = HealthPending, "starting the new version"
			return s
		}
		current++
	}
	if current == 0 {
		s.Health, s.Message = HealthPending, "starting the new version"
	}
	return s
}

// Since is when this allocation's tasks started, or the zero time.
func (a AllocState) Since() time.Time {
	var out time.Time
	for _, t := range a.Tasks {
		if !t.StartedAt.IsZero() && (out.IsZero() || t.StartedAt.Before(out)) {
			out = t.StartedAt
		}
	}
	return out
}

// anyTaskFailing reports a task that is dead or restarting under a job that is
// supposed to be running: the crash-loop case that a spec-hash comparison
// cannot see, because the spec is perfectly current while nothing works.
func anyTaskFailing(allocs []AllocState) bool {
	for _, a := range allocs {
		if a.ClientStatus == "failed" {
			return true
		}
		for _, t := range a.Tasks {
			if t.Failed {
				return true
			}
			// Deliberately not "any dead task": a task that has finished its
			// work is dead too. A poststart task that sets a service up exits
			// as soon as it is done, and a completed batch run ends the same
			// way. Neither is a failure, and Nomad says so by leaving Failed
			// unset. Restarting is the signal that something ended when it
			// should not have.
			if strings.Contains(t.Last, "restarting") || strings.Contains(t.Last, "Restarting") {
				return true
			}
		}
	}
	return false
}

func latestDeployments(deployments []DeploymentState) map[string]*DeploymentState {
	out := map[string]*DeploymentState{}
	for i := range deployments {
		d := &deployments[i]
		// A resubmitted job leaves its previous deployment behind as
		// "cancelled". Taking the highest modify index means health is always
		// judged against the rollout in progress, never a stale one.
		if prev, ok := out[d.JobID]; !ok || d.ModifyIndex > prev.ModifyIndex {
			out[d.JobID] = d
		}
	}
	return out
}

// Settled reports whether this service has reached a state worth stopping a
// wait on: either it works, or it is not going to without a change.
func (s ServiceStatus) Settled() bool {
	switch s.Health {
	case HealthOK, HealthFailed, HealthStopped, HealthScheduled:
		return true
	}
	return false
}

// Line renders one status row.
func (s ServiceStatus) Line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %-14s %-9s %d/%d", s.Service, s.Health, s.Running, s.Desired)

	if !s.Since.IsZero() && s.Health == HealthOK {
		fmt.Fprintf(&b, "  up %s", HumanDuration(time.Since(s.Since)))
	}
	if s.Restarts > 0 {
		fmt.Fprintf(&b, "  restarts %d", s.Restarts)
	}
	if s.Image != "" {
		fmt.Fprintf(&b, "  %s", shortImage(s.Image))
	}
	if s.Health != HealthOK && s.Message != "" {
		fmt.Fprintf(&b, "\n  %-14s %s", "", s.Message)
	}
	return b.String()
}

// HumanDuration is a duration the way status writes it: its largest unit only.
func HumanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
