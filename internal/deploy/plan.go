package deploy

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/internal/manifest"
)

// JobState is what the cluster currently knows about one orca-managed job.
type JobState struct {
	ID      string
	Group   string
	Service string
	Image   string
	Stopped bool

	// ImageRef is the image as the manifest named it when it was deployed,
	// before it was pinned to Image.
	ImageRef string

	// Version is the job's current version in Nomad, which every allocation
	// and deployment records. Waiting for a deploy means waiting for this
	// version, not for whatever happens to be running.
	Version uint64

	// Count is the desired replica count, so status can report 1/1 rather than
	// just "something is running".
	Count int

	// System marks a job that runs one copy on every machine. Its Count is
	// per machine, so what it should have is however many machines Nomad
	// placed it on, not Count.
	System bool

	// Periodic marks a job that runs on a schedule. It has no allocation
	// until it next fires, which is not the same thing as having failed to
	// get one.
	Periodic bool

	// AuthHash is the bcrypt hash ingress is currently using for the
	// dashboards. Read back so an unchanged password produces an unchanged
	// job spec; see resolveAuthHash.
	AuthHash string
}

// ChangeKind is what apply will do to one service.
type ChangeKind string

const (
	ChangeNone   ChangeKind = "unchanged"
	ChangeCreate ChangeKind = "create"
	ChangeUpdate ChangeKind = "update"
	ChangeStop   ChangeKind = "stop"
)

// Change is one service's worth of work.
type Change struct {
	Kind    ChangeKind
	JobID   string
	Group   string
	Service string

	OldImage string
	NewImage string

	// Job is the spec to submit. Nil for ChangeStop.
	Job *nomad.Job

	// Plan is Nomad's dry run of submitting Job. Zero for ChangeStop.
	Plan JobPlan
}

// Plan is everything apply intends to do, in a stable order.
type Plan struct {
	Changes []Change
}

// Work returns only the changes that do something, which is what apply acts on
// and what a CI log should show.
func (p Plan) Work() []Change {
	var out []Change
	for _, c := range p.Changes {
		if c.Kind != ChangeNone {
			out = append(out, c)
		}
	}
	return out
}

func (p Plan) HasWork() bool { return len(p.Work()) > 0 }

// Unplaceable is every change Nomad could not find room for. Submitting one
// would stop what runs now and then wait for capacity that is not coming, so
// apply refuses the whole plan instead.
func (p Plan) Unplaceable() []Change {
	var out []Change
	for _, c := range p.Work() {
		if c.Plan.Unplaceable != "" {
			out = append(out, c)
		}
	}
	return out
}

// String renders the plan for a human. Unchanged services are summarised rather
// than listed: most applies change one thing, and a wall of "unchanged" buries
// the line that matters.
func (p Plan) String() string {
	work := p.Work()
	if len(work) == 0 {
		return fmt.Sprintf("%d service(s), nothing to do", len(p.Changes))
	}

	var b strings.Builder
	for _, c := range work {
		switch c.Kind {
		case ChangeCreate:
			fmt.Fprintf(&b, "  create  %s/%s  %s\n", c.Group, c.Service, c.NewImage)
		case ChangeStop:
			fmt.Fprintf(&b, "  stop    %s/%s  (no longer declared)\n", c.Group, c.Service)
		case ChangeUpdate:
			if c.OldImage != c.NewImage {
				fmt.Fprintf(&b, "  update  %s/%s  %s -> %s\n", c.Group, c.Service, shortImage(c.OldImage), shortImage(c.NewImage))
			} else {
				fmt.Fprintf(&b, "  update  %s/%s  %s\n", c.Group, c.Service, c.Plan.describe())
			}
		}
		if c.Plan.Unplaceable != "" {
			fmt.Fprintf(&b, "          cannot be placed: %s\n", c.Plan.Unplaceable)
		}
	}
	fmt.Fprintf(&b, "\n%d change(s), %d unchanged", len(work), len(p.Changes)-len(work))
	return b.String()
}

// shortImage trims a digest to something readable while staying unambiguous.
func shortImage(ref string) string {
	repo, digest, ok := strings.Cut(ref, "@sha256:")
	if !ok {
		return ref
	}
	if len(digest) > 12 {
		digest = digest[:12]
	}
	return repo + "@" + digest
}

