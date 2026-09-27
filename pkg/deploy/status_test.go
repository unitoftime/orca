package deploy

import (
	"testing"
	"time"
)

func job(id, app, svc string, count int) JobState {
	return JobState{ID: id, App: app, Service: svc, Count: count, Image: "img@sha256:abc"}
}

func only(t *testing.T, got []ServiceStatus) ServiceStatus {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("want 1 status, got %d", len(got))
	}
	return got[0]
}

func TestSummarizeHealthy(t *testing.T) {
	jobs := map[string]JobState{"a-web": job("a-web", "a", "web", 1)}
	allocs := []AllocState{{
		JobID: "a-web", ClientStatus: "running", NodeName: "box0",
		Tasks: []TaskState{{Name: "web", State: "running", StartedAt: time.Now().Add(-time.Hour), Last: "Task started by client"}},
	}}
	deps := []DeploymentState{{JobID: "a-web", Status: "successful", ModifyIndex: 10, Desired: 1, Healthy: 1, Placed: 1}}

	s := only(t, Summarize(jobs, allocs, deps))
	if s.Health != HealthOK {
		t.Errorf("health = %s, want %s (%q)", s.Health, HealthOK, s.Message)
	}
	if s.Running != 1 || s.Desired != 1 {
		t.Errorf("running/desired = %d/%d, want 1/1", s.Running, s.Desired)
	}
	if s.Node != "box0" {
		t.Errorf("node = %q", s.Node)
	}
}

// The case a spec-hash comparison cannot see: the spec is perfectly current
// and nothing works.
func TestSummarizeCrashLoop(t *testing.T) {
	jobs := map[string]JobState{"a-web": job("a-web", "a", "web", 1)}
	allocs := []AllocState{{
		JobID: "a-web", ClientStatus: "pending",
		Tasks: []TaskState{{
			Name: "web", State: "pending", Restarts: 3,
			Last: "Task restarting in 16.1s",
			Fail: `Exit Code: 3, Exit Message: "Docker container exited with non-zero exit code: 3"`,
		}},
	}}
	deps := []DeploymentState{{JobID: "a-web", Status: "running", ModifyIndex: 10, Desired: 1, Placed: 1}}

	s := only(t, Summarize(jobs, allocs, deps))
	if s.Health != HealthFailed {
		t.Errorf("health = %s, want %s", s.Health, HealthFailed)
	}
	// "restarting in 16.1s" is the last event and says nothing about why. The
	// exit code is the part worth printing.
	if s.Message != `Exit Code: 3, Exit Message: "Docker container exited with non-zero exit code: 3"` {
		t.Errorf("message = %q, want the terminal event, not the restart notice", s.Message)
	}
	if s.Restarts != 3 {
		t.Errorf("restarts = %d, want 3", s.Restarts)
	}
}

// A job Nomad accepted but placed nowhere looks exactly like one that is still
// starting, while being permanent. It gets its own state so it can be
// explained rather than waited on.
func TestSummarizeUnplaced(t *testing.T) {
	jobs := map[string]JobState{"a-web": job("a-web", "a", "web", 1)}
	deps := []DeploymentState{{JobID: "a-web", Status: "running", ModifyIndex: 10, Desired: 1, Placed: 0}}

	s := only(t, Summarize(jobs, nil, deps))
	if s.Health != HealthUnplaced {
		t.Errorf("health = %s, want %s", s.Health, HealthUnplaced)
	}
}

func TestSummarizeStopped(t *testing.T) {
	jobs := map[string]JobState{"a-web": {ID: "a-web", App: "a", Service: "web", Count: 1, Stopped: true}}

	s := only(t, Summarize(jobs, nil, nil))
	if s.Health != HealthStopped {
		t.Errorf("health = %s, want %s", s.Health, HealthStopped)
	}
	// A stopped service is a fact, not a fault: apply must not fail because of
	// one, and Settled keeps a wait from hanging on it.
	if !s.Settled() {
		t.Error("a stopped service should be settled")
	}
}

func TestSummarizeRollingOut(t *testing.T) {
	jobs := map[string]JobState{"a-web": job("a-web", "a", "web", 2)}
	allocs := []AllocState{{
		JobID: "a-web", ClientStatus: "running",
		Tasks: []TaskState{{Name: "web", State: "running", StartedAt: time.Now()}},
	}}
	deps := []DeploymentState{{JobID: "a-web", Status: "running", ModifyIndex: 10, Desired: 2, Healthy: 1, Placed: 2}}

	s := only(t, Summarize(jobs, allocs, deps))
	if s.Health != HealthPending {
		t.Errorf("health = %s, want %s", s.Health, HealthPending)
	}
	if s.Settled() {
		t.Error("a rollout in progress is not settled; the wait should keep polling")
	}
}

