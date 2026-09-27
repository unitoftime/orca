package deploy

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/pkg/manifest"
)

// JobState is what the cluster currently knows about one orca-managed job.
type JobState struct {
	ID      string
	App     string
	Service string
	Hash    string
	Image   string
	Stopped bool

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
	// job spec — see resolveAuthHash.
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
	App     string
	Service string

	OldImage string
	NewImage string

	// Job is the spec to submit. Nil for ChangeStop.
	Job *nomad.Job
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
			fmt.Fprintf(&b, "  create  %s/%s  %s\n", c.App, c.Service, c.NewImage)
		case ChangeStop:
			fmt.Fprintf(&b, "  stop    %s/%s  (no longer declared)\n", c.App, c.Service)
		case ChangeUpdate:
			if c.OldImage != c.NewImage {
				fmt.Fprintf(&b, "  update  %s/%s  %s -> %s\n", c.App, c.Service, shortImage(c.OldImage), shortImage(c.NewImage))
			} else {
				fmt.Fprintf(&b, "  update  %s/%s  (spec changed)\n", c.App, c.Service)
			}
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

// BuildApp renders every service of one group into a job. images maps a service
// name to its already-resolved, digest-pinned image; nodeFor decides placement
// and may return "" to leave it to Nomad.
func BuildApp(m *manifest.Manifest, images map[string]string, opts Options, nodeFor func(*manifest.Service) (string, error)) ([]*nomad.Job, error) {
	var jobs []*nomad.Job
	var errs []error

	for _, s := range m.Services {
		// A target names somewhere outside the cluster. There is no container,
		// so there is nothing to schedule — it exists to be referred to.
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
// scope names the apps this apply covers, and a nil scope covers everything.
// A job outside the scope is left alone entirely — that is what makes
// `orca apply blog` safe to run when other apps exist, and it is the reason
// scope is an explicit argument rather than being inferred from the desired
// set.
//
// nil is the whole-cluster case and has to be distinct from "the set of groups
// that currently have a directory". Deriving the scope from what is on disk
// looks equivalent and is not: the jobs that most need stopping belong to a
// group whose directory is *gone*, so they were never in the derived set and
// apply could not see them. A renamed group did it to the platform; deleting
// any directory did it to anything.
func BuildPlan(desired []*nomad.Job, current map[string]JobState, scope map[string]bool) Plan {
	var plan Plan
	seen := map[string]bool{}

	for _, job := range desired {
		id := *job.ID
		seen[id] = true

		c := Change{
			JobID:    id,
			App:      job.Meta[MetaApp],
			Service:  job.Meta[MetaService],
			NewImage: job.Meta[MetaImage],
			Job:      job,
		}

		switch cur, running := current[id]; {
		case !running || cur.Stopped:
			c.Kind = ChangeCreate
		case cur.Hash == job.Meta[MetaHash]:
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
	// actually stops — and it stops only; the volume is kept, and only purge
	// deletes data.
	for id, cur := range current {
		if seen[id] || cur.Stopped {
			continue
		}
		if scope != nil && !scope[cur.App] {
			continue
		}
		plan.Changes = append(plan.Changes, Change{
			Kind:     ChangeStop,
			JobID:    id,
			App:      cur.App,
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
			stopping[c.App] = true
		} else {
			declared[c.App] = true
		}
	}
	var out []string
	for app := range stopping {
		if !declared[app] {
			out = append(out, app)
		}
	}
	sort.Strings(out)
	return out
}
