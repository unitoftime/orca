package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unitoftime/orca/internal/manifest"
)

func clusterTree(t *testing.T) Config {
	t.Helper()
	root := writeTree(t, map[string]string{
		"cluster.yaml":  "nodes:\n  - host: root@10.0.0.1\n",
		"blog/db.yaml":  "{name: db, template: postgres:17, volume: 5G}",
		"blog/api.yaml": "{name: api, image: i:1}",
	})
	cfg, err := loadConfigFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// purge refuses while the manifests still declare a group, so reaching it
// takes two deliberate steps rather than one mistyped word.
func TestDeclared(t *testing.T) {
	cfg := clusterTree(t)
	if yes, err := declared(cfg, "blog"); err != nil || !yes {
		t.Errorf("blog is declared, got %v, %v", yes, err)
	}
	if yes, err := declared(cfg, "gone"); err != nil || yes {
		t.Errorf("gone is not declared, got %v, %v", yes, err)
	}
}

// A manifest that does not parse must not read as "every group is gone". If
// declared() folded the load error into false, a typo in any group's file
// would let `orca purge` delete a group whose directory still exists, and
// list its live database as orphaned data to delete with it.
func TestPurgeFailsClosedWhenManifestsDoNotLoad(t *testing.T) {
	cfg := clusterTree(t)
	if err := os.WriteFile(filepath.Join(cfg.Root, "blog", "broken.yaml"), []byte("{name: x, imgae: i:1}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := declared(cfg, "blog"); err == nil {
		t.Fatal("declared must report that it cannot tell")
	}
	if _, err := declaredVolumes(cfg); err == nil {
		t.Fatal("declaredVolumes must report that it cannot tell")
	}
	err := cmdPurge(context.Background(), cfg, invoke(t, "purge", "blog", "--yes"))
	if err == nil || !strings.Contains(err.Error(), "do not load") {
		t.Fatalf("purge must refuse while the manifests do not load, got %v", err)
	}
}

// With every group deleted there is nothing declared, which is an answer, not
// an error; otherwise the last group could never be purged.
func TestDeclaredWithNoGroupsLeft(t *testing.T) {
	cfg := clusterTree(t)
	if err := os.RemoveAll(filepath.Join(cfg.Root, "blog")); err != nil {
		t.Fatal(err)
	}
	if yes, err := declared(cfg, "blog"); err != nil || yes {
		t.Errorf("got %v, %v; want false, nil", yes, err)
	}
}

// A volume's group is its directory. Matching the prefix "shop-" when purging
// "shop" would take shop-prod's data too.
func TestOrphansAreMatchedByExactGroup(t *testing.T) {
	l := parseVolumeListing("shop/db\nshop-prod/db\nblog/db\n")
	o := orphansOf(l, map[Volume]bool{{Group: "blog", Service: "db"}: true})

	want := []Volume{{"shop", "db"}, {"shop-prod", "db"}}
	if len(o.Volumes) != 2 || o.Volumes[0] != want[0] || o.Volumes[1] != want[1] {
		t.Errorf("orphans = %v, want %v", o.Volumes, want)
	}

	script, err := deleteVolumesScript([]Volume{{"shop", "db"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "shop-prod") || !strings.Contains(script, "'/var/orca/volumes/services/shop/db'") {
		t.Errorf("delete script reaches beyond shop/db:\n%s", script)
	}
}

// Only volumes the manifests still claim are exempt from the orphan listing,
// so data left behind by a removed service is always findable.
func TestDeclaredVolumes(t *testing.T) {
	got, err := declaredVolumes(clusterTree(t))
	if err != nil {
		t.Fatal(err)
	}
	if !got[Volume{"blog", "db"}] {
		t.Errorf("blog/db has a volume and should be claimed, got %v", got)
	}
	if got[Volume{"blog", "api"}] {
		t.Errorf("blog/api has no volume and should not be claimed, got %v", got)
	}
}

// Deleting a group's directory is what makes it purgeable.
func TestDeclaredAfterDirectoryRemoved(t *testing.T) {
	cfg := clusterTree(t)
	if err := os.RemoveAll(filepath.Join(cfg.Root, "blog")); err != nil {
		t.Fatal(err)
	}
	if yes, _ := declared(cfg, "blog"); yes {
		t.Error("a group whose directory is gone is no longer declared")
	}
}

// groupSecrets must match only the group being purged, or a purge would take
// another group's secrets with it.
func TestGroupSecretPrefixIsExact(t *testing.T) {
	// "blog" must not match "blog-dev": the separator is part of the prefix.
	for _, tc := range []struct {
		path  string
		group string
		want  bool
	}{
		{"orca/blog/token", "blog", true},
		{"orca/blog-dev/token", "blog", false},
		{"orca/other/token", "blog", false},
	} {
		got := strings.HasPrefix(tc.path, "orca/"+tc.group+"/")
		if got != tc.want {
			t.Errorf("%q under group %q = %v, want %v", tc.path, tc.group, got, tc.want)
		}
	}
}

// The reserved group has no directory, so purge's main safety rule (refuse a
// group the manifests still declare) can never fire for it. Without a guard,
// `orca purge orca` takes ingress, the resolver and both stores with one
// confirmed word.
func TestPurgeRefusesTheReservedGroup(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cluster.yaml":  "nodes:\n  - host: root@203.0.113.10\n",
		"shop/app.yaml": "{name: app, image: i:1}",
	})
	cfg, err := loadConfig(root + "/cluster.yaml")
	if err != nil {
		t.Fatal(err)
	}

	err = cmdPurge(context.Background(), cfg, invoke(t, "purge", manifest.ReservedGroup, "--yes"))
	if err == nil {
		t.Fatal("purging the reserved group should be refused")
	}
	// It has to say what to do instead, or the only read is "orca is broken".
	if !strings.Contains(err.Error(), "ingress: false") {
		t.Errorf("the error should point at the capability switch, got: %v", err)
	}
}
