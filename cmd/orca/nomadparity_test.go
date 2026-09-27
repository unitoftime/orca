package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/pkg/deploy"
)

// The CLI reads Nomad over SSH through jq; the status page reads the same API
// with Nomad's Go types. Both must arrive at the same state, or `orca status`
// and the status page disagree about the same service. These run the CLI's
// actual jq programs and the page's Go projections over the same captured
// Nomad responses — real ones from a test machine, plus a crash loop and a
// placement failure written by hand — and require identical results.

func runJQ(t *testing.T, program string, input []byte, slurp bool) string {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	args := []string{"-c"}
	if slurp {
		args = append(args, "-s")
	}
	cmd := exec.Command("jq", append(args, program)...)
	cmd.Stdin = bytes.NewReader(input)
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("jq: %v: %s", err, errOut.String())
	}
	return string(out)
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/nomad/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sameJSON(t *testing.T, what string, cli, page any) {
	t.Helper()
	a, _ := json.MarshalIndent(cli, "", "  ")
	b, _ := json.MarshalIndent(page, "", "  ")
	if !bytes.Equal(a, b) {
		t.Errorf("%s: the CLI and the status page disagree\ncli:  %s\npage: %s", what, a, b)
	}
}

func TestJobsParity(t *testing.T) {
	raw := fixture(t, "jobs.json")

	// The script streams one job per curl into jq -s.
	var jobs []json.RawMessage
	if err := json.Unmarshal(raw, &jobs); err != nil {
		t.Fatal(err)
	}
	cliJobs, err := parseJobs(runJQ(t, jobsJQ, bytes.Join(jobsToLines(jobs), []byte("\n")), true))
	if err != nil {
		t.Fatal(err)
	}

	var typed []*nomad.Job
	if err := json.Unmarshal(raw, &typed); err != nil {
		t.Fatal(err)
	}
	pageJobs := map[string]deploy.JobState{}
	for _, j := range typed {
		if s, ok := deploy.JobStateFromNomad(j); ok {
			pageJobs[s.ID] = s
		}
	}

	if len(cliJobs) != len(typed) {
		t.Fatalf("fixture jobs should all be orca's: cli kept %d of %d", len(cliJobs), len(typed))
	}
	sameJSON(t, "jobs", cliJobs, pageJobs)
	if !pageJobs["shop-db-backup"].Periodic || pageJobs["shop-db"].Periodic {
		t.Errorf("periodic flags wrong: %+v", pageJobs)
	}
}

func jobsToLines(jobs []json.RawMessage) [][]byte {
	out := make([][]byte, len(jobs))
	for i, j := range jobs {
		out[i] = j
	}
	return out
}

func TestRuntimeParity(t *testing.T) {
	allocsRaw := fixture(t, "allocations.json")
	depsRaw := fixture(t, "deployments.json")

	cliOut := `{"allocs":` + runJQ(t, allocsJQ, allocsRaw, false) +
		`,"deployments":` + runJQ(t, deploymentsJQ, depsRaw, false) + `}`
	cliAllocs, cliDeps, err := parseRuntime(cliOut)
	if err != nil {
		t.Fatal(err)
	}

	var stubs []*nomad.AllocationListStub
	if err := json.Unmarshal(allocsRaw, &stubs); err != nil {
		t.Fatal(err)
	}
	var pageAllocs []deploy.AllocState
	for _, a := range stubs {
		pageAllocs = append(pageAllocs, deploy.AllocStateFromNomad(a))
	}

	var deps []*nomad.Deployment
	if err := json.Unmarshal(depsRaw, &deps); err != nil {
		t.Fatal(err)
	}
	var pageDeps []deploy.DeploymentState
	for _, d := range deps {
		pageDeps = append(pageDeps, deploy.DeploymentStateFromNomad(d))
	}

	// jq walks task states in key order, which is the order Nomad wrote them
	// in; Go's map has none, so the page sorts. Compare in the same order.
	for _, as := range [][]deploy.AllocState{cliAllocs, pageAllocs} {
		for i := range as {
			sort.Slice(as[i].Tasks, func(a, b int) bool { return as[i].Tasks[a].Name < as[i].Tasks[b].Name })
		}
	}
	sameJSON(t, "allocations", cliAllocs, pageAllocs)
	sameJSON(t, "deployments", cliDeps, pageDeps)

	// And the fixture exercises what it is here for.
	var crash deploy.AllocState
	for _, a := range pageAllocs {
		if strings.Contains(a.ID, "synthetic-crashloop") {
			crash = a
		}
	}
	if len(crash.Tasks) != 2 || crash.Tasks[0].Fail != "Exit Code: 3" || crash.Tasks[0].Last != "Task restarting in 16s" {
		t.Errorf("crash loop not projected as expected: %+v", crash.Tasks)
	}

	// The judgement is shared, so equal inputs are equal answers — checked
	// end to end anyway, since that is the property that matters.
	var typedJobs []*nomad.Job
	json.Unmarshal(fixture(t, "jobs.json"), &typedJobs)
	jobs := map[string]deploy.JobState{}
	for _, j := range typedJobs {
		if s, ok := deploy.JobStateFromNomad(j); ok {
			jobs[s.ID] = s
		}
	}
	sameJSON(t, "summary", deploy.Summarize(jobs, cliAllocs, cliDeps), deploy.Summarize(jobs, pageAllocs, pageDeps))
}

func TestPlacementFailureParity(t *testing.T) {
	raw := fixture(t, "evaluations.json")
	cli := strings.TrimSpace(runJQ(t, placementJQ, raw, false))
	// -c keeps jq's string quoted; the script uses -r.
	var cliText string
	if err := json.Unmarshal([]byte(cli), &cliText); err != nil {
		t.Fatalf("jq output %q: %v", cli, err)
	}

	var evals []*nomad.Evaluation
	if err := json.Unmarshal(raw, &evals); err != nil {
		t.Fatal(err)
	}
	page := deploy.PlacementFailureFromNomad(evals)

	if cliText != page {
		t.Errorf("placement failure differs\ncli:  %q\npage: %q", cliText, page)
	}
	if !strings.Contains(page, "box1 (2 nodes)") || !strings.Contains(page, "no capacity: memory") {
		t.Errorf("fixture not exercised: %q", page)
	}
}
