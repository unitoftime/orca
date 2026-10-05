package statuspage

import (
	"context"
	"fmt"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/internal/deploy"
)

// fetchNomad reads the same state `orca status` reads, through the same
// readers, so the two judge a service identically.
func fetchNomad(ctx context.Context, c *nomad.Client) (*nomadView, error) {
	jobs, err := deploy.ReadJobs(ctx, c)
	if err != nil {
		return nil, err
	}
	nv := &nomadView{Jobs: jobs, Placement: map[string]string{}}

	// With what each allocation and machine has to hand out, which is how
	// Nomad decides whether anything more fits.
	if nv.Allocs, nv.Deployments, err = deploy.ReadRuntime(ctx, c, true); err != nil {
		return nil, err
	}

	withResources := (&nomad.QueryOptions{Params: map[string]string{"resources": "true"}}).WithContext(ctx)
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
		nv.Placement[st.JobID] = deploy.ReadPlacement(ctx, c, st.JobID)
	}
	return nv, nil
}
