package deploy

import (
	"strings"
	"testing"

	nomad "github.com/hashicorp/nomad/api"
)

// The hash must be a pure function of the spec: building the same manifest
// twice has to produce the same hash, or every apply would look like a change
// and redeploy everything.
func TestHashIsStable(t *testing.T) {
	body := "{name: web, image: i:1, ports: {8080: web.example.com}, env: {A: b, C: d}}"
	a := buildOne(t, body, "web", defaultOpts())
	b := buildOne(t, body, "web", defaultOpts())

	if a.Meta[MetaHash] != b.Meta[MetaHash] {
		t.Errorf("same input produced different hashes:\n%s\n%s", a.Meta[MetaHash], b.Meta[MetaHash])
	}
}

// Stamping the hash into the job must not change what the hash is of.
func TestHashExcludesItself(t *testing.T) {
	job := buildOne(t, "{name: web, image: i:1}", "web", defaultOpts())

	stamped := job.Meta[MetaHash]
	if got := Hash(job); got != stamped {
		t.Errorf("rehashing a stamped job gave %s, want %s", got, stamped)
	}
	if job.Meta[MetaHash] != stamped {
		t.Error("hashing must not leave the job mutated")
	}
}

func TestHashChangesWithTheSpec(t *testing.T) {
	base := "{name: web, image: i:1, cpu: 1, memory: 512M}"
	baseHash := buildOne(t, base, "web", defaultOpts()).Meta[MetaHash]

	tests := map[string]string{
		"memory":   "{name: web, image: i:1, cpu: 1, memory: 1G}",
		"cpu":      "{name: web, image: i:1, cpu: 2, memory: 512M}",
		"replicas": "{name: web, image: i:1, cpu: 1, memory: 512M, replicas: 2}",
		"env":      "{name: web, image: i:1, cpu: 1, memory: 512M, env: {X: y}}",
		"ports":    "{name: web, image: i:1, cpu: 1, memory: 512M, ports: {8080: web.example.com}}",
	}

	for what, body := range tests {
		t.Run(what, func(t *testing.T) {
			if got := buildOne(t, body, "web", defaultOpts()).Meta[MetaHash]; got == baseHash {
				t.Errorf("changing %s did not change the hash", what)
			}
		})
	}
}

// The image is the thing that most often changes and the one a mutable tag
// hides, so it must be covered.
func TestHashChangesWithTheImage(t *testing.T) {
	m := parse(t, "{name: web, image: i:1}")
	s, _ := m.Service("web")

	one, _ := Build(m, s, "i@sha256:aaa", defaultOpts())
	two, _ := Build(m, s, "i@sha256:bbb", defaultOpts())

	if one.Meta[MetaHash] == two.Meta[MetaHash] {
		t.Error("a different image digest must change the hash")
	}
}

// Env is a map, and map iteration order is random. If that order leaked into
// the hash, applies would flap between two values forever.
func TestHashDoesNotDependOnMapOrder(t *testing.T) {
	body := "name: web\nimage: i:1\nenv: {A: 1, B: 2, C: 3, D: 4, E: 5, F: 6}"

	first := buildOne(t, body, "web", defaultOpts()).Meta[MetaHash]
	for i := 0; i < 20; i++ {
		if got := buildOne(t, body, "web", defaultOpts()).Meta[MetaHash]; got != first {
			t.Fatalf("hash is not order-stable: %s != %s", got, first)
		}
	}
}

// Every task image is pinned — a template's init task and the platform's
// images went out as tags — and a job whose spec changed is re-hashed, or the
// plan would compare against a stale hash.
func TestPinImagesPinsEveryTask(t *testing.T) {
	job := buildOneIn(t, "files", "{name: store, template: garage:2.3.0, volume: 1G}", "store", defaultOpts())
	before := job.Meta[MetaHash]

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
	if job.Meta[MetaHash] == before || job.Meta[MetaHash] != Hash(job) {
		t.Error("a job whose images changed must carry the hash of what it now is")
	}
}