// Resubmitting a job leaves the old deployment behind as "cancelled". Judging
// health against it would report a fresh rollout using stale information.
func TestSummarizeIgnoresStaleDeployments(t *testing.T) {
	jobs := map[string]JobState{"a-web": job("a-web", "a", "web", 1)}
	allocs := []AllocState{{
		JobID: "a-web", ClientStatus: "running",
		Tasks: []TaskState{{Name: "web", State: "running", StartedAt: time.Now()}},
	}}
	deps := []DeploymentState{
		{JobID: "a-web", Status: "successful", ModifyIndex: 5, Desired: 1, Healthy: 1, Placed: 1},
		{JobID: "a-web", Status: "running", ModifyIndex: 99, Desired: 1, Healthy: 0, Placed: 1},
	}

	if s := only(t, Summarize(jobs, allocs, deps)); s.Health != HealthPending {
		t.Errorf("health = %s, want %s from the newest deployment", s.Health, HealthPending)
	}
}

// A replaced allocation lingers. Counting it reports two replicas where one is
// already on its way out.
func TestSummarizeIgnoresFinishedAllocs(t *testing.T) {
	jobs := map[string]JobState{"a-web": job("a-web", "a", "web", 1)}
	allocs := []AllocState{
		{JobID: "a-web", ClientStatus: "complete", Tasks: []TaskState{{Name: "web", State: "dead"}}},
		{JobID: "a-web", ClientStatus: "lost", Tasks: []TaskState{{Name: "web", State: "dead"}}},
		{JobID: "a-web", ClientStatus: "running", Tasks: []TaskState{{Name: "web", State: "running", StartedAt: time.Now()}}},
	}
	deps := []DeploymentState{{JobID: "a-web", Status: "successful", ModifyIndex: 10, Desired: 1, Healthy: 1, Placed: 1}}

	s := only(t, Summarize(jobs, allocs, deps))
	if s.Running != 1 {
		t.Errorf("running = %d, want 1: finished allocations must not be counted", s.Running)
	}
	if s.Health != HealthOK {
		t.Errorf("health = %s, want %s: a dead replaced alloc is not a failure", s.Health, HealthOK)
	}
}

func TestSummarizeSortsByAppThenService(t *testing.T) {
	jobs := map[string]JobState{
		"b-web":  job("b-web", "b", "web", 1),
		"a-zeta": job("a-zeta", "a", "zeta", 1),
		"a-beta": job("a-beta", "a", "beta", 1),
	}

	got := Summarize(jobs, nil, nil)
	want := []string{"a/beta", "a/zeta", "b/web"}
	for i, w := range want {
		if got[i].App+"/"+got[i].Service != w {
			t.Errorf("position %d = %s/%s, want %s", i, got[i].App, got[i].Service, w)
		}
	}
}

// A failed allocation Nomad has already replaced is history, not current
// state. Reporting a service as broken because of a failure it recovered from
// makes apply fail on a healthy cluster, which is worse than not checking at
// all, because it trains you to ignore the check.
func TestSummarizeIgnoresRecoveredFailures(t *testing.T) {
	jobs := map[string]JobState{"a-web": job("a-web", "a", "web", 1)}
	allocs := []AllocState{
		{
			JobID: "a-web", ClientStatus: "failed", DesiredStatus: "stop",
			Tasks: []TaskState{{Name: "web", State: "dead", Failed: true, Restarts: 2, Fail: "failed to setup alloc: network"}},
		},
		{
			JobID: "a-web", ClientStatus: "running", DesiredStatus: "run",
			Tasks: []TaskState{{Name: "web", State: "running", StartedAt: time.Now()}},
		},
	}
	deps := []DeploymentState{{JobID: "a-web", Status: "successful", ModifyIndex: 10, Desired: 1, Healthy: 1, Placed: 1}}

	s := only(t, Summarize(jobs, allocs, deps))
	if s.Health != HealthOK {
		t.Errorf("health = %s (%q), want %s: the failed alloc was already replaced", s.Health, s.Message, HealthOK)
	}
	if s.Running != 1 {
		t.Errorf("running = %d, want 1", s.Running)
	}
}

