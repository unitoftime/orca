package deploy

import (
	"strings"
	"testing"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/pkg/manifest"
)

func backupSpecFixture() BackupSpec {
	return BackupSpec{
		Endpoint: "https://acct.r2.cloudflarestorage.com",
		Bucket:   "orca-backups",
		Region:   "auto",
		Schedule: "0 3 * * *",
		Keep:     14,
		Image:    "rclone/rclone:1.71.0",

		// Credentials live with the target that owns them, not under a
		// cluster-wide name.
		SecretGroup:     "storage",
		KeyIDSecret:     "offsite_key_id",
		SecretKeySecret: "offsite_secret_key",
	}
}

func backupJob(t *testing.T) (*manifest.Manifest, *manifest.Service) {
	t.Helper()
	m, err := manifest.ParseGroup("shop", []byte("{name: db, template: postgres:17, memory: 512M, volume: 2G}"), "shop/db.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return m, m.Services[0]
}

// Nomad interpolates ${...} in a task's config before the shell ever sees it,
// so a shell variable written with braces fails the job with "no variable
// named STAMP" rather than reaching bash. Only NOMAD_ prefixed names are
// Nomad's to substitute.
func TestBackupScriptsAvoidNomadInterpolation(t *testing.T) {
	m, s := backupJob(t)
	job := mustBackup(t, m, s)

	for _, task := range job.TaskGroups[0].Tasks {
		args, _ := task.Config["args"].([]string)
		script := strings.Join(args, " ")

		for _, open := range findBraced(script) {
			if !strings.HasPrefix(open, "NOMAD_") {
				t.Errorf("task %q uses ${%s}, which Nomad will try to interpolate", task.Name, open)
			}
		}
	}
}

// findBraced returns the names inside every ${...} in s.
func findBraced(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			return out
		}
		s = s[i+2:]
		j := strings.Index(s, "}")
		if j < 0 {
			return out
		}
		out = append(out, s[:j])
		s = s[j+1:]
	}
}

// The dump runs as a prestart task, so a failed dump stops the upload rather
// than shipping whatever happened to be on disk.
func TestDumpGatesTheUpload(t *testing.T) {
	m, s := backupJob(t)
	job := mustBackup(t, m, s)

	var dump *string
	for _, task := range job.TaskGroups[0].Tasks {
		if task.Name == "dump" {
			if task.Lifecycle == nil || task.Lifecycle.Hook != "prestart" {
				t.Errorf("dump lifecycle = %+v, want a prestart hook", task.Lifecycle)
			}
			dump = &task.Name
		}
	}
	if dump == nil {
		t.Fatal("no dump task")
	}
}

// The dump uses the database's own image, so the client is never older than
// the server — which pg_dump refuses outright.
func TestDumpUsesTheDatabaseImage(t *testing.T) {
	m, s := backupJob(t)
	job := mustBackup(t, m, s)

	for _, task := range job.TaskGroups[0].Tasks {
		if task.Name == "dump" && task.Config["image"] != "postgres:17-alpine" {
			t.Errorf("dump image = %v, want the database's own", task.Config["image"])
		}
	}
}

// A backup that overruns its schedule must not have a second copy started on
// top of it.
func TestBackupIsPeriodicAndNonOverlapping(t *testing.T) {
	m, s := backupJob(t)
	job := mustBackup(t, m, s)

	if *job.Type != "batch" {
		t.Errorf("type = %q, want batch", *job.Type)
	}
	if job.Periodic == nil || !*job.Periodic.Enabled {
		t.Fatal("the job should be periodic")
	}
	if *job.Periodic.Spec != "0 3 * * *" {
		t.Errorf("schedule = %q", *job.Periodic.Spec)
	}
	if !*job.Periodic.ProhibitOverlap {
		t.Error("overlapping backups must be prohibited")
	}
}

// Credentials are configured through the environment, so none is ever written
// to a file in the job spec.
func TestBackupCredentialsComeFromTheSecretStore(t *testing.T) {
	m, s := backupJob(t)
	job := mustBackup(t, m, s)

	var found bool
	for _, task := range job.TaskGroups[0].Tasks {
		for _, tmpl := range task.Templates {
			body := *tmpl.EmbeddedTmpl
			if strings.Contains(body, "RCLONE_CONFIG_STORE_ACCESS_KEY_ID") {
				found = true
				if !strings.Contains(body, `nomadVar "orca/storage/offsite_key_id"`) {
					t.Errorf("credentials should come from the secret store:\n%s", body)
				}
			}
		}
	}
	if !found {
		t.Error("the upload task has no credentials")
	}
}

// A periodic job's children inherit its metadata, so without filtering them
// every past backup run would appear as a service of its own.
func TestPeriodicChildrenAreRecognised(t *testing.T) {
	if !IsPeriodicChild("shop-db-backup/periodic-1790268603") {
		t.Error("a periodic child should be recognised")
	}
	if IsPeriodicChild("shop-db-backup") {
		t.Error("the parent is not a child")
	}
}

func TestBackupPrefixIsPerDatabase(t *testing.T) {
	if got := BackupPrefix("shop", "db"); got != "shop/db" {
		t.Errorf("BackupPrefix = %q, want shop/db", got)
	}
}

