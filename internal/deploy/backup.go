package deploy

import (
	"fmt"
	"strings"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/internal/manifest"
)

// BackupSpec is where backups go. Any S3-compatible store: R2, Backblaze,
// Wasabi, MinIO and S3 itself differ only in the endpoint.
type BackupSpec struct {
	Endpoint string
	Bucket   string
	Region   string
	Schedule string
	Keep     int

	// Prefix is where in the bucket this database's backups live; see
	// BackupPrefix.
	Prefix string

	// Image is the client used to talk to the store.
	Image string

	// SecretGroup, KeyIDSecret and SecretKeySecret locate the credentials in
	// the secret store. They live with the target that owns them rather than
	// under a cluster-wide name, so two targets can have two sets of keys and
	// deleting the group that declared one takes its credentials with it.
	SecretGroup     string
	KeyIDSecret     string
	SecretKeySecret string
}

// IsPeriodicChild reports a job Nomad created from a periodic parent. They
// carry the parent's metadata, so without this every past backup run would
// appear in `orca status` as its own service.
func IsPeriodicChild(jobID string) bool { return strings.Contains(jobID, "/periodic-") }

// BuildBackup renders the periodic job that backs up one templated service.
//
// Two tasks sharing the allocation's directory, both stock images: the
// database's own image dumps (so the client is never older than the server,
// which pg_dump refuses), and an S3 client uploads and prunes. Nothing is
// built, and nothing has to be kept in step with the database's version by
// hand.
func BuildBackup(m *manifest.Manifest, s *manifest.Service, image string, spec BackupSpec, opts Options) (*nomad.Job, error) {
	id := BackupJobID(m.App, s.Name)

	// The dump is the one part that knows what kind of data this is. Stated
	// as a switch on the template's backup kind, so a template that declares
	// a kind this does not know is an error here rather than a Postgres dump
	// pointed at something that is not Postgres.
	t := s.Tmpl()
	if t == nil {
		return nil, fmt.Errorf("%s/%s: only a templated service can be backed up", m.App, s.Name)
	}
	var script, env string
	switch kind := t.Spec().Backup; kind {
	case manifest.BackupPostgres:
		script, env = postgresDumpScript(m.App, s.Name), dumpEnv(m.App, s.Name)
	case manifest.BackupRedis:
		script, env = redisDumpScript(m.App, s.Name), redisDumpEnv(m.App, s.Name)
	default:
		return nil, fmt.Errorf("%s/%s: template %s has no backup of kind %q", m.App, s.Name, t, kind)
	}

	dump := &nomad.Task{
		Name:   "dump",
		Driver: "docker",
		Config: map[string]any{
			"image":      image,
			"entrypoint": []string{"/bin/sh", "-c"},
			"args":       []string{script},
		},
		Lifecycle: &nomad.TaskLifecycle{
			Hook:    "prestart",
			Sidecar: false,
		},
		Resources: &nomad.Resources{CPU: ptr(200), MemoryMB: ptr(256)},
		Templates: []*nomad.Template{{
			EmbeddedTmpl: ptr(env),
			DestPath:     ptr("secrets/" + manifest.EnvFile),
			Envvars:      ptr(true),
			ChangeMode:   ptr("noop"),
		}},
	}

	upload := &nomad.Task{
		Name:   "upload",
		Driver: "docker",
		Config: map[string]any{
			"image":      spec.Image,
			"entrypoint": []string{"/bin/sh", "-c"},
			"args":       []string{uploadScript(spec)},
		},
		Resources: &nomad.Resources{CPU: ptr(200), MemoryMB: ptr(256)},
		Templates: []*nomad.Template{{
			EmbeddedTmpl: ptr(uploadEnv(spec)),
			DestPath:     ptr("secrets/" + manifest.EnvFile),
			Envvars:      ptr(true),
			ChangeMode:   ptr("noop"),
		}},
	}

	group := &nomad.TaskGroup{
		Name:  ptr("backup"),
		Count: ptr(1),
		Tasks: []*nomad.Task{dump, upload},
		// A failed backup is retried a couple of times and then left failed,
		// rather than restarted forever. The next scheduled run is the real
		// retry, and a job that keeps restarting hides that it is failing.
		RestartPolicy: &nomad.RestartPolicy{
			Attempts: ptr(2),
			Interval: durPtr("10m"),
			Delay:    durPtr("30s"),
			Mode:     ptr("fail"),
		},
		Networks: []*nomad.NetworkResource{{Mode: "bridge"}},
	}

	if opts.DNS {
		// The dump reaches the database by name, like anything else.
		group.Networks[0].DNS = &nomad.DNSConfig{
			Servers:  []string{DNSAddress},
			Searches: SearchDomains(m.App),
		}
	}
	if opts.Node != "" {
		group.Constraints = append(group.Constraints, &nomad.Constraint{
			LTarget: "${node.unique.name}", RTarget: opts.Node, Operand: "=",
		})
	}

	job := &nomad.Job{
		ID:          ptr(id),
		Name:        ptr(id),
		Type:        ptr("batch"),
		Datacenters: []string{opts.Datacenter},
		TaskGroups:  []*nomad.TaskGroup{group},
		Periodic: &nomad.PeriodicConfig{
			Enabled:  ptr(true),
			Spec:     ptr(spec.Schedule),
			SpecType: ptr("cron"),
			// A backup that overruns its schedule must not have a second copy
			// started on top of it.
			ProhibitOverlap: ptr(true),
		},
		Meta: map[string]string{
			MetaManaged: "true",
			MetaApp:     m.App,
			MetaService: s.Name + "-backup",
			MetaImage:   image,
		},
	}
	return job, nil
}

