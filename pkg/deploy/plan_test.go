package deploy

import (
	"strings"
	"testing"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/pkg/manifest"
)

func jobsFor(t *testing.T, body string, images map[string]string) []*nomad.Job {
	return jobsForIn(t, "blog", body, images)
}

func jobsForIn(t *testing.T, group, body string, images map[string]string) []*nomad.Job {
	t.Helper()
	m := parseIn(t, group, body)
	jobs, err := BuildApp(m, images, defaultOpts(), func(*manifest.Service) (string, error) { return "box0", nil })
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return jobs
}

const twoServices = `
name: api
image: ghcr.io/x/blog:1.4
---
name: web
image: ghcr.io/x/web:1.0
`

func state(jobs []*nomad.Job) map[string]JobState {
	out := map[string]JobState{}
	for _, j := range jobs {
		out[*j.ID] = JobState{
			ID:      *j.ID,
			App:     j.Meta[MetaApp],
			Service: j.Meta[MetaService],
			Hash:    j.Meta[MetaHash],
			Image:   j.Meta[MetaImage],
		}
	}
	return out
}

func TestPlanCreatesWhenNothingIsRunning(t *testing.T) {
	jobs := jobsFor(t, twoServices, map[string]string{"api": "a@sha256:1", "web": "b@sha256:2"})

	plan := BuildPlan(jobs, map[string]JobState{}, map[string]bool{"blog": true})

	if len(plan.Work()) != 2 {
		t.Fatalf("want 2 creates, got: %s", plan)
	}
	for _, c := range plan.Work() {
		if c.Kind != ChangeCreate {
			t.Errorf("%s/%s = %s, want create", c.App, c.Service, c.Kind)
		}
	}
}

// The property that lets apply run from CI on every commit: applying the same
// manifests twice must do nothing at all the second time.
func TestPlanIsANoOpWhenNothingChanged(t *testing.T) {
	images := map[string]string{"api": "a@sha256:1", "web": "b@sha256:2"}
	jobs := jobsFor(t, twoServices, images)

	plan := BuildPlan(jobsFor(t, twoServices, images), state(jobs), map[string]bool{"blog": true})

	if plan.HasWork() {
		t.Errorf("re-applying unchanged manifests should do nothing, got:\n%s", plan)
	}
	if !strings.Contains(plan.String(), "nothing to do") {
		t.Errorf("a no-op plan should say so, got: %s", plan)
	}
}

// A moved tag is the case mutable-tag deploys get wrong: the manifest is
// byte-identical, and only the resolved digest reveals that anything changed.
func TestPlanDetectsAMovedTag(t *testing.T) {
	before := jobsFor(t, twoServices, map[string]string{"api": "a@sha256:1", "web": "b@sha256:2"})
	after := jobsFor(t, twoServices, map[string]string{"api": "a@sha256:99", "web": "b@sha256:2"})

	plan := BuildPlan(after, state(before), map[string]bool{"blog": true})

	work := plan.Work()
	if len(work) != 1 {
		t.Fatalf("want exactly the changed service, got:\n%s", plan)
	}
	if work[0].Service != "api" || work[0].Kind != ChangeUpdate {
		t.Errorf("change = %+v, want an update to api", work[0])
	}
	if work[0].OldImage != "a@sha256:1" || work[0].NewImage != "a@sha256:99" {
		t.Errorf("want the digests reported, got %s -> %s", work[0].OldImage, work[0].NewImage)
	}
}

// The file is desired state, so a service deleted from it stops.
func TestPlanStopsUndeclaredServices(t *testing.T) {
	before := jobsFor(t, twoServices, map[string]string{"api": "a@sha256:1", "web": "b@sha256:2"})
	after := jobsFor(t, `
name: api
image: ghcr.io/x/blog:1.4
`, map[string]string{"api": "a@sha256:1"})

	plan := BuildPlan(after, state(before), map[string]bool{"blog": true})

	work := plan.Work()
	if len(work) != 1 || work[0].Kind != ChangeStop || work[0].Service != "web" {
		t.Fatalf("want web stopped, got:\n%s", plan)
	}
	if work[0].Job != nil {
		t.Error("a stop carries no job spec")
	}
}

// `orca apply blog` must not touch anything else. Without the scope check, an
// app absent from the desired set would look undeclared and get stopped.
func TestPlanLeavesOtherAppsAlone(t *testing.T) {
	blog := jobsFor(t, twoServices, map[string]string{"api": "a@sha256:1", "web": "b@sha256:2"})
	other := jobsForIn(t, "bot", "{name: bot, image: c:1}", map[string]string{"bot": "c@sha256:3"})

	current := state(append(append([]*nomad.Job{}, blog...), other...))

	// Applying only blog, and blog declares nothing any more.
	plan := BuildPlan(nil, current, map[string]bool{"blog": true})

	for _, c := range plan.Work() {
		if c.App != "blog" {
			t.Errorf("plan touched %s/%s, which is outside the scope", c.App, c.Service)
		}
	}
	if len(plan.Work()) != 2 {
		t.Errorf("want both blog services stopped, got:\n%s", plan)
	}
}

