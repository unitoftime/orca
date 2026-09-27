package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/unitoftime/orca/pkg/deploy"
)

// HealthTimeout bounds how long apply waits for a deploy to settle.
//
// Bounded, not infinite: a cold image pull is slow and perfectly healthy, while
// a crash loop never resolves. Waiting forever would leave apply hanging on
// the first crash loop it meets.
const HealthTimeout = 2 * time.Minute

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

	if len(args) > 0 {
		want := map[string]bool{}
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
	return nil
}

func readStatus(ctx context.Context, cluster *Cluster) ([]deploy.ServiceStatus, error) {
	jobs, err := cluster.Jobs(ctx)
	if err != nil {
		return nil, err
	}
	allocs, deps, err := cluster.Runtime(ctx)
	if err != nil {
		return nil, err
	}
	return deploy.Summarize(jobs, allocs, deps), nil
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
// verbose is false for that no-op case, where a healthy cluster should print
// nothing at all and only problems are worth words.
func waitForHealth(ctx context.Context, cluster *Cluster, jobIDs []string, timeout time.Duration, verbose bool) error {
	if len(jobIDs) == 0 {
		return nil
	}

	want := map[string]bool{}
	for _, id := range jobIDs {
		want[id] = true
	}

	if verbose {
		fmt.Printf("\nwaiting for health (up to %s)\n", timeout)
	}

	deadline := time.Now().Add(timeout)
	settled := map[string]deploy.ServiceStatus{}

	for {
		statuses, err := readStatus(ctx, cluster)
		if err != nil {
			return err
		}

		for _, s := range statuses {
			if !want[s.JobID] || settled[s.JobID].JobID != "" {
				continue
			}
			if !s.Settled() {
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
			for _, s := range statuses {
				if want[s.JobID] && settled[s.JobID].JobID == "" {
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
			}
			break
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
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
