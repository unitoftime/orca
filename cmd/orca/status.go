package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/unitoftime/orca/pkg/deploy"
	"github.com/unitoftime/orca/pkg/manifest"
	"github.com/unitoftime/orca/pkg/statuspage"
)

// HealthTimeout bounds how long apply waits for services it did not change
// to settle.
//
// Bounded, not infinite: a cold image pull is slow and perfectly healthy, while
// a crash loop never resolves. Waiting forever would leave apply hanging on
// the first crash loop it meets.
const HealthTimeout = 2 * time.Minute

// RolloutTimeout bounds how long apply waits for what it submitted. Nomad
// fails a rollout that has made no progress by its deadline and rolls it
// back, so waiting a little past that is waiting for Nomad's verdict rather
// than giving up before Nomad has one.
const RolloutTimeout = deploy.ProgressDeadline + time.Minute

// healthPoll is how often the cluster is asked. Two seconds is frequent enough
// to feel live and rare enough that an SSH round trip per poll costs nothing
// worth counting.
const healthPoll = 2 * time.Second

// cmdStatus reports what is actually running, as opposed to what was last
// submitted. A matching spec hash says a deploy happened, not that anything
// works. This is the command that answers the second question.
func cmdStatus(ctx context.Context, cfg Config, args []string) error {
	cluster, err := clusterFor(ctx, cfg)
	if err != nil {
		return err
	}

	statuses, err := readStatus(ctx, cluster)
	if err != nil {
		return err
	}

	var want map[string]bool
	if len(args) > 0 {
		want = map[string]bool{}
		for _, a := range args {
			want[a] = true
		}
		var filtered []deploy.ServiceStatus
		for _, s := range statuses {
			if want[s.App] {
				filtered = append(filtered, s)
			}
		}
		statuses = filtered
	}

	if len(statuses) == 0 {
		fmt.Println("nothing deployed")
		return nil
	}

	// Explain an unplaced service rather than leaving it looking merely slow.
	// The reason is in the evaluation, which is not anywhere a person would
	// think to look: the task has no logs because it never started.
	for i := range statuses {
		if statuses[i].Health == deploy.HealthUnplaced {
			if why := cluster.PlacementFailure(ctx, statuses[i].JobID); why != "" {
				statuses[i].Message = why
			}
		}
	}

	// Data whose service no longer exists. Removing a service keeps its data
	// deliberately, so this is what stops "kept" from meaning "invisible",
	// and it is the only way to find out that `orca purge` has something to
	// do. When the manifests do not load, what is declared is unknown, so it
	// says that instead of calling every volume orphaned.
	orphans, orphanErr := findOrphans(ctx, cfg)
	defer func() {
		if orphanErr != nil {
			fmt.Printf("\ncould not check for data left by removed services: %v\n", orphanErr)
			return
		}
		if len(orphans.Volumes) == 0 {
			return
		}
		fmt.Printf("\ndata kept for services that are no longer declared:\n")
		for _, o := range orphans.Volumes {
			fmt.Printf("  %s:%s\n", o.Node, o.Path())
		}
		fmt.Printf("remove it with: orca purge <group>\n")
	}()

	app := ""
	for _, s := range statuses {
		if s.App != app {
			if app != "" {
				fmt.Println()
			}
			fmt.Println(s.App)
			app = s.App
		}
		fmt.Println(s.Line())
	}

	printVolumeUsage(ctx, cfg, cluster, want)
	printCertificates(ctx, cluster, want)
	return nil
}

