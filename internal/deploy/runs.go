package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	nomad "github.com/hashicorp/nomad/api"
)

// A scheduled job's last run is remembered after Nomad has forgotten it.
//
// Nomad keeps a finished run for a few hours and then collects it: the job,
// its evaluation and its allocation. Read from Nomad alone, a backup that ran
// last night has "not run yet" by the afternoon, and one that failed says the
// same. So the last finished run of each scheduled job is kept in the
// variable store, as the allocation a run is judged by, and read back beside
// the live ones: to everything that reads allocations it is one more.
//
// The status job writes them, since it is what is always running with a view
// of Nomad; see RememberRuns.

// RunPrefix is where the remembered runs live in Nomad's variable store: one
// variable per scheduled job. A sibling of SecretPrefix, like the others. It
// is no secret, so it is in no export.
const RunPrefix = "orca-run"

// RunItemKey holds the run, an AllocState as JSON.
const RunItemKey = "run"

// RunPath is where one scheduled job's last run is remembered.
func RunPath(jobID string) string {
	return RunPrefix + "/" + jobID
}

// finishedRuns picks the newest run that has ended for each scheduled job,
// keyed by that job's ID. A run still going is not one to remember: Nomad
// has it, and its outcome is not known.
func finishedRuns(allocs []AllocState) map[string]AllocState {
	out := map[string]AllocState{}
	for _, a := range allocs {
		parent, _, ok := strings.Cut(a.JobID, "/periodic-")
		if !ok || (a.ClientStatus != "complete" && a.ClientStatus != "failed") {
			continue
		}
		if prev, seen := out[parent]; !seen || a.CreateTime > prev.CreateTime {
			out[parent] = a
		}
	}
	return out
}

// runsToRemember is the runs among live that are newer than the one
// remembered for their job, ordered by job.
func runsToRemember(live, remembered []AllocState) []AllocState {
	have := finishedRuns(remembered)
	var out []AllocState
	for parent, run := range finishedRuns(live) {
		if prev, seen := have[parent]; seen && (prev.ID == run.ID || prev.CreateTime > run.CreateTime) {
			continue
		}
		out = append(out, run)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobID < out[j].JobID })
	return out
}

// withRuns adds the remembered runs Nomad no longer has to the live
// allocations.
func withRuns(live, remembered []AllocState) []AllocState {
	held := make(map[string]bool, len(live))
	for _, a := range live {
		held[a.ID] = true
	}
	for _, run := range remembered {
		if !held[run.ID] {
			live = append(live, run)
		}
	}
	return live
}

// ReadRuns returns the last finished run remembered for each scheduled job,
// ordered by job.
func ReadRuns(ctx context.Context, c *nomad.Client) ([]AllocState, error) {
	vars, err := ReadVariables(ctx, c, []string{RunPrefix + "/"})
	if err != nil {
		return nil, err
	}
	runs := make([]AllocState, 0, len(vars))
	for _, items := range vars {
		var run AllocState
		// One that does not read is one another build of orca wrote. It is
		// left out, and replaced by the job's next run.
		if json.Unmarshal([]byte(items[RunItemKey]), &run) != nil || run.JobID == "" {
			continue
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].JobID < runs[j].JobID })
	return runs, nil
}

// RememberRuns records the newest finished run of each scheduled job that
// Nomad still has, where it is not the one already remembered.
//
// Called on a loop by the status job. Nomad keeps a finished run for hours,
// so a run is missed only if nothing called this for all of that time.
func RememberRuns(ctx context.Context, c *nomad.Client) error {
	live, err := readAllocs(ctx, c, (&nomad.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return err
	}
	remembered, err := ReadRuns(ctx, c)
	if err != nil {
		return err
	}

	w := (&nomad.WriteOptions{}).WithContext(ctx)
	for _, run := range runsToRemember(live, remembered) {
		parent, _, _ := strings.Cut(run.JobID, "/periodic-")
		raw, err := json.Marshal(run)
		if err != nil {
			return fmt.Errorf("remember the run of %s: %w", parent, err)
		}
		v := &nomad.Variable{Path: RunPath(parent), Items: map[string]string{RunItemKey: string(raw)}}
		if _, _, err := c.Variables().Update(v, w); err != nil {
			return fmt.Errorf("remember the run of %s: %w", parent, err)
		}
	}
	return nil
}
