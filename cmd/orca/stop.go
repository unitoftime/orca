package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/unitoftime/orca/internal/deploy"
	"github.com/unitoftime/orca/internal/manifest"
)

// cmdStop takes a group down without deleting anything it wrote.
//
// The fast way off the machine: something is misbehaving and you want it down
// now, without editing files and waiting for an apply. Because the manifests
// are unchanged, the next apply brings it back, which is said out loud rather
// than left to be discovered.
func cmdStop(ctx context.Context, cfg Config, group string) error {
	cluster, err := clusterFor(ctx, cfg)
	if err != nil {
		return err
	}

	jobs, err := groupJobs(ctx, cluster, group)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return fmt.Errorf("nothing deployed for group %q", group)
	}

	for _, id := range jobs {
		fmt.Printf("  stop    %s ... ", id)
		if err := cluster.Stop(ctx, id); err != nil {
			fmt.Println("FAILED")
			return err
		}
		fmt.Println("ok")
	}

	fmt.Printf("\nstopped %d service(s). Data is untouched.\n", len(jobs))
	if yes, err := declared(cfg, group); err == nil && yes {
		fmt.Printf("The manifests still declare %q, so `orca apply` will bring it back.\n", group)
		fmt.Printf("Delete %s/%s to remove it for good.\n", cfg.Root, group)
	}
	return nil
}

// forgetRun deletes the last run remembered for a job that has been removed,
// so a scheduled job declared again later does not start with another's
// history. `orca stop` keeps it: that job comes back as it was. Not fatal: a
// record left behind is a line of text nothing reads.
func forgetRun(ctx context.Context, cluster *Cluster, jobID string) {
	if err := cluster.deleteVariable(ctx, deploy.RunPath(jobID)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not forget the last run of %s: %v\n", jobID, err)
	}
}

// cmdPurge deletes a group's data.
//
// The one irreversible verb. It refuses while the manifests still declare the
// group, so the only way to reach it is to have already deleted the
// directory, which makes an accidental purge take two deliberate steps rather
// than one mistyped word.
func cmdPurge(ctx context.Context, cfg Config, in invocation) error {
	yes, group := in.Has(flagYes), in.Arg(0)

	// The reserved group is never "declared" (it has no directory), so the
	// rule below cannot protect it, and without this `orca purge orca` would
	// take ingress, the resolver and both stores with one confirmed word.
	// Turning a capability off in cluster.yaml is the way to remove one, and
	// the ordinary "stop what is no longer declared" rule does it.
	if group == manifest.ReservedGroup {
		return fmt.Errorf(
			"%q holds the jobs orca runs for you, and is not yours to purge\n\n"+
				"turn one off in %s/%s instead (`ingress: false`, `monitoring: false`) "+
				"and apply removes it, keeping its data",
			group, cfg.Root, manifest.ClusterFile)
	}

	// Fails closed. If the manifests cannot be read, whether this group is
	// still declared is unknown, and a purge guard that answers "no" when it
	// does not know is one a typo in some other group's file switches off.
	isDeclared, err := declared(cfg, group)
	if err != nil {
		return fmt.Errorf("purge refuses to run while the manifests do not load, "+
			"because it cannot tell which data is still in use:\n%w", err)
	}
	if isDeclared {
		return fmt.Errorf(
			"group %q is still declared in %s/%s\n\n"+
				"purge deletes data permanently, so it only works on a group you have already removed.\n"+
				"delete the directory and run `orca apply` first, then purge.",
			group, cfg.Root, group)
	}

	cluster, err := clusterFor(ctx, cfg)
	if err != nil {
		return err
	}

	jobs, err := groupJobs(ctx, cluster, group)
	if err != nil {
		return err
	}
	orphans, err := findOrphans(ctx, cfg)
	if err != nil {
		return err
	}
	// Exact group match: a volume's group is its directory, not a prefix of
	// its name, so purging "shop" can never reach "shop-prod".
	var mine []nodeVolume
	for _, o := range orphans {
		if o.Group == group {
			mine = append(mine, o)
		}
	}
	secrets, err := groupSecrets(ctx, cluster, group)
	if err != nil {
		return err
	}

	if len(jobs) == 0 && len(mine) == 0 && len(secrets) == 0 {
		fmt.Printf("nothing left of %q to purge\n", group)
		return nil
	}

	// Named before it is asked, so the answer is to a specific question.
	fmt.Printf("This will permanently delete:\n")
	for _, id := range jobs {
		fmt.Printf("  job     %s\n", id)
	}
	for _, v := range mine {
		fmt.Printf("  DATA    %s:%s\n", v.Node, v.Path())
	}
	for _, s := range secrets {
		fmt.Printf("  secret  %s\n", s)
	}

	if err := confirmTyped("\npermanently delete the above? ", group, yes); err != nil {
		return err
	}

	for _, id := range jobs {
		if err := cluster.Stop(ctx, id); err != nil {
			return err
		}
		forgetRun(ctx, cluster, id)
	}
	for _, s := range secrets {
		g, n, err := parseSecretRef(s)
		if err != nil {
			return err
		}
		if err := cluster.DeleteSecret(ctx, g, n); err != nil {
			return err
		}
	}
	byHost := map[string][]Volume{}
	for _, v := range mine {
		byHost[v.Host] = append(byHost[v.Host], v.Volume)
	}
	for host, vols := range byHost {
		if err := (Node{Host: host}).DeleteVolumes(ctx, vols); err != nil {
			return err
		}
	}

	fmt.Printf("\npurged %q\n", group)
	return nil
}

