package deploy

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	nomad "github.com/hashicorp/nomad/api"
)

// JobPlan is Nomad's answer to "what would submitting this job do", from a
// dry run of its scheduler.
type JobPlan struct {
	// Changed reports that the job differs from the version Nomad holds.
	Changed bool

	// Restart reports that submitting it replaces running allocations,
	// rather than updating them in place or not touching them at all.
	Restart bool

	// Fields names what changed, for a person to read.
	Fields []string

	// Unplaceable explains what Nomad could not find room for, empty when it
	// could place everything.
	Unplaceable string

	// JobModifyIndex is the version of the job the plan was made against,
	// zero for a job Nomad does not have. Submitting with it refuses if
	// anything else changed the job in between, so a stale plan cannot
	// overwrite a newer deploy.
	JobModifyIndex uint64
}

// planFieldsShown bounds how many changed fields a plan line names.
const planFieldsShown = 4

// describe is the plan line's account of a change that is not an image.
func (p JobPlan) describe() string {
	how := "in place"
	if p.Restart {
		how = "restart"
	}
	if len(p.Fields) == 0 {
		return how
	}
	fields := p.Fields
	more := ""
	if len(fields) > planFieldsShown {
		more = fmt.Sprintf(" +%d more", len(fields)-planFieldsShown)
		fields = fields[:planFieldsShown]
	}
	return how + ": " + strings.Join(fields, ", ") + more
}

// JobPlanFromNomad reads Nomad's plan response.
func JobPlanFromNomad(r *nomad.JobPlanResponse) JobPlan {
	p := JobPlan{JobModifyIndex: r.JobModifyIndex}
	if r.Diff != nil && r.Diff.Type != "None" {
		p.Changed = true
		p.Fields = diffFields(r.Diff)
	}
	if r.Annotations != nil {
		for _, u := range r.Annotations.DesiredTGUpdates {
			if u != nil && (u.DestructiveUpdate > 0 || u.Canary > 0) {
				p.Restart = true
			}
		}
	}
	var failed []string
	for _, tg := range slices.Sorted(maps.Keys(r.FailedTGAllocs)) {
		if why := describePlacement(r.FailedTGAllocs[tg]); why != "" {
			failed = append(failed, why)
		}
	}
	p.Unplaceable = strings.Join(failed, "; ")
	return p
}

// diffFields flattens a job diff into the names of what changed, in the order
// Nomad lists them, once each. Groups and tasks are left out of the names:
// every job orca builds has one group, and naming its tasks says less than
// naming the field.
//
// orca's own bookkeeping in the job's meta is left out too. It changes only
// alongside something that is named already: the image, or the config that
// carries the dashboard password.
func diffFields(d *nomad.JobDiff) []string {
	var out []string
	add := func(name string) {
		if !strings.HasPrefix(name, "Meta[orca.") && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	var fields func(prefix string, fs []*nomad.FieldDiff, os []*nomad.ObjectDiff)
	fields = func(prefix string, fs []*nomad.FieldDiff, os []*nomad.ObjectDiff) {
		for _, f := range fs {
			if f != nil && f.Type != "None" {
				add(prefix + f.Name)
			}
		}
		for _, o := range os {
			if o != nil && o.Type != "None" {
				fields(prefix+o.Name+".", o.Fields, o.Objects)
			}
		}
	}

	fields("", d.Fields, d.Objects)
	for _, g := range d.TaskGroups {
		if g == nil || g.Type == "None" {
			continue
		}
		fields("", g.Fields, g.Objects)
		for _, t := range g.Tasks {
			switch {
			case t == nil || t.Type == "None":
			case t.Type == "Added" || t.Type == "Deleted":
				// Every field of a task that is new or gone is new or gone
				// with it; the task is what changed.
				add(strings.ToLower(t.Type) + " task " + t.Name)
			default:
				fields("", t.Fields, t.Objects)
			}
		}
	}
	return out
}