// BuildGroup renders every service of one group into a job. images maps a service
// name to its already-resolved, digest-pinned image; nodeFor decides placement
// and may return "" to leave it to Nomad.
func BuildGroup(m *manifest.Manifest, images map[string]string, opts Options, nodeFor func(*manifest.Service) (string, error)) ([]*nomad.Job, error) {
	var jobs []*nomad.Job
	var errs []error

	for _, s := range m.Services {
		// A target names somewhere outside the cluster. There is no container,
		// so there is nothing to schedule: it exists to be referred to.
		if s.IsTarget() {
			continue
		}

		image, ok := images[s.Name]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: service %q: no resolved image", m.Path, s.Name))
			continue
		}

		node, err := nodeFor(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: service %q: %w", m.Path, s.Name, err))
			continue
		}

		o := opts
		o.Node = node

		if s.UsesIngress() && !o.Ingress {
			errs = append(errs, fmt.Errorf(
				"%s: service %q: routes a port through ingress, which is switched off in cluster.yaml",
				m.Path, s.Name))
			continue
		}

		job, err := Build(m, s, image, o)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		jobs = append(jobs, job)
	}

	return jobs, errors.Join(errs...)
}

// BuildPlan diffs the desired jobs against what the cluster is running.
//
// Whether a job changed is Nomad's answer, from plans: a dry run of
// submitting each one. Nomad compares the job with the version it holds after
// normalizing both, so it is right where a comparison made here would not be,
// such as a new field in a newer client library, and it knows which changes
// replace allocations and which it applies in place.
//
// scope names the groups this apply covers, and a nil scope covers everything.
// A job outside the scope is left alone entirely. That is what makes
// `orca apply blog` safe to run when other groups exist, and it is the reason
// scope is an explicit argument rather than being inferred from the desired
// set.
//
// nil is the whole-cluster case and has to be distinct from "the set of groups
// that currently have a directory". Deriving the scope from what is on disk
// looks equivalent and is not: the jobs that most need stopping belong to a
// group whose directory is *gone*, so they would never be in the derived set
// and apply could not see them. Renaming a group or deleting any directory
// must still stop what it left behind, the platform's jobs included.
func BuildPlan(desired []*nomad.Job, current map[string]JobState, plans map[string]JobPlan, scope map[string]bool) Plan {
	var plan Plan
	seen := map[string]bool{}

	for _, job := range desired {
		id := *job.ID
		seen[id] = true

		c := Change{
			JobID:    id,
			Group:    job.Meta[MetaGroup],
			Service:  job.Meta[MetaService],
			NewImage: job.Meta[MetaImage],
			Job:      job,
			Plan:     plans[id],
		}

		// A job with no plan is submitted: Nomad treats an identical job as a
		// no-op, so the cost of being wrong is a round trip, where skipping it
		// could leave a change undeployed.
		p, planned := plans[id]
		switch cur, running := current[id]; {
		case !running || cur.Stopped:
			c.Kind = ChangeCreate
		case planned && !p.Changed:
			c.Kind = ChangeNone
			c.OldImage = cur.Image
		default:
			c.Kind = ChangeUpdate
			c.OldImage = cur.Image
		}

		plan.Changes = append(plan.Changes, c)
	}

	// Anything orca owns, inside the scope, that the manifests no longer
	// declare. The manifest is desired state, so this is how a deleted service
	// actually stops. It stops only: the volume is kept, and only purge
	// deletes data.
	for id, cur := range current {
		if seen[id] || cur.Stopped {
			continue
		}
		if scope != nil && !scope[cur.Group] {
			continue
		}
		plan.Changes = append(plan.Changes, Change{
			Kind:     ChangeStop,
			JobID:    id,
			Group:    cur.Group,
			Service:  cur.Service,
			OldImage: cur.Image,
		})
	}

	sort.Slice(plan.Changes, func(i, j int) bool { return plan.Changes[i].JobID < plan.Changes[j].JobID })
	return plan
}

// Stops is every service this plan would stop.
//
// Separated out because stopping is the only part of an apply a person might
// not have meant: a create or an update comes from something they wrote, while
// a stop comes from something they did not write — including, when the working
// directory is wrong, everything they did not write *here*.
func (p Plan) Stops() []Change {
	var out []Change
	for _, c := range p.Changes {
		if c.Kind == ChangeStop {
			out = append(out, c)
		}
	}
	return out
}

// VanishedGroups names the groups this plan would stop entirely, sorted. A
// group losing one service is an ordinary edit; a group disappearing whole is
// either a deliberate removal or an apply run from the wrong directory.
func (p Plan) VanishedGroups() []string {
	declared := map[string]bool{}
	stopping := map[string]bool{}
	for _, c := range p.Changes {
		if c.Kind == ChangeStop {
			stopping[c.Group] = true
		} else {
			declared[c.Group] = true
		}
	}
	var out []string
	for group := range stopping {
		if !declared[group] {
			out = append(out, group)
		}
	}
	sort.Strings(out)
	return out
}