// printVolumeUsage shows how much each declared volume holds against the size
// it was declared with, for the groups in want (nil: every group).
//
// The size is what a volume is expected to hold, not a quota: a volume is a
// directory on the machine's disk, and nothing stops it growing past. This is
// where one that has is seen, before the disk it shares with everything else
// fills.
func printVolumeUsage(ctx context.Context, cfg Config, cluster *Cluster, want map[string]bool) {
	groups, err := loadGroups(cfg)
	if err != nil {
		return
	}
	sizes := map[string]manifest.Size{}
	names := map[string]string{}
	var scoped []*manifest.Manifest
	for _, m := range groups {
		if want != nil && !want[m.App] {
			continue
		}
		scoped = append(scoped, m)
		for _, s := range m.Services {
			if s.Volume != nil {
				path := deploy.VolumePath(DataDir, m.App, s.Name)
				sizes[path], names[path] = s.Volume.Size, m.App+"/"+s.Name
			}
		}
	}
	dirs, err := declaredVolumeDirs(cfg, scoped)
	if err != nil || len(dirs) == 0 {
		return
	}

	facts, err := inspectVolumes(ctx, cfg, cluster, dirs, true)
	if err != nil {
		fmt.Printf("\ncould not measure volumes: %v\n", err)
		return
	}

	fmt.Printf("\nvolumes\n")
	var t table
	for _, d := range dirs {
		f, size := facts[d.Path], sizes[d.Path]
		used, note := "?", ""
		switch {
		case !f.HasData:
			used = "empty"
		case f.Used >= 0:
			used = statuspage.BytesHuman(f.Used)
			if f.Used > int64(size) {
				note = "over its declared size"
			}
		}
		t.row(names[d.Path], d.Node, used+" of "+size.String(), note)
	}
	t.write(os.Stdout, "  ")
}

func readStatus(ctx context.Context, cluster *Cluster) ([]deploy.ServiceStatus, error) {
	st, err := readClusterState(ctx, cluster)
	if err != nil {
		return nil, err
	}
	return st.summarize(), nil
}

// clusterState is one reading of everything status is judged from.
type clusterState struct {
	jobs   map[string]deploy.JobState
	allocs []deploy.AllocState
	deps   []deploy.DeploymentState
}

func readClusterState(ctx context.Context, cluster *Cluster) (clusterState, error) {
	jobs, err := cluster.Jobs(ctx)
	if err != nil {
		return clusterState{}, err
	}
	allocs, deps, err := cluster.Runtime(ctx)
	if err != nil {
		return clusterState{}, err
	}
	return clusterState{jobs: jobs, allocs: allocs, deps: deps}, nil
}

func (st clusterState) summarize() []deploy.ServiceStatus {
	return deploy.Summarize(st.jobs, st.allocs, st.deps)
}

// waitForHealth blocks until every named job settles or the deadline passes,
// then returns an error if anything ended unhealthy, so a broken deploy fails
// the command, and therefore fails CI, instead of being reported as a success.
//
// Every service in scope is checked, not only the ones this apply changed. An
// apply that changes nothing while a service is crash-looping still has to say
// so: otherwise a CI job running apply on every commit stays green while
// something is down, which is the failure this whole function exists to
// prevent.
//
// What this apply submitted is judged by Nomad's rollout of the version it
// submitted (see deploy.Rollout), everything else by what is running.
//
// verbose is false for the no-op case, where a healthy cluster should print
// nothing at all and only problems are worth words.
func waitForHealth(ctx context.Context, cluster *Cluster, jobIDs []string, submitted map[string]bool, verbose bool) error {
	if len(jobIDs) == 0 {
		return nil
	}

	want := map[string]bool{}
	for _, id := range jobIDs {
		want[id] = true
	}

	timeout := HealthTimeout
	if len(submitted) > 0 {
		timeout = RolloutTimeout
	}
	if verbose {
		fmt.Printf("\nwaiting for health (up to %s)\n", timeout)
	}

	deadline := time.Now().Add(timeout)
	settled := map[string]deploy.ServiceStatus{}

	// The version each submitted job is at, as first read after submitting.
	// Apply holds the cluster's apply lock, so that version is this apply's.
	versions := map[string]uint64{}

	for {
		st, err := readClusterState(ctx, cluster)
		if err != nil {
			return err
		}

		var pending []deploy.ServiceStatus
		for _, s := range st.summarize() {
			if !want[s.JobID] || settled[s.JobID].JobID != "" {
				continue
			}
			if job := st.jobs[s.JobID]; submitted[s.JobID] && !job.Periodic {
				if _, ok := versions[s.JobID]; !ok {
					versions[s.JobID] = job.Version
				}
				s = deploy.Rollout(s, versions[s.JobID], st.allocs, st.deps)
			}
			if !s.Settled() {
				pending = append(pending, s)
				continue
			}
			settled[s.JobID] = s
			if verbose || problem(s) {
				report(s)
			}
		}

		if len(settled) == len(want) {
			break
		}
		if time.Now().After(deadline) {
			for _, s := range pending {
				if s.Health == deploy.HealthUnplaced {
					if why := cluster.PlacementFailure(ctx, s.JobID); why != "" {
						s.Message = why
					}
				}
				settled[s.JobID] = s
				if verbose || problem(s) {
					report(s)
				}
			}
			break
		}

		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(healthPoll):
		}
	}

	bad := unhealthy(want, settled)

	if verbose || len(bad) > 0 {
		fmt.Printf("\n%d of %d healthy\n", len(want)-len(bad), len(want))
	}
	if len(bad) > 0 {
		return fmt.Errorf("not healthy: %s", strings.Join(bad, ", "))
	}
	return nil
}

