package deploy

import (
	"context"
	"fmt"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/internal/manifest"
)

// Nomad runs with its ACLs on, so nothing is answered without a token it
// issued. That is what keeps a container away from the scheduler: Nomad puts
// a socket to its own API inside every task, where no firewall rule applies,
// and with ACLs off it would answer whatever asked.
//
// Three kinds of thing hold a token. orca itself, with the one bootstrap
// keeps on each machine in a file only root can read. Nomad, rendering a
// service's secrets into it under the service's own identity, which the
// container never sees. And three of orca's jobs, which call the API and are
// handed their identity to do it with. The policies here say what the last
// two may do; nothing else may do anything.

// Namespace is the one Nomad namespace everything orca deploys lives in.
const Namespace = "default"

// ACLPolicies is every policy orca keeps in Nomad. Each is attached to the
// jobs it is for rather than to a token, so it applies to whatever those jobs
// run as and there is no token of orca's making to store or rotate.
func ACLPolicies() []*nomad.ACLPolicy {
	return []*nomad.ACLPolicy{
		{
			// Every job: what Nomad may read on a service's behalf to render
			// its files. A group is a namespace, not a security boundary, so
			// this is the cluster's secrets and not one group's.
			Name:        "orca-workloads",
			Description: "Render any service's secrets and certificate into it",
			JobACL:      &nomad.JobACL{Namespace: Namespace},
			Rules: aclRules("",
				aclVars{SecretPrefix + "/*", "read"},
				aclVars{CertPrefix + "/*", "read"}),
		},
		{
			// Ingress routes by the service catalog, and presents every
			// certificate there is.
			Name:        "orca-ingress",
			Description: "Read the catalog and every certificate",
			JobACL:      &nomad.JobACL{Namespace: Namespace, JobID: JobID(manifest.ReservedGroup, "traefik")},
			Rules: aclRules("read",
				aclVars{CertPrefix + "/*", "read", "list"}),
		},
		{
			// The certificate job fills in certificate records, and keeps
			// its account with the authority and the checks in flight.
			Name:        "orca-certs",
			Description: "Issue and renew certificates",
			JobACL:      &nomad.JobACL{Namespace: Namespace, JobID: JobID(manifest.ReservedGroup, "certs")},
			Rules: aclRules("",
				aclVars{CertPrefix + "/*", "read", "write", "list"},
				aclVars{"orca-acme/*", "read", "write", "destroy", "list"}),
		},
		{
			// The status page reads: jobs, allocations, machines. The one
			// thing it writes is the last run of each scheduled job, which
			// it keeps after Nomad has forgotten it.
			Name:        "orca-status",
			Description: "Read what is running and where, and remember scheduled jobs' runs",
			JobACL:      &nomad.JobACL{Namespace: Namespace, JobID: JobID(manifest.ReservedGroup, "status")},
			Rules: aclRules("read",
				aclVars{RunPrefix + "/*", "read", "write", "list"}) + "\nnode {\n  policy = \"read\"\n}\n",
		},
	}
}

// WriteACLPolicies makes Nomad's copy of each policy the one above. Written
// by bootstrap, before anything is deployed, and again by every apply, so a
// build of orca whose jobs need something new is allowed it by the apply that
// deploys them.
func WriteACLPolicies(ctx context.Context, c *nomad.Client) error {
	policies := ACLPolicies()
	w := (&nomad.WriteOptions{}).WithContext(ctx)
	return each(len(policies), func(i int) error {
		if _, err := c.ACLPolicies().Upsert(policies[i], w); err != nil {
			return fmt.Errorf("write policy %s: %w", policies[i].Name, err)
		}
		return nil
	})
}

// aclVars grants capabilities on the variables matching a path.
type aclVars []string

// aclRules writes a policy's rules for the namespace: a policy for its jobs
// ("" for none), and what it may do with which variables.
func aclRules(jobs string, vars ...aclVars) string {
	s := fmt.Sprintf("namespace %q {\n", Namespace)
	if jobs != "" {
		s += fmt.Sprintf("  policy = %q\n", jobs)
	}
	if len(vars) > 0 {
		s += "  variables {\n"
		for _, v := range vars {
			s += fmt.Sprintf("    path %q {\n      capabilities = [", v[0])
			for i, c := range v[1:] {
				if i > 0 {
					s += ", "
				}
				s += fmt.Sprintf("%q", c)
			}
			s += "]\n    }\n"
		}
		s += "  }\n"
	}
	return s + "}\n"
}

// exposeIdentity hands a task its own identity as NOMAD_TOKEN, for a task
// that calls Nomad's API itself. Only orca's own jobs are given one, each
// with a policy above that says what it is good for.
func exposeIdentity(task *nomad.Task) {
	task.Identity = &nomad.WorkloadIdentity{Env: true}
}
