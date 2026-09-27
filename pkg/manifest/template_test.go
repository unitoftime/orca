package manifest

import (
	"strings"
	"testing"
)

// Postgres ships with a 128MB shared_buffers and a 4GB effective_cache_size
// regardless of the machine, so a database given 8GB would use a sixteenth of
// it and plan as though it had half. These two are the difference between
// working and working badly.
func TestPostgresTuning(t *testing.T) {
	got := strings.Join(postgresTune(8*Gigabyte), " ")
	if !strings.Contains(got, "shared_buffers=2048MB") {
		t.Errorf("shared_buffers should be a quarter of memory, got %q", got)
	}
	if !strings.Contains(got, "effective_cache_size=4096MB") {
		t.Errorf("effective_cache_size should be half of memory, got %q", got)
	}
}

// Below Postgres's own floor the setting is refused, so a deliberately tiny
// database must be left at the default rather than failing to start.
func TestPostgresTuningSkipsTinyInstances(t *testing.T) {
	if got := postgresTune(32 * Megabyte); got != nil {
		t.Errorf("a tiny instance should keep the defaults, got %v", got)
	}
}

// A generated secret is named after its service so it stays visible in
// `orca secret list` and referenceable from anything that connects to it.
func TestGeneratedSecretName(t *testing.T) {
	if got := GeneratedSecret("db", "password"); got != "db_password" {
		t.Errorf("GeneratedSecret = %q, want db_password", got)
	}
}

func TestPostgresTemplateContract(t *testing.T) {
	spec := Templates["postgres"]

	// A bind mount keeps the host's ownership, so the image's user has to own
	// the directory or the database cannot write to its own data.
	if spec.VolumeUID == 0 {
		t.Error("postgres drops privileges and needs its volume owned")
	}
	var password *GeneratedSecretSpec
	for i, sec := range spec.Secrets {
		if sec.Env == "POSTGRES_PASSWORD" {
			password = &spec.Secrets[i]
		}
	}
	if password == nil || password.Suffix != "password" {
		t.Errorf("the password should be generated and injected, got %+v", spec.Secrets)
	}
	if !spec.VolumeRequired {
		t.Error("a database without a volume is a database that loses everything")
	}
}

// The generated secret has to be reported, since nothing references it.
func TestGeneratedSecretsAreReported(t *testing.T) {
	m, err := ParseGroup("shop", []byte("{name: db, template: postgres:17, volume: 2G}"), "shop/db.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got := m.GeneratedSecrets()
	if len(got) != 1 || got[0] != "db_password" {
		t.Errorf("GeneratedSecrets = %v, want [db_password]", got)
	}
	// And it is not a reference, so it must not appear as one.
	if len(m.Secrets()) != 0 {
		t.Errorf("Secrets = %v, want none: the template wires it in directly", m.Secrets())
	}
}

// Garage is not usable the moment its process starts: a fresh node holds no
// data at all until a layout is applied, and an S3 endpoint with no access key
// is one nobody can use. The template has to do both.
func TestGarageTemplateContract(t *testing.T) {
	spec := Templates["garage"]

	if spec.Init == nil {
		t.Fatal("garage needs an init step; a server with no layout silently stores nothing")
	}
	if spec.Config == nil {
		t.Fatal("garage is configured by file, not environment")
	}
	if !spec.VolumeRequired {
		t.Error("an object store without a volume loses everything put in it")
	}
	if spec.Port != 3900 {
		t.Errorf("port = %d, want the S3 API on 3900", spec.Port)
	}

	want := map[string]bool{"rpc_secret": true, "admin_token": true, "key_id": true, "secret_key": true}
	for _, sec := range spec.Secrets {
		delete(want, sec.Suffix)
		// None of them belongs in the environment: they are read from the
		// config file, which Nomad renders on the machine.
		if sec.Env != "" {
			t.Errorf("secret %q should not be injected as %q", sec.Suffix, sec.Env)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing generated secrets: %v", want)
	}
}

// The admin interface must not be reachable from outside the allocation: the
// init task shares the network namespace, so loopback is enough.
func TestGarageAdminBindsLoopback(t *testing.T) {
	if !strings.Contains(garageConfig, `api_bind_addr = "127.0.0.1:3903"`) {
		t.Errorf("the admin API should bind loopback:\n%s", garageConfig)
	}
	// The S3 API is the point, so it binds the allocation's address.
	if !strings.Contains(garageConfig, `api_bind_addr = "0.0.0.0:3900"`) {
		t.Errorf("the S3 API should be reachable:\n%s", garageConfig)
	}
}

// The init script runs again on every deploy, so nothing it does may fail the
// second time.
func TestGarageInitIsIdempotent(t *testing.T) {
	for _, want := range []string{
		"layout already assigned", // skipped once a role exists
		"already exists",          // creating the bucket twice is fine
		"already imported",        // importing the key twice is fine
	} {
		if !strings.Contains(garageInit, want) {
			t.Errorf("init should handle the already-done case %q:\n%s", want, garageInit)
		}
	}
}

// Garage's own formats: an access key id is "GK" and 24 hex characters.
func TestGarageKeyFormats(t *testing.T) {
	for _, sec := range Templates["garage"].Secrets {
		if sec.Suffix == "key_id" {
			if sec.Prefix != "GK" || !sec.Hex || sec.Bytes != 12 {
				t.Errorf("key_id = %+v, want GK plus 24 hex characters", sec)
			}
		}
		if sec.Suffix == "rpc_secret" && (!sec.Hex || sec.Bytes != 32) {
			t.Errorf("rpc_secret = %+v, want 32 bytes of hex", sec)
		}
	}
}

// Garage's v2 API takes the role in a "roles" list. The map form its v1 API
// used is still accepted with a 200 and stages nothing at all, so the apply
// that follows fails with "the number of nodes with positive capacity (0) is
// smaller than the replication factor" — an error about the wrong thing
// entirely, on a server that then stores nothing.
func TestGarageLayoutUsesTheRolesForm(t *testing.T) {
	if !strings.Contains(garageInit, "{roles: [{id: $n") {
		t.Errorf("layout must be staged as a roles list:\n%s", garageInit)
	}
}

// Applying a layout is not the same as the cluster having acted on it: for a
// short window every other call answers 500 "Layout not ready", which under
// set -e ends the script with curl's exit 22 and no explanation.
func TestGarageInitWaitsForLayoutReady(t *testing.T) {
	apply := strings.Index(garageInit, "ApplyClusterLayout")
	wait := strings.Index(garageInit, "layout never became ready")
	if apply < 0 || wait < 0 || wait < apply {
		t.Errorf("the init must wait for readiness after applying:\n%s", garageInit)
	}
}