// unhealthy names every waited-on job that did not end healthy, including
// one that never appeared in the cluster at all. That case counts as bad:
// otherwise a job missing from the listing would let apply exit zero while
// reporting fewer healthy services than it waited for.
func unhealthy(want map[string]bool, settled map[string]deploy.ServiceStatus) []string {
	var bad []string
	for id := range want {
		s, ok := settled[id]
		switch {
		case !ok:
			bad = append(bad, fmt.Sprintf("%s (not found in the cluster)", id))
		case problem(s):
			bad = append(bad, fmt.Sprintf("%s/%s (%s)", s.App, s.Service, s.Health))
		}
	}
	sort.Strings(bad)
	return bad
}

// problem reports a state worth interrupting a quiet apply for. A stopped
// service and a periodic job between runs are both facts, not faults.
func problem(s deploy.ServiceStatus) bool {
	switch s.Health {
	case deploy.HealthOK, deploy.HealthStopped, deploy.HealthScheduled:
		return false
	}
	return true
}

func report(s deploy.ServiceStatus) {
	line := fmt.Sprintf("  %-26s %s", s.App+"/"+s.Service, s.Health)
	if s.Health == deploy.HealthOK && !s.Since.IsZero() {
		line += "  " + humanSince(s.Since)
	}
	fmt.Println(line)
	if s.Health != deploy.HealthOK && s.Message != "" {
		fmt.Printf("  %-26s %s\n", "", s.Message)
	}
}

func humanSince(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	if d < time.Second {
		d = time.Second
	}
	return d.String()
}

// clusterFor returns a connection to a machine orca can drive the cluster
// through.
//
// With several servers it probes them in order rather than always using the
// first: a three-server cluster survives losing one, and orca refusing to work
// because the machine listed first is down would throw that away.
func clusterFor(ctx context.Context, cfg Config) (*Cluster, error) {
	servers := cfg.Servers()
	switch len(servers) {
	case 0:
		return nil, fmt.Errorf("no server node in the cluster config")
	case 1:
		// One server is the only candidate, so probing it would only turn a
		// clear failure later into a vaguer one now.
		return NewCluster(Node{Host: servers[0].Host}), nil
	}

	var tried []string
	for _, s := range servers {
		c := NewCluster(Node{Host: s.Host})
		if c.Alive(ctx) {
			return c, nil
		}
		tried = append(tried, s.Name)
	}
	return nil, fmt.Errorf("no server is reachable (tried %s)", strings.Join(tried, ", "))
}