// A stopped job is recreated rather than reported unchanged, so `orca apply`
// is how you bring something back.
func TestPlanRecreatesAStoppedJob(t *testing.T) {
	jobs := jobsFor(t, twoServices, map[string]string{"api": "a@sha256:1", "web": "b@sha256:2"})
	current := state(jobs)
	for id, s := range current {
		s.Stopped = true
		current[id] = s
	}

	plan := BuildPlan(jobs, current, map[string]bool{"blog": true})

	for _, c := range plan.Work() {
		if c.Kind != ChangeCreate {
			t.Errorf("%s = %s, want create", c.Service, c.Kind)
		}
	}
}

// A nil scope is the whole-cluster apply, and it has to reach jobs whose group
// no longer exists on disk. That is exactly the set that most needs
// stopping, and exactly the set a scope derived from the directories could
// never contain.
func TestNilScopeStopsGroupsThatNoLongerExist(t *testing.T) {
	current := map[string]JobState{
		"gone-web":  {ID: "gone-web", App: "gone", Service: "web"},
		"shop-app":  {ID: "shop-app", App: "shop", Service: "app", Hash: "h"},
		"old-store": {ID: "old-store", App: "old", Service: "store"},
	}
	desired := []*nomad.Job{{
		ID:   ptr("shop-app"),
		Meta: map[string]string{MetaApp: "shop", MetaService: "app", MetaHash: "h"},
	}}

	plan := BuildPlan(desired, current, nil)

	stopped := map[string]bool{}
	for _, c := range plan.Changes {
		if c.Kind == ChangeStop {
			stopped[c.JobID] = true
		}
	}
	for _, want := range []string{"gone-web", "old-store"} {
		if !stopped[want] {
			t.Errorf("%s belongs to no declared group and should be stopped; plan:\n%s", want, plan.String())
		}
	}
	if stopped["shop-app"] {
		t.Error("shop-app is still declared and must not be stopped")
	}
}

// A narrowed apply still touches nothing outside the groups it was given.
// This is the property the nil case must not cost.
func TestNamedScopeLeavesOtherGroupsAlone(t *testing.T) {
	current := map[string]JobState{
		"gone-web": {ID: "gone-web", App: "gone", Service: "web"},
		"shop-app": {ID: "shop-app", App: "shop", Service: "app"},
	}

	plan := BuildPlan(nil, current, map[string]bool{"shop": true})

	for _, c := range plan.Changes {
		if c.Kind == ChangeStop && c.JobID != "shop-app" {
			t.Errorf("a narrowed apply stopped %s, which is outside its scope", c.JobID)
		}
	}
	// And it does stop what is inside the scope and no longer declared.
	var stoppedShop bool
	for _, c := range plan.Changes {
		if c.Kind == ChangeStop && c.JobID == "shop-app" {
			stoppedShop = true
		}
	}
	if !stoppedShop {
		t.Error("shop-app is in scope and undeclared, so it should be stopped")
	}
}

// Naming a group whose directory is gone is how you remove it by name.
func TestNamedScopeCanStopADeletedGroup(t *testing.T) {
	current := map[string]JobState{
		"gone-web": {ID: "gone-web", App: "gone", Service: "web"},
		"shop-app": {ID: "shop-app", App: "shop", Service: "app"},
	}

	plan := BuildPlan(nil, current, map[string]bool{"gone": true})

	stopped := map[string]bool{}
	for _, c := range plan.Changes {
		if c.Kind == ChangeStop {
			stopped[c.JobID] = true
		}
	}
	if !stopped["gone-web"] {
		t.Error("naming a deleted group should stop its jobs")
	}
	if stopped["shop-app"] {
		t.Error("shop was not named and must be left alone")
	}
}

// A group losing one service is an ordinary edit. A group disappearing whole
// is either a deliberate removal or an apply run from the wrong directory, and
// only the second kind is worth interrupting someone for.
func TestVanishedGroupsNamesOnlyWholeGroups(t *testing.T) {
	plan := Plan{Changes: []Change{
		{Kind: ChangeStop, App: "shop", Service: "worker"},
		{Kind: ChangeNone, App: "shop", Service: "app"},
		{Kind: ChangeStop, App: "files", Service: "store"},
		{Kind: ChangeStop, App: "demo", Service: "whoami"},
		{Kind: ChangeUpdate, App: "orca", Service: "dns"},
	}}

	got := plan.VanishedGroups()
	want := []string{"demo", "files"}
	if len(got) != len(want) {
		t.Fatalf("VanishedGroups() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("VanishedGroups() = %v, want %v", got, want)
		}
	}

	if n := len(plan.Stops()); n != 3 {
		t.Errorf("Stops() = %d, want 3", n)
	}
}
