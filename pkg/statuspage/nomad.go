package statuspage

import (
	"context"
	"fmt"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/pkg/deploy"
)

// fetchNomad reads the same state `orca status` reads over SSH, through the
// same projections, so the two judge a service identically.
func fetchNomad(ctx context.Context, c *nomad.Client) (*nomadView, error) {
	q := (&nomad.QueryOptions{}).WithContext(ctx)

	stubs, _, err := c.Jobs().List(q)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	nv := &nomadView{Jobs: map[string]deploy.JobState{}, Placement: map[string]string{}}
	for _, stub := range stubs {
		// A periodic job's past runs are listed as jobs of their own.
		if stub.ParentID != "" {
			continue
		}
		job, _, err := c.Jobs().Info(stub.ID, q)
		if err != nil {
			return nil, fmt.Errorf("read job %s: %w", stub.ID, err)
		}
		if s, ok := deploy.JobStateFromNomad(job); ok {
			nv.Jobs[s.ID] = s
		}
	}

	// With what each allocation and machine has to hand out, which is how
	// Nomad decides whether anything more fits.
	withResources := (&nomad.QueryOptions{Params: map[string]string{"resources": "true"}}).WithContext(ctx)

	allocs, _, err := c.Allocations().List(withResources)
	if err != nil {
		return nil, fmt.Errorf("list allocations: %w", err)
	}
	for _, a := range allocs {
		nv.Allocs = append(nv.Allocs, deploy.AllocStateFromNomad(a))
	}

	deps, _, err := c.Deployments().List(q)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	for _, d := range deps {
		nv.Deployments = append(nv.Deployments, deploy.DeploymentStateFromNomad(d))
	}

	nodes, _, err := c.Nodes().List(withResources)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	for _, n := range nodes {
		info := nodeInfo{
			Name:     n.Name,
			Status:   n.Status,
			Eligible: n.SchedulingEligibility == nomad.NodeSchedulingEligible,
			Draining: n.Drain,
		}
		// What is left for allocations once the machine's own reservation
		// is taken out.
		if r := n.NodeResources; r != nil {
			info.CPUMHz, info.MemoryMB = r.Cpu.CpuShares, r.Memory.MemoryMB
			if rr := n.ReservedResources; rr != nil {
				info.CPUMHz -= int64(rr.Cpu.CpuShares)
				info.MemoryMB -= int64(rr.Memory.MemoryMB)
			}
		}
		nv.Nodes = append(nv.Nodes, info)
	}

	// Why an unplaced service has nowhere to run is in its evaluations, and
	// costs a call per job, so it is read only for the ones that need it.
	for _, st := range deploy.Summarize(nv.Jobs, nv.Allocs, nv.Deployments) {
		if st.Health != deploy.HealthUnplaced {
			continue
		}
		evals, _, err := c.Jobs().Evaluations(st.JobID, q)
		if err == nil {
			nv.Placement[st.JobID] = deploy.PlacementFailureFromNomad(evals)
		}
	}
	return nv, nil
}
