package main

import (
	"strings"
	"testing"

	"github.com/unitoftime/orca/pkg/manifest"
)

func groupsFrom(t *testing.T, files map[string]string) []*manifest.Manifest {
	t.Helper()
	files["cluster.yaml"] = "nodes:\n  - host: root@203.0.113.10\n"
	root := writeTree(t, files)
	cfg, err := LoadConfig(root + "/cluster.yaml")
	if err != nil {
		t.Fatal(err)
	}
	groups, err := loadGroups(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return groups
}

const offsiteTarget = `
name: offsite
target: s3
endpoint: https://acct.r2.cloudflarestorage.com
bucket: orca-backups
`

// The whole point of a target: the endpoint and the credentials are written
// once, and every database that backs up refers to them by name.
func TestBackupResolvesThroughTheTarget(t *testing.T) {
	groups := groupsFrom(t, map[string]string{
		"storage/offsite.yaml": offsiteTarget,
		"shop/db.yaml": `
name: db
template: postgres:17
volume: 1G
backup:
  to: storage/offsite
  keep: 3
`,
	})

	var db *manifest.Service
	var dbGroup *manifest.Manifest
	for _, m := range groups {
		if s, ok := m.Service("db"); ok {
			db, dbGroup = s, m
		}
	}
	if db == nil {
		t.Fatal("no db service")
	}

	spec, err := resolveBackup(groups, dbGroup, db)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Endpoint != "https://acct.r2.cloudflarestorage.com" || spec.Bucket != "orca-backups" {
		t.Errorf("spec did not come from the target: %+v", spec)
	}
	// Unset on the target, so it takes the default rather than being empty:
	// most S3-compatible stores ignore the region but require it set.
	if spec.Region != manifest.DefaultRegion {
		t.Errorf("region = %q, want %q", spec.Region, manifest.DefaultRegion)
	}
	// Schedule and keep are the database's, not the target's: one noisy
	// database can keep more history without everything else doing the same.
	if spec.Keep != 3 || spec.Schedule != manifest.DefaultBackupSchedule {
		t.Errorf("schedule/keep should come from the database: %+v", spec)
	}
	// Credentials follow the target's group, so deleting that group takes them
	// with it.
	if spec.SecretGroup != "storage" || spec.KeyIDSecret != "offsite_key_id" ||
		spec.SecretKeySecret != "offsite_secret_key" {
		t.Errorf("credentials should live with the target: %+v", spec)
	}
}

// A target's credentials are required, not generated, so apply's existing
// preflight has to demand them. Otherwise the job is created, runs on
// schedule, fails inside a container nobody is watching, and the first anyone
// knows is when a restore is needed.
func TestTargetCredentialsAreRequiredSecrets(t *testing.T) {
	groups := groupsFrom(t, map[string]string{"storage/offsite.yaml": offsiteTarget})

	var got []string
	for _, m := range groups {
		if m.App == "storage" {
			got = m.Secrets()
		}
	}
	want := map[string]bool{"offsite_key_id": true, "offsite_secret_key": true}
	for _, n := range got {
		delete(want, n)
	}
	if len(want) != 0 {
		t.Errorf("Secrets() = %v, missing %v", got, want)
	}
}

func TestBackupReferenceErrors(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{
			"target that does not exist",
			map[string]string{"shop/db.yaml": "{name: db, template: 'postgres:17', volume: 1G, backup: {to: storage/nope}}"},
			"names no service",
		},
		{
			// Pointing a backup at a database rather than a store would create
			// the job and fail inside a container nobody is watching.
			"target that is a container",
			map[string]string{
				"storage/offsite.yaml": offsiteTarget,
				"shop/db.yaml":         "{name: db, template: 'postgres:17', volume: 1G, backup: {to: shop/other}}",
				"shop/other.yaml":      "{name: other, image: i:1}",
			},
			"is a service, not a target",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups := groupsFrom(t, tt.files)
			_, err := collectBackups(groups, groups)
			if err == nil {
				t.Fatalf("expected an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			// The error has to name what does exist, or the only way to find
			// the right spelling is to go reading directories.
			if tt.name == "target that is a container" && !strings.Contains(err.Error(), "storage/offsite") {
				t.Errorf("error should list the declared targets, got %v", err)
			}
		})
	}
}