// postgresDumpScript writes every database, and the roles, into one tar in
// the allocation directory, where the upload task picks it up.
//
// Every database rather than only postgres: the databases an application
// creates for itself are the ones worth keeping. And the roles, which no
// database's dump carries, so that owners and grants restore onto a server
// that has never seen them. The superuser is left out of those: its password
// is this cluster's own secret, and restoring another's would lock orca out.
func postgresDumpScript(group, service string) string {
	// Shell variables are written unbraced on purpose. Nomad interpolates
	// ${...} in a task's config before the shell ever sees it, so ${STAMP}
	// fails the job with "no variable named STAMP" rather than reaching bash.
	// ${NOMAD_ALLOC_DIR} is left braced because it really is Nomad's to
	// substitute.
	return fmt.Sprintf(`set -eu
set -o pipefail
STAMP=$(date -u +%%Y%%m%%dT%%H%%M%%SZ)
DATA="${NOMAD_ALLOC_DIR}/data"
WORK="$DATA/work"
OUT="$DATA/%s-%s-$STAMP.%s"
rm -rf "$WORK"
mkdir -p "$WORK"

# Host and port come from PGHOST/PGPORT, rendered from the service catalog.
# Naming the service and assuming 5432 is right only while the database
# registers its own address; once a port is published instead, the catalog
# carries a dynamic one and the assumption connects to nothing.
pg_dumpall -U postgres --globals-only | grep -vE '^(CREATE|ALTER) ROLE postgres( |;)' > "$WORK/globals.sql"

# -Fc is the custom format: compressed, and restorable selectively rather than
# as one all-or-nothing script. What a restore keeps of a database it
# replaced (<name>%s<time>) is a copy, not something to back up again.
psql -U postgres -AtX -c "SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate AND strpos(datname, '%s') = 0" |
while IFS= read -r DB; do
  pg_dump -Fc -U postgres -d "$DB" -f "$WORK/$DB.pgc"
done

tar -cf "$OUT" -C "$WORK" .
rm -rf "$WORK"
echo "$OUT" > "$DATA/latest"
ls -lh "$OUT"`, group, service, manifest.BackupPostgres.Ext(), PreRestoreSuffix, PreRestoreSuffix)
}

func dumpEnv(group, service string) string {
	f := envFile{group: group}
	f.setSecret("PGPASSWORD", manifest.GeneratedSecret(service, manifest.PasswordSuffix))
	return f.String() + fmt.Sprintf(`{{ range nomadService %q }}PGHOST="{{ .Address }}"
PGPORT="{{ .Port }}"
{{ end }}`, CatalogName(group, service))
}