// An allocation being drained is still running but no longer wanted, so it
// must not be counted during a rolling update.
func TestSummarizeIgnoresDrainingAllocs(t *testing.T) {
	jobs := map[string]JobState{"a-web": job("a-web", "a", "web", 1)}
	allocs := []AllocState{
		{JobID: "a-web", ClientStatus: "running", DesiredStatus: "stop",
			Tasks: []TaskState{{Name: "web", State: "running", StartedAt: time.Now()}}},
		{JobID: "a-web", ClientStatus: "running", DesiredStatus: "run",
			Tasks: []TaskState{{Name: "web", State: "running", StartedAt: time.Now()}}},
	}
	deps := []DeploymentState{{JobID: "a-web", Status: "successful", ModifyIndex: 10, Desired: 1, Healthy: 1, Placed: 1}}

	if s := only(t, Summarize(jobs, allocs, deps)); s.Running != 1 {
		t.Errorf("running = %d, want 1: a draining alloc is on its way out", s.Running)
	}
}

// A periodic job never owns an allocation: Nomad attributes every run to a
// dispatched child. Reporting the parent by its own (always empty) allocation
// set would make a failing nightly backup look "scheduled" forever. That is
// what a silent backup failure looks like from outside, and the exact thing
// backups exist to prevent.
func TestPeriodicHealthIsItsLastRun(t *testing.T) {
	jobs := map[string]JobState{
		"shop-db-backup": {ID: "shop-db-backup", App: "shop", Service: "db-backup", Count: 1, Periodic: true},
	}

	runs := []AllocState{
		{JobID: "shop-db-backup/periodic-100", CreateTime: 100, ClientStatus: "complete",
			Tasks: []TaskState{{Name: "dump", State: "dead"}}},
		{JobID: "shop-db-backup/periodic-200", CreateTime: 200, ClientStatus: "failed",
			Tasks: []TaskState{{Name: "upload", State: "dead", Failed: true, Fail: "Exit Code: 1"}}},
	}

	s := only(t, Summarize(jobs, runs, nil))
	if s.Health != HealthFailed {
		t.Errorf("health = %s, want %s: the most recent run failed", s.Health, HealthFailed)
	}
	if s.Message != "Exit Code: 1" {
		t.Errorf("message = %q, want the failure's own", s.Message)
	}
}

// A completed batch run is a success, not an allocation on its way out.
func TestPeriodicCompletedRunIsHealthy(t *testing.T) {
	jobs := map[string]JobState{
		"shop-db-backup": {ID: "shop-db-backup", App: "shop", Service: "db-backup", Count: 1, Periodic: true},
	}
	runs := []AllocState{
		{JobID: "shop-db-backup/periodic-200", CreateTime: 200, ClientStatus: "failed",
			Tasks: []TaskState{{Name: "upload", State: "dead", Failed: true}}},
		{JobID: "shop-db-backup/periodic-300", CreateTime: 300, ClientStatus: "complete",
			Tasks: []TaskState{{Name: "upload", State: "dead"}}},
	}

	s := only(t, Summarize(jobs, runs, nil))
	if s.Health != HealthOK {
		t.Errorf("health = %s, want %s: the newest run succeeded", s.Health, HealthOK)
	}
}

// Before its first run there is nothing to report, which is not a failure.
func TestPeriodicNeverRunIsScheduled(t *testing.T) {
	jobs := map[string]JobState{
		"shop-db-backup": {ID: "shop-db-backup", App: "shop", Service: "db-backup", Count: 1, Periodic: true},
	}
	s := only(t, Summarize(jobs, nil, nil))
	if s.Health != HealthScheduled {
		t.Errorf("health = %s, want %s", s.Health, HealthScheduled)
	}
}

// A task that has finished its work is dead too. A poststart task that sets a
// service up exits as soon as it is done, and reporting that as a failure would
// make a perfectly healthy object store show as failed on every deploy.
func TestFinishedPoststartTaskIsNotAFailure(t *testing.T) {
	jobs := map[string]JobState{"files-store": job("files-store", "files", "store", 1)}
	allocs := []AllocState{{
		JobID: "files-store", ClientStatus: "running", DesiredStatus: "run",
		Tasks: []TaskState{
			{Name: "store", State: "running", StartedAt: time.Now()},
			{Name: "store-init", State: "dead", Failed: false, Last: "Terminated: Exit Code: 0"},
		},
	}}
	deps := []DeploymentState{{JobID: "files-store", Status: "successful", ModifyIndex: 1, Desired: 1, Healthy: 1, Placed: 1}}

	if s := only(t, Summarize(jobs, allocs, deps)); s.Health != HealthOK {
		t.Errorf("health = %s (%q), want %s", s.Health, s.Message, HealthOK)
	}
}

