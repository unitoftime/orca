package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/pkg/deploy"
	"github.com/unitoftime/orca/pkg/manifest"
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
	cfg := Config{Nodes: []NodeConfig{{Host: "root@a", Name: "box0", Role: RoleServer}}}
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
	cfg := Config{Nodes: []NodeConfig{{Host: "root@a", Name: "box0", Role: RoleServer}}}
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
		{Host: "root@a", Name: "box0", PrivateIP: "10.0.0.1", Role: RoleServer},
		{Host: "root@b", Name: "box1", PrivateIP: "10.0.0.2", Role: RoleClient},
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
		{Host: "root@a", Name: "box0", PrivateIP: "10.0.0.1", Role: RoleClient},
		{Host: "root@b", Name: "box1", PrivateIP: "10.0.0.2", Role: RoleServer},
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
		{Host: "root@a", Name: "box0", PrivateIP: "10.0.0.1", Role: RoleServer},
		{Host: "root@b", Name: "box1", PrivateIP: "10.0.0.2", Role: RoleClient},
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
	cfg := Config{Nodes: []NodeConfig{{Host: "root@a", Name: "box0", Role: RoleServer}}}
	m := parseManifest(t, "a", "{name: bot, image: i:1, node: nowhere}")

	if _, err := nodeFor(cfg, m.Services[0]); err == nil {
		t.Fatal("expected an error for a node not in the cluster config")
	}
}

func TestSelectGroups(t *testing.T) {
	groups := []*manifest.Manifest{
		parseManifest(t, "blog", "{name: api, image: i:1}"),
		parseManifest(t, "notifier", "{name: bot, image: i:1}"),
	}

	all, err := selectGroups(groups, nil)
	if err != nil || len(all) != 2 {
		t.Errorf("no names should select everything, got %d groups, err %v", len(all), err)
	}

	one, err := selectGroups(groups, []string{"blog"})
	if err != nil || len(one) != 1 || one[0].App != "blog" {
		t.Errorf("selecting blog gave %v, err %v", one, err)
	}

	if _, err := selectGroups(groups, []string{"nope"}); err == nil {
		t.Fatal("expected an error for an unknown group")
	} else if !strings.Contains(err.Error(), "known groups") {
		t.Errorf("error should list what is available, got: %v", err)
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
		"shop-app": {ID: "shop-app", App: "shop", Service: "app"},
		"old-bot":  {ID: "old-bot", App: "old", Service: "bot"},
	}

	got, err := scopeGroups(groups, []string{"shop", "old"}, current)
	if err != nil || len(got) != 1 || got[0].App != "shop" {
		t.Fatalf("want shop deployed, got %v, %v", got, err)
	}

	desired := []*nomad.Job{{ID: ptrTo("shop-app"), Meta: map[string]string{deploy.MetaApp: "shop", deploy.MetaService: "app"}}}
	plan := deploy.BuildPlan(desired, current, nil, map[string]bool{"shop": true, "old": true})
	for _, c := range plan.Stops() {
		if c.App == "shop" {
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

// Group names come from directories, so a collision needs two real
// directories whose names combine with service names to the same job id.
func TestJobIDCollisionIsCaught(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cluster.yaml":   "nodes:\n  - host: root@10.0.0.1\n",
		"blog-db/x.yaml": "{name: x, image: i:1}",
		"blog/db-x.yaml": "{name: db-x, image: i:1}",
	})

	cfg, err := LoadConfigFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadGroups(cfg); err == nil {
		t.Fatal("expected a job name collision to be reported")
	} else if !strings.Contains(err.Error(), "blog-db-x") {
		t.Errorf("error should name the colliding job, got: %v", err)
	}
}

func TestHostPortCollisionAcrossGroups(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cluster.yaml":   "nodes:\n  - host: root@10.0.0.1\n",
		"blog/api.yaml":  "{name: api, image: i:1, ports: {7777: tcp:7777}}",
		"other/srv.yaml": "{name: srv, image: i:1, ports: {9000: tcp:7777}}",
	})

	cfg, err := LoadConfigFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = loadGroups(cfg)
	if err == nil {
		t.Fatal("expected a host port collision across groups to be reported")
	}
	for _, want := range []string{"7777", "blog/api", "other/srv"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// Ingress holds 80 and 443 through host networking and sshd holds 22, none of
// which Nomad knows about, so a service claiming one would validate, place,
// and then fail on the box with "address already in use" unless loading the
// manifests refuses it.
func TestHostPortTakenByTheMachine(t *testing.T) {
	cases := []struct {
		name, cluster, ports, want string
	}{
		{"https with ingress", "ingress: {}\n", "{8080: tcp:443}", "ingress"},
		{"http with ingress", "ingress: {}\n", "{8080: tcp:80}", "ingress"},
		{"ssh", "ingress: false\n", "{2222: tcp:22}", "ssh"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"cluster.yaml":    "nodes:\n  - host: root@10.0.0.1\n" + c.cluster,
				"blog/proxy.yaml": "{name: proxy, image: i:1, ports: " + c.ports + "}",
			})
			cfg, err := LoadConfigFrom(root)
			if err != nil {
				t.Fatal(err)
			}
			_, err = loadGroups(cfg)
			if err == nil {
				t.Fatal("expected the machine's own port to be refused")
			}
			for _, want := range []string{c.want, "blog/proxy"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error should mention %q, got: %v", want, err)
				}
			}
		})
	}

	// Without ingress, 80 and 443 are the machine's to give, and udp 443 is
	// never ingress's: Traefik serves tcp only.
	root := writeTree(t, map[string]string{
		"cluster.yaml":    "nodes:\n  - host: root@10.0.0.1\ningress: false\n",
		"blog/proxy.yaml": "{name: proxy, image: i:1, ports: {8080: tcp:443, 7777: udp:443}}",
	})
	cfg, err := LoadConfigFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadGroups(cfg); err != nil {
		t.Errorf("443 should be free with ingress off, got: %v", err)
	}
}

