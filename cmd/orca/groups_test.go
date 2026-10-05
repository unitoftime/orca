package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/unitoftime/orca/internal/manifest"
)

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
	if err != nil || len(one) != 1 || one[0].Group != "blog" {
		t.Errorf("selecting blog gave %v, err %v", one, err)
	}

	if _, err := selectGroups(groups, []string{"nope"}); err == nil {
		t.Fatal("expected an error for an unknown group")
	} else if !strings.Contains(err.Error(), "known groups") {
		t.Errorf("error should list what is available, got: %v", err)
	}
}

// Group names come from directories, so a collision needs two real
// directories whose names combine with service names to the same job id.
func TestJobIDCollisionIsCaught(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cluster.yaml":   "nodes:\n  - host: root@10.0.0.1\n",
		"blog-db/x.yaml": "{name: x, image: i:1}",
		"blog/db-x.yaml": "{name: db-x, image: i:1}",
	})

	cfg, err := loadConfigFrom(root)
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

	cfg, err := loadConfigFrom(root)
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
			cfg, err := loadConfigFrom(root)
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
	cfg, err := loadConfigFrom(root)
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
	cfg, err := loadConfigFrom(root)
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

	cfg, err := loadConfigFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := loadGroups(cfg)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]int{}
	for _, g := range groups {
		got[g.Group] = len(g.Services)
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

	cfg, err := loadConfigFrom(root)
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

	cfg, err := loadConfigFrom(filepath.Join(root, "blog"))
	if err != nil {
		t.Fatalf("should find cluster.yaml by walking up: %v", err)
	}
	if cfg.Root != root {
		t.Errorf("root = %q, want %q", cfg.Root, root)
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
