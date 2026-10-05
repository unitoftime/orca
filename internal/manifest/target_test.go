package manifest

import (
	"strings"
	"testing"
)

func TestTargetParses(t *testing.T) {
	m, err := ParseGroup("storage", []byte(`
name: offsite
target: s3
endpoint: https://acct.r2.cloudflarestorage.com
bucket: orca-backups
`), "offsite.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, _ := m.Service("offsite")
	if !s.IsTarget() {
		t.Fatal("should be a target")
	}
	if s.Region != DefaultRegion {
		t.Errorf("region = %q, want the default %q", s.Region, DefaultRegion)
	}
	// A target runs nothing, so the sizing defaults every container gets must
	// not be applied to it: a cpu on a thing with no process is noise.
	if s.CPU != 0 || s.Replicas != 0 || s.Memory != 0 {
		t.Errorf("a target should not be sized: cpu=%v replicas=%d memory=%v", s.CPU, s.Replicas, s.Memory)
	}
}

func TestBackupDefaultsAreApplied(t *testing.T) {
	m, err := ParseGroup("shop", []byte(
		"{name: db, template: 'postgres:17', volume: 1G, backup: {to: storage/offsite}}"), "db.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, _ := m.Service("db")
	if s.Backup.Schedule != DefaultBackupSchedule || s.Backup.Keep != DefaultBackupKeep {
		t.Errorf("backup defaults not applied: %+v", s.Backup)
	}
}

func TestTargetValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			"unknown target kind",
			"{name: x, target: gcs, endpoint: e, bucket: b}",
			`unknown target "gcs"`,
		},
		{
			"target without an endpoint",
			"{name: x, target: s3, bucket: b}",
			"needs an endpoint",
		},
		{
			"target without a bucket",
			"{name: x, target: s3, endpoint: e}",
			"needs a bucket",
		},
		{
			// It runs no container, so anything describing one would look
			// effective and do nothing.
			"target with container fields",
			"{name: x, target: s3, endpoint: e, bucket: b, cpu: 1, volume: 1G}",
			"runs no container",
		},
		{
			"both an image and a target",
			"{name: x, image: i:1, target: s3, endpoint: e, bucket: b}",
			"declares more than one of image, template and target",
		},
		{
			// These belong to a target. On a container they are almost
			// certainly a backup policy written in the wrong shape.
			"bucket on an ordinary service",
			"{name: x, image: i:1, bucket: b}",
			"belong to a target",
		},
		{
			"backup.to that is not group/service",
			"{name: x, template: 'postgres:17', volume: 1G, backup: {to: offsite}}",
			"must be <group>/<service>",
		},
		{
			// orca can back up a database because it knows what one is. It
			// cannot back up an arbitrary volume safely.
			"backup on a plain image",
			"{name: x, image: i:1, volume: {size: 1G, mount: /d}, backup: {to: storage/offsite}}",
			"only available on a template that knows how to dump itself",
		},
		{
			"unknown field inside backup",
			"{name: x, template: 'postgres:17', volume: 1G, backup: {to: a/b, evry: 5m}}",
			`unknown field "evry"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseGroup("g", []byte(tt.body), "g.yaml")
			if err == nil {
				t.Fatalf("expected an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
