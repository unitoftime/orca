package deploy

import (
	"encoding/json"
	"strings"
	"testing"

	nomad "github.com/hashicorp/nomad/api"
)

// Nomad compares what is submitted with what it holds, so a job has to be a
// pure function of its inputs: building the same manifest twice must produce
// byte-identical specs, or every apply would look like a change and redeploy
// everything.
func TestBuildIsDeterministic(t *testing.T) {
	// Env is a map, and map iteration order is random. If that order leaked
	// into the spec, applies would flap between two versions forever.
	body := "name: web\nimage: i:1\nports: {8080: web.example.com}\nenv: {A: 1, B: 2, C: 3, D: 4, E: 5, F: 6}"

	spec := func() string {
		b, err := json.Marshal(buildOne(t, body, "web", defaultOpts()))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	first := spec()
	for i := 0; i < 20; i++ {
		if got := spec(); got != first {
			t.Fatalf("building the same manifest twice gave different specs:\n%s\n%s", first, got)
		}
	}
}

// Every task image is pinned, a template's init task and the platform's
// images included.
func TestPinImagesPinsEveryTask(t *testing.T) {
	job := buildOneIn(t, "files", "{name: store, template: garage:2.3.0, volume: 1G}", "store", defaultOpts())

	err := PinImages([]*nomad.Job{job}, func(ref string) (string, error) {
		return ref + "@sha256:abc", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range job.TaskGroups[0].Tasks {
		if img := task.Config["image"].(string); !strings.Contains(img, "@sha256:") {
			t.Errorf("task %s image %q is not pinned", task.Name, img)
		}
	}
}