// The directory is the index: a group added by creating a directory is
// discovered, and nesting deeper joins the path with dashes so "app", "group"
// and "stage" are all just how deep you nest.
// blog/prod/ and blog-prod/ both flatten to the group "blog-prod"; sharing a
// name would mean sharing jobs, secrets and a DNS domain.
func TestTwoDirectoriesCannotBeOneGroup(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cluster.yaml":       "nodes:\n  - host: root@10.0.0.1\n",
		"blog/prod/web.yaml": "{name: web, image: i:1}",
		"blog-prod/bot.yaml": "{name: bot, image: i:1}",
	})
	cfg, err := LoadConfigFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = loadGroups(cfg)
	if err == nil || !strings.Contains(err.Error(), `both the group "blog-prod"`) {
		t.Fatalf("want a collision naming both directories, got %v", err)
	}
}

func TestDiscoveryFromDirectories(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cluster.yaml":       "nodes:\n  - host: root@10.0.0.1\n",
		"blog/db.yaml":       "{name: db, template: postgres:17, volume: 5G}",
		"blog/api.yaml":      "{name: api, image: i:1}",
		"blog/prod/web.yaml": "{name: web, image: i:1}",
		"notifier/bot.yaml":  "{name: bot, image: i:1}",
		".hidden/skip.yaml":  "{name: nope, image: i:1}",
	})

	cfg, err := LoadConfigFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := loadGroups(cfg)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]int{}
	for _, g := range groups {
		got[g.App] = len(g.Services)
	}
	want := map[string]int{"blog": 2, "blog-prod": 1, "notifier": 1}
	if len(got) != len(want) {
		t.Fatalf("groups = %v, want %v", got, want)
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("group %q has %d services, want %d", name, got[name], n)
		}
	}
}

// A service file beside cluster.yaml is almost certainly misplaced, and
// ignoring it would deploy nothing while looking like it worked.
func TestStrayManifestAtRootIsAnError(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cluster.yaml": "nodes:\n  - host: root@10.0.0.1\n",
		"api.yaml":     "{name: api, image: i:1}",
		"blog/db.yaml": "{name: db, image: i:1}",
	})

	cfg, err := LoadConfigFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadGroups(cfg); err == nil {
		t.Fatal("expected an error for a manifest beside cluster.yaml")
	} else if !strings.Contains(err.Error(), "group directory") {
		t.Errorf("error should explain where it belongs, got: %v", err)
	}
}

// orca finds its root by walking up, so commands work from anywhere inside.
func TestFindRootFromSubdirectory(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cluster.yaml": "nodes:\n  - host: root@10.0.0.1\n",
		"blog/db.yaml": "{name: db, image: i:1}",
	})

	cfg, err := LoadConfigFrom(filepath.Join(root, "blog"))
	if err != nil {
		t.Fatalf("should find cluster.yaml by walking up: %v", err)
	}
	if cfg.Root != root {
		t.Errorf("root = %q, want %q", cfg.Root, root)
	}
}

// A volume whose machine moved in the config would start empty and look
// healthy. Data found anywhere but where the service is going stops the apply.
func TestMisplacedVolumesAreRefused(t *testing.T) {
	dirs := []VolumeDir{
		{Path: "/v/shop/db", Node: "box1"},
		{Path: "/v/shop/cache", Node: "box0"},
		{Path: "/v/blog/db", Node: "box1"},
	}
	holders := map[string][]string{
		"/v/shop/db":    {"box0"}, // moved away from its data
		"/v/shop/cache": {"box0"}, // where it always was
		// blog/db is new: no data anywhere yet
	}
	err := misplacedVolumes(dirs, holders)
	if err == nil || !strings.Contains(err.Error(), "/v/shop/db holds data on box0") {
		t.Fatalf("want shop/db refused, got %v", err)
	}
	if strings.Contains(err.Error(), "cache") || strings.Contains(err.Error(), "blog") {
		t.Errorf("only the moved volume should be refused: %v", err)
	}
}

// One hostname, one service: ingress would otherwise pick between them by
// its own rules, and a service could sit in front of a dashboard's password.
func TestHostnameCollisionsAreCaught(t *testing.T) {
	a := parseManifest(t, "shop", "{name: web, image: i:1, ports: {8080: Shop.example.com}}")
	b := parseManifest(t, "blog", "{name: web, image: i:1, ports: {8080: shop.example.com}}")
	if err := checkHostnames([]*manifest.Manifest{a, b}, nil); err == nil {
		t.Error("two groups claiming one hostname should be refused")
	}

	c := parseManifest(t, "blog", "{name: web, image: i:1, ports: {8080: status.example.com}}")
	if err := checkHostnames([]*manifest.Manifest{c}, map[string]string{"status.example.com": "orca's dashboards"}); err == nil {
		t.Error("a dashboard's hostname should be refused")
	}
}