// declared reports whether the manifests still define this group.
//
// An error is returned rather than folded into "no": purge's whole safety
// rests on this answer, and a manifest that fails to parse must not read as
// every group having been deleted.
func declared(cfg Config, group string) (bool, error) {
	groups, err := discoverGroups(cfg)
	if err != nil {
		return false, err
	}
	for _, m := range groups {
		if m.Group == group {
			return true, nil
		}
	}
	return false, nil
}

// declaredVolumes is every volume the manifests still claim.
func declaredVolumes(cfg Config) (map[Volume]bool, error) {
	groups, err := discoverGroups(cfg)
	if err != nil {
		return nil, err
	}
	vols := map[Volume]bool{}
	for _, m := range groups {
		for _, s := range m.Services {
			if s.Volume != nil {
				vols[Volume{Group: m.Group, Service: s.Name}] = true
			}
		}
	}
	return vols, nil
}

// nodeVolume is a volume and the machine holding it.
type nodeVolume struct {
	Volume
	Node string
	Host string
}

// findOrphans lists data whose service is no longer declared, on every
// machine. Every one, not just the machine orca is talking to: a volume lives
// where its service was pinned, and data on the second machine is no less
// kept than data on the first.
func findOrphans(ctx context.Context, cfg Config) ([]nodeVolume, error) {
	vols, err := declaredVolumes(cfg)
	if err != nil {
		return nil, err
	}
	var out []nodeVolume
	for _, nc := range cfg.Nodes {
		held, err := nc.Node().ListVolumes(ctx)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", nc.Name, err)
		}
		for _, v := range orphansOf(held, vols) {
			out = append(out, nodeVolume{Volume: v, Node: nc.Name, Host: nc.Host})
		}
	}
	return out, nil
}

// orphansOf is the comparison on its own, so it is testable without a machine.
func orphansOf(held []Volume, declared map[Volume]bool) []Volume {
	var out []Volume
	for _, v := range held {
		if !declared[v] {
			out = append(out, v)
		}
	}
	return out
}

// groupJobs is every orca-managed job belonging to a group.
func groupJobs(ctx context.Context, cluster *Cluster, group string) ([]string, error) {
	jobs, err := cluster.Jobs(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for id, j := range jobs {
		if j.Group == group {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// groupSecrets is every secret stored under a group.
func groupSecrets(ctx context.Context, cluster *Cluster, group string) ([]string, error) {
	paths, err := cluster.SecretPaths(ctx)
	if err != nil {
		return nil, err
	}
	prefix := deploy.SecretPrefix + "/" + group + "/"
	var out []string
	for p := range paths {
		if strings.HasPrefix(p, prefix) {
			out = append(out, strings.TrimPrefix(p, deploy.SecretPrefix+"/"))
		}
	}
	sort.Strings(out)
	return out, nil
}
