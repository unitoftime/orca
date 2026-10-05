package main

import (
	"os"
	"path/filepath"
	"testing"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/internal/deploy"
	"github.com/unitoftime/orca/internal/manifest"
)

func parseManifest(t *testing.T, group, body string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.ParseGroup(group, []byte(body), group+"/svc.yaml")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	return m
}

// writeTree lays out a cluster directory: cluster.yaml at the root and one
// directory per group, which is how orca finds what to deploy.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestNodeForStatelessServiceIsUnpinned(t *testing.T) {
	cfg := Config{Nodes: []NodeConfig{{Host: "root@a", Name: "box0", Role: roleServer}}}
	m := parseManifest(t, "a", "{name: bot, image: i:1}")

	got, err := nodeFor(cfg, m.Services[0])
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("node = %q, want Nomad to choose", got)
	}
}

func TestNodeForVolumeOnOneMachineIsAutomatic(t *testing.T) {
	cfg := Config{Nodes: []NodeConfig{{Host: "root@a", Name: "box0", Role: roleServer}}}
	m := parseManifest(t, "a", "{name: db, template: postgres:17, volume: 5G}")

	got, err := nodeFor(cfg, m.Services[0])
	if err != nil {
		t.Fatal(err)
	}
	if got != "box0" {
		t.Errorf("node = %q, want the only machine", got)
	}
}

// Adding a machine must not move, or refuse, a database: its data is on the
// machine it has always been on, which is the first server.
func TestNodeForVolumeStaysOnTheFirstServer(t *testing.T) {
	cfg := Config{Nodes: []NodeConfig{
		{Host: "root@a", Name: "box0", PrivateIP: "10.0.0.1", Role: roleServer},
		{Host: "root@b", Name: "box1", PrivateIP: "10.0.0.2", Role: roleClient},
	}}
	m := parseManifest(t, "a", "{name: db, template: postgres:17, volume: 5G}")

	got, err := nodeFor(cfg, m.Services[0])
	if err != nil {
		t.Fatal(err)
	}
	if got != "box0" {
		t.Errorf("node = %q, want the first server", got)
	}
}

// The first server, not the first machine listed: a client listed first
// holds nothing of orca's.
func TestNodeForVolumeSkipsALeadingClient(t *testing.T) {
	cfg := Config{Nodes: []NodeConfig{
		{Host: "root@a", Name: "box0", PrivateIP: "10.0.0.1", Role: roleClient},
		{Host: "root@b", Name: "box1", PrivateIP: "10.0.0.2", Role: roleServer},
	}}
	m := parseManifest(t, "a", "{name: db, template: postgres:17, volume: 5G}")

	got, err := nodeFor(cfg, m.Services[0])
	if err != nil {
		t.Fatal(err)
	}
	if got != "box1" {
		t.Errorf("node = %q, want the first server", got)
	}
}

func TestNodeForVolumeHonoursNode(t *testing.T) {
	cfg := Config{Nodes: []NodeConfig{
		{Host: "root@a", Name: "box0", PrivateIP: "10.0.0.1", Role: roleServer},
		{Host: "root@b", Name: "box1", PrivateIP: "10.0.0.2", Role: roleClient},
	}}
	m := parseManifest(t, "a", "{name: db, template: postgres:17, volume: 5G, node: box1}")

	got, err := nodeFor(cfg, m.Services[0])
	if err != nil {
		t.Fatal(err)
	}
	if got != "box1" {
		t.Errorf("node = %q, want the node the manifest names", got)
	}
}

func TestNodeForRejectsAnUnknownNode(t *testing.T) {
	cfg := Config{Nodes: []NodeConfig{{Host: "root@a", Name: "box0", Role: roleServer}}}
	m := parseManifest(t, "a", "{name: bot, image: i:1, node: nowhere}")

	if _, err := nodeFor(cfg, m.Services[0]); err == nil {
		t.Fatal("expected an error for a node not in the cluster config")
	}
}

// Naming a deleted group alongside a live one narrows the apply to both: the
// live one is deployed, the deleted one's jobs are in scope to be stopped.
// The live group's manifests must not be dropped too, or the plan stops all
// of it.
func TestScopeGroupsKeepsLiveGroupsBesideDeletedOnes(t *testing.T) {
	groups := []*manifest.Manifest{
		parseManifest(t, "shop", "{name: app, image: i:1}"),
		parseManifest(t, "blog", "{name: api, image: i:1}"),
	}
	current := map[string]deploy.JobState{
		"shop-app": {ID: "shop-app", Group: "shop", Service: "app"},
		"old-bot":  {ID: "old-bot", Group: "old", Service: "bot"},
	}

	got, err := scopeGroups(groups, []string{"shop", "old"}, current)
	if err != nil || len(got) != 1 || got[0].Group != "shop" {
		t.Fatalf("want shop deployed, got %v, %v", got, err)
	}

	desired := []*nomad.Job{{ID: ptrTo("shop-app"), Meta: map[string]string{deploy.MetaGroup: "shop", deploy.MetaService: "app"}}}
	plan := deploy.BuildPlan(desired, current, nil, map[string]bool{"shop": true, "old": true})
	for _, c := range plan.Stops() {
		if c.Group == "shop" {
			t.Errorf("shop must not be stopped: %+v", c)
		}
	}

	if _, err := scopeGroups(groups, []string{"shop", "typo"}, current); err == nil {
		t.Error("a name that is neither a directory nor running is a typo")
	}
	if got, err := scopeGroups(groups, []string{"old"}, current); err != nil || len(got) != 0 {
		t.Errorf("a deleted group alone deploys nothing, got %v, %v", got, err)
	}
}

func ptrTo[T any](v T) *T { return &v }