// redisDumpScript has the server write a snapshot and stream it over the
// replication protocol, which is how a replica is seeded. The snapshot is a
// consistent point in time taken while the server keeps serving, and nothing
// has to be read out of the volume while Redis is writing to it.
func redisDumpScript(group, service string) string {
	// Unbraced shell variables for the reason postgresDumpScript gives.
	return fmt.Sprintf(`set -eu
STAMP=$(date -u +%%Y%%m%%dT%%H%%M%%SZ)
mkdir -p "${NOMAD_ALLOC_DIR}/data"
OUT="${NOMAD_ALLOC_DIR}/data/%s-%s-$STAMP.%s"

# The password arrives as REDISCLI_AUTH, which redis-cli reads itself, so it
# is never on a command line.
redis-cli -h "$REDISHOST" -p "$REDISPORT" --rdb "$OUT"

echo "$OUT" > "${NOMAD_ALLOC_DIR}/data/latest"
ls -lh "$OUT"`, group, service, manifest.BackupRedis.Ext())
}

func redisDumpEnv(group, service string) string {
	f := envFile{group: group}
	f.setSecret("REDISCLI_AUTH", manifest.GeneratedSecret(service, manifest.PasswordSuffix))
	return f.String() + fmt.Sprintf(`{{ range nomadService %q }}REDISHOST="{{ .Address }}"
REDISPORT="{{ .Port }}"
{{ end }}`, CatalogName(group, service))
}

// uploadEnv configures the S3 remote entirely through the environment, so no
// credential is ever written to a file on the machine.
func uploadEnv(spec BackupSpec) string {
	f := envFile{group: spec.SecretGroup}
	f.setLiteral("RCLONE_CONFIG_STORE_TYPE", "s3")
	f.setLiteral("RCLONE_CONFIG_STORE_PROVIDER", "Other")
	f.setLiteral("RCLONE_CONFIG_STORE_ENDPOINT", spec.Endpoint)
	f.setLiteral("RCLONE_CONFIG_STORE_REGION", spec.Region)
	// rclone otherwise makes sure the bucket exists before every upload, by
	// trying to create it. A key allowed only to read and write objects in
	// one bucket, which is the key a backup should have, is refused that, and
	// the upload fails. The bucket is yours to have made; a missing one fails
	// the upload on its own, by name.
	f.setLiteral("RCLONE_CONFIG_STORE_NO_CHECK_BUCKET", "true")
	f.setSecret("RCLONE_CONFIG_STORE_ACCESS_KEY_ID", spec.KeyIDSecret)
	f.setSecret("RCLONE_CONFIG_STORE_SECRET_ACCESS_KEY", spec.SecretKeySecret)
	return f.String()
}

// uploadScript ships the dump and prunes old ones.
//
// Object names are timestamps, so lexicographic order is chronological and
// keeping the newest N is a sort and a count rather than anything that has to
// parse a date.
func uploadScript(spec BackupSpec) string {
	remote := fmt.Sprintf("store:%s/%s", spec.Bucket, spec.Prefix)
	return fmt.Sprintf(`set -eu
FILE=$(cat "${NOMAD_ALLOC_DIR}/data/latest")
NAME=$(basename "$FILE")

rclone copyto "$FILE" "%[1]s/$NAME"
echo "uploaded %[2]s/$NAME"

# Uploaded, so the copy on this machine's disk is only a plaintext duplicate
# of the whole database, kept until Nomad gets round to collecting the run.
rm -f "$FILE"

# Keep the newest %[3]d. Names are timestamps, so sorting them sorts by age.
rclone lsf "%[1]s/" 2>/dev/null | sort | head -n -%[3]d | while read -r old; do
  [ -n "$old" ] || continue
  echo "pruning $old"
  rclone delete "%[1]s/$old"
done

echo "backups now held:"
rclone lsf "%[1]s/" | sort`, remote, spec.Prefix, spec.Keep)
}