// Names are timestamps, so keeping the newest N is a sort and a count.
func TestPruneKeepsTheNewest(t *testing.T) {
	spec := backupSpecFixture()
	spec.Keep = 3
	script := uploadScript(spec, "shop/db")

	if !strings.Contains(script, "head -n -3") {
		t.Errorf("prune should keep the newest 3:\n%s", script)
	}
	if !strings.Contains(script, "sort") {
		t.Errorf("prune should sort before counting:\n%s", script)
	}
}

func mustBackup(t *testing.T, m *manifest.Manifest, s *manifest.Service) *nomad.Job {
	t.Helper()
	job, err := BuildBackup(m, s, "postgres:17-alpine", backupSpecFixture(), defaultOpts())
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// A template's arguments and the ones derived from its size are composed, not
// one replacing the other: garage's config path once overwrote whatever Tune
// produced, from a branch that knew garage's command line.
func TestTemplateArgsComeFromTheRegistry(t *testing.T) {
	job := buildOneIn(t, "files", "{name: store, template: garage:2.3.0, volume: 1G}", "store", defaultOpts())
	args, _ := job.TaskGroups[0].Tasks[0].Config["args"].([]string)
	spec := manifest.Templates["garage"]
	if strings.Join(args, " ") != strings.Join(spec.Args, " ") {
		t.Errorf("args = %v, want the registry's %v", args, spec.Args)
	}
	if !strings.Contains(strings.Join(spec.Args, " "), "/"+spec.Config.Path) {
		t.Errorf("garage's args must point at the config it renders (%s): %v", spec.Config.Path, spec.Args)
	}

	pg := buildOneIn(t, "shop", "{name: db, template: postgres:17, volume: 1G, memory: 4G}", "db", defaultOpts())
	if args, _ := pg.TaskGroups[0].Tasks[0].Config["args"].([]string); len(args) == 0 || !strings.Contains(strings.Join(args, " "), "shared_buffers=1024MB") {
		t.Errorf("postgres should be tuned to its memory, got %v", args)
	}
}

// The redis template runs as a database of record: its config is rendered
// from the variable store, it is pointed at that config, its dataset is capped
// below its allocation, and its password is never in the environment or on the
// command line.
func TestRedisTemplate(t *testing.T) {
	job := buildOneIn(t, "blog", "{name: redis, template: redis:8.10, volume: 1G, memory: 1G}", "redis", defaultOpts())
	task := job.TaskGroups[0].Tasks[0]

	if img := job.Meta[MetaImage]; img == "" {
		t.Fatal("no image")
	}
	args := strings.Join(task.Config["args"].([]string), " ")
	if args != "redis-server /local/redis.conf --maxmemory 768mb" {
		t.Errorf("args = %q", args)
	}

	var conf string
	for _, tmpl := range task.Templates {
		if *tmpl.DestPath == "local/redis.conf" {
			conf = *tmpl.EmbeddedTmpl
		}
		if tmpl.Envvars != nil && *tmpl.Envvars && strings.Contains(*tmpl.EmbeddedTmpl, "redis_password") {
			t.Error("the password should not be put in the environment")
		}
	}
	for _, want := range []string{
		`nomadVar "orca/blog/redis_password"`,
		"appendonly yes",
		"maxmemory-policy noeviction",
		"dir /data",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("redis.conf missing %q:\n%s", want, conf)
		}
	}

	if got := task.Config["volumes"].([]string)[0]; got != "/var/orca/volumes/services/blog/redis:/data" {
		t.Errorf("volume = %q", got)
	}
	svc := job.TaskGroups[0].Services[0]
	if svc.PortLabel != "6379" {
		t.Errorf("registered at %q, want 6379", svc.PortLabel)
	}
}

// A Redis backup is a snapshot the server streams over the replication
// protocol, taken with the database's own image and logged in with its
// generated password — which reaches redis-cli through the environment, never
// the command line.
func TestRedisBackupDumpsOverReplication(t *testing.T) {
	m, err := manifest.ParseGroup("blog", []byte("{name: redis, template: redis:8.10, volume: 2G, backup: {to: storage/offsite}}"), "blog/redis.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := m.Services[0]
	job := mustBackup(t, m, s)

	var dump *nomad.Task
	for _, task := range job.TaskGroups[0].Tasks {
		if task.Name == "dump" {
			dump = task
		}
	}
	if dump == nil {
		t.Fatal("no dump task")
	}
	script := strings.Join(dump.Config["args"].([]string), " ")
	if !strings.Contains(script, "--rdb") || !strings.Contains(script, "blog-redis-$STAMP.rdb") {
		t.Errorf("dump should stream an .rdb snapshot, got:\n%s", script)
	}
	if strings.Contains(script, " -a ") || strings.Contains(script, "--pass") || strings.Contains(script, "--user") {
		t.Errorf("the password must not be on the command line:\n%s", script)
	}
	env := *dump.Templates[0].EmbeddedTmpl
	for _, want := range []string{"REDISCLI_AUTH=", "orca/blog/redis_password", "REDISHOST=", `nomadService "blog-redis"`} {
		if !strings.Contains(env, want) {
			t.Errorf("dump env missing %q:\n%s", want, env)
		}
	}
	for _, open := range findBraced(script) {
		if !strings.HasPrefix(open, "NOMAD_") {
			t.Errorf("dump uses ${%s}, which Nomad will try to interpolate", open)
		}
	}
}