// A task that died without being asked to still has to be caught.
func TestFailedTaskIsStillAFailure(t *testing.T) {
	jobs := map[string]JobState{"a-web": job("a-web", "a", "web", 1)}
	allocs := []AllocState{{
		JobID: "a-web", ClientStatus: "pending", DesiredStatus: "run",
		Tasks: []TaskState{{Name: "web", State: "dead", Failed: true, Fail: "Exit Code: 3"}},
	}}

	if s := only(t, Summarize(jobs, allocs, nil)); s.Health != HealthFailed {
		t.Errorf("health = %s, want %s", s.Health, HealthFailed)
	}
}

// A deployment that failed is a fact about the past and Nomad never changes
// it. A task that crashed, exhausted its restart attempts, and then recovered
// once the cause was cleared is running. Reporting it as failed sends you to
// look at a container that is working.
func TestRecoveredTaskOutranksAFailedDeployment(t *testing.T) {
	jobs := map[string]JobState{
		"orca-vector": {ID: "orca-vector", App: "orca", Service: "vector", Count: 1},
	}
	allocs := []AllocState{{
		JobID:        "orca-vector",
		ClientStatus: "running",
		Tasks: []TaskState{{
			Name:     "vector",
			State:    "running",
			Failed:   false,
			Restarts: 8,
			Last:     "Task started by client",
			Fail:     `Exit Code: 78, Exit Message: "Docker container exited with non-zero exit code: 78"`,
		}},
	}}
	deps := []DeploymentState{{
		JobID:       "orca-vector",
		Status:      "failed",
		Description: "Failed due to unhealthy allocations - no stable job version to auto revert to",
	}}

	got := Summarize(jobs, allocs, deps)
	if len(got) != 1 {
		t.Fatalf("got %d statuses", len(got))
	}
	if got[0].Health != HealthOK {
		t.Errorf("health = %q, want %q: the task is running now", got[0].Health, HealthOK)
	}
	// The exit code that sent it into a restart loop is not news once it has
	// come back, and printed beside a healthy service it only misleads.
	if got[0].Message != "" {
		t.Errorf("a healthy service should carry no failure message, got %q", got[0].Message)
	}
}

// The move must not hide a deployment that failed and left the allocations
// broken; only the recovered case changes.
func TestFailedDeploymentWithBrokenAllocsStillFails(t *testing.T) {
	jobs := map[string]JobState{
		"a-web": {ID: "a-web", App: "a", Service: "web", Count: 1},
	}
	allocs := []AllocState{{
		JobID:        "a-web",
		ClientStatus: "failed",
		Tasks: []TaskState{{
			Name: "web", State: "dead", Failed: true,
			Last: "Terminated", Fail: "Exit Code: 1",
		}},
	}}
	deps := []DeploymentState{{JobID: "a-web", Status: "failed", Description: "unhealthy"}}

	got := Summarize(jobs, allocs, deps)
	if got[0].Health != HealthFailed {
		t.Errorf("health = %q, want %q", got[0].Health, HealthFailed)
	}
	if got[0].Message == "" {
		t.Error("a failing service should still say why")
	}
}

// A job that runs on every machine wants one copy per machine, not its
// group's count of 1: three machines must not read "3/1".
func TestSummarizeSystemJobCountsMachines(t *testing.T) {
	sys := job("orca-dns", "orca", "dns", 1)
	sys.System = true
	jobs := map[string]JobState{"orca-dns": sys}

	running := func(node string) AllocState {
		return AllocState{
			JobID: "orca-dns", ClientStatus: "running", DesiredStatus: "run", NodeName: node,
			Tasks: []TaskState{{Name: "dns", State: "running", StartedAt: time.Now().Add(-time.Hour)}},
		}
	}

	s := only(t, Summarize(jobs, []AllocState{running("box0"), running("box1"), running("box2")}, nil))
	if s.Running != 3 || s.Desired != 3 || s.Health != HealthOK {
		t.Errorf("three machines: %d/%d %s, want 3/3 %s", s.Running, s.Desired, s.Health, HealthOK)
	}

	// A copy still starting on a machine that just joined is one short, not
	// healthy.
	starting := AllocState{JobID: "orca-dns", ClientStatus: "pending", DesiredStatus: "run", NodeName: "box2",
		Tasks: []TaskState{{Name: "dns", State: "pending"}}}
	s = only(t, Summarize(jobs, []AllocState{running("box0"), running("box1"), starting}, nil))
	if s.Running != 2 || s.Desired != 3 || s.Health == HealthOK {
		t.Errorf("one starting: %d/%d %s, want 2/3 and not %s", s.Running, s.Desired, s.Health, HealthOK)
	}
}
