package manifest

import (
	"fmt"
	"sort"
	"strings"

	"github.com/unitoftime/orca/internal/images"
)

// Template is a known-good piece of infrastructure orca operates for you,
// selected as `template: postgres:17`. It is sugar: it expands into an ordinary
// service, so deploys, logs, metrics and discovery need no new machinery.
//
// The template owns everything that has to be correct and is tedious or
// dangerous to get right by hand: the pinned image, the port, the health
// check, the generated password, where the volume mounts, and how it is
// dumped for a backup. You tune only size.
type Template struct {
	Name    string
	Version string
}

// TemplateSpec is what a template fixes. Fields an author sets that appear here
// are rejected rather than silently overridden.
type TemplateSpec struct {
	// Versions are the versions orca will run, newest first, each with the
	// exact image it runs. Pinned rather than open-ended because "we run this
	// and have tested the restore path" is the entire promise of a template.
	Versions []TemplateVersion

	// Port is the service's primary port: its discovery registration and the
	// health-check target.
	Port int

	// VolumeMount is where a declared volume lands, which is why a templated
	// service may write `volume: 20G` with no mount.
	VolumeMount string

	// VolumeRequired marks a template whose whole point is durable state.
	VolumeRequired bool

	// DefaultMemory applies when the author does not size it. Databases need
	// considerably more than an average service, so the generic default would
	// be actively harmful here.
	DefaultMemory Size

	// VersionFile is a file in the volume naming the version its data was
	// written by, empty when the data has none. Apply compares it with the
	// template's version before deploying, for a template that cannot start
	// on another version's data.
	VersionFile string

	// SharedMemory sizes /dev/shm from the memory asked for, nil to leave
	// Docker's 64MB.
	SharedMemory func(memory Size) Size

	// VolumeUID is the user the image runs as, and therefore who must own the
	// volume directory. A bind mount keeps the host's ownership, so a
	// root-owned directory handed to a process running as someone else fails
	// at startup with a permission error that says nothing about volumes.
	VolumeUID int

	// Secrets are values orca generates on first apply, stores in the cluster,
	// and never shows. The best secret is one nobody handles.
	Secrets []GeneratedSecretSpec

	// Config is a file the template renders and the image reads, rendered by
	// Nomad so it can pull secrets from the variable store at run time.
	// Empty for an image configured entirely by environment.
	Config *TemplateFile

	// Init is a one-shot task that runs after the service starts, for a
	// template that is not usable the moment its process is up. It must be
	// idempotent: it runs again on every deploy.
	Init *TemplateFile

	// Backup names how this template is backed up, empty for a template with
	// no data worth keeping.
	Backup BackupKind

	// Entrypoint overrides the image's, for an image that declares none and
	// whose command is therefore replaced by arguments rather than extended
	// by them.
	Entrypoint []string

	// Args are the image's arguments. Tune's are appended after them rather
	// than replacing them, so a template can have both without one silently
	// dropping the other.
	Args []string

	// Tune returns image arguments derived from the size the author asked
	// for. Defaults built for a 1990s machine are the most common reason a
	// database that "just runs" runs badly.
	Tune func(memory Size) []string
}

// TemplateVersion is one version a template offers: the name an author
// writes after the colon, and the image that runs. Images come from
// internal/images, pinned to a digest, so a database is never restarted because a
// tag moved upstream.
type TemplateVersion struct {
	Name  string
	Image string
}

// BackupKind is how a template's data is dumped for a backup.
type BackupKind string

const (
	// BackupPostgres dumps every database with pg_dump, and the roles with
	// pg_dumpall, into one tar, and restores with pg_restore.
	BackupPostgres BackupKind = "postgres"

	// BackupRedis copies a snapshot over the replication protocol with
	// redis-cli --rdb, and restores by loading it into a stopped server.
	BackupRedis BackupKind = "redis"
)

// Ext is the file extension a backup of this kind is written with. Part of
// the name so a bucket listing says what each file is, and so a restore can
// refuse a file of the wrong kind before it touches anything.
func (k BackupKind) Ext() string {
	return k.Exts()[0]
}

// Exts is every extension a restore of this kind reads: what it writes now,
// then what earlier versions of orca wrote, so a backup taken before an
// upgrade still restores after it.
func (k BackupKind) Exts() []string {
	switch k {
	case BackupPostgres:
		// .pgc is a single custom-format dump of the postgres database.
		return []string{"tar", "pgc"}
	case BackupRedis:
		return []string{"rdb"}
	}
	return []string{""}
}

// PasswordSuffix names the password a database template generates:
// <service>_password. Backup and restore log in with it, so it is named once
// rather than spelled out wherever it is read.
const PasswordSuffix = "password"

// Templates is the registry. Deliberately tiny: each one is here because
// something concrete needed it, and the rest can wait until something does.
var Templates = map[string]TemplateSpec{
	"postgres": {
		Versions:       []TemplateVersion{{"17", images.Postgres17}, {"16", images.Postgres16}},
		Port:           5432,
		VolumeMount:    "/var/lib/postgresql/data",
		VolumeRequired: true,
		DefaultMemory:  1 * Gigabyte,
		VolumeUID:      70, // the alpine image's postgres user
		Backup:         BackupPostgres,
		Secrets: []GeneratedSecretSpec{
			// The image sets it when it creates the database and never again.
			{Suffix: PasswordSuffix, Env: "POSTGRES_PASSWORD", SetAtInit: true},
		},
		// Postgres refuses to start on a data directory written by another
		// major version.
		VersionFile: "PG_VERSION",
		// Parallel queries share their working memory through /dev/shm, and
		// Docker's 64MB fails them with "could not resize shared memory
		// segment" on anything sizable. It counts against the memory limit
		// only as it is used.
		SharedMemory: func(memory Size) Size { return max(memory/4, 64*Megabyte) },
		Tune:         postgresTune,
	},

	// Redis, for a service whose state lives in it, and so run as a database
	// of record rather than a cache: every write is appended to disk and
	// fsynced each second, and nothing is ever evicted.
	//
	// The official image, which from Redis 8 carries JSON, search, time series
	// and probabilistic types itself; Redis Stack, which used to be how you got
	// them, is discontinued.
	"redis": {
		Versions:       []TemplateVersion{{"8.10", images.Redis810}},
		Args:           []string{"redis-server", "/secrets/redis.conf"},
		Port:           6379,
		VolumeMount:    "/data",
		VolumeRequired: true,
		DefaultMemory:  512 * Megabyte,
		VolumeUID:      999, // the alpine image's redis user
		Backup:         BackupRedis,
		Secrets: []GeneratedSecretSpec{
			// Read from the config file rather than the environment: the
			// image has no variable for it, and on the command line it would
			// sit in the host's process table.
			{Suffix: PasswordSuffix},
		},
		Config: &TemplateFile{
			Path: "secrets/redis.conf",
			Body: redisConfig,
		},
		Tune: redisTune,
	},

	// Garage is a self-hosted, S3-compatible object store: somewhere to put
	// the things a filesystem is the wrong home for, without renting a bucket.
	//
	// Unlike a database, it is not usable the moment its process is up. A
	// fresh Garage node holds no data at all until a storage layout is
	// assigned and applied, and an S3 endpoint with no access key is an S3
	// endpoint nobody can use. The template does both, and hands back a key
	// the way postgres hands back a password.
	"garage": {
		Versions:       []TemplateVersion{{"2.3.0", images.Garage230}},
		Entrypoint:     []string{"/garage"},
		Args:           []string{"-c", "/secrets/garage.toml", "server"},
		Port:           3900, // the S3 API
		VolumeMount:    "/var/lib/garage",
		VolumeRequired: true,
		DefaultMemory:  512 * Megabyte,
		Secrets: []GeneratedSecretSpec{
			// Garage's own formats: the RPC secret and the S3 secret key are
			// 32 bytes of hex, and an access key id is "GK" and 24 more.
			{Suffix: "rpc_secret", Bytes: 32, Hex: true},
			{Suffix: "admin_token", Bytes: 32, Hex: true},
			{Suffix: "key_id", Prefix: "GK", Bytes: 12, Hex: true},
			{Suffix: "secret_key", Bytes: 32, Hex: true},
		},
		Config: &TemplateFile{
			Path: "secrets/garage.toml",
			Body: garageConfig,
		},
		Init: &TemplateFile{
			Path: "secrets/init.sh",
			Body: garageInit,
			// Garage's image holds the binary and nothing else (no shell, no
			// coreutils), so the setup it still needs cannot run inside it.
			// This drives the admin API instead, reached on loopback because
			// the task shares the allocation's network namespace.
			Image: images.Alpine,
		},
	},
}

// garageConfig is rendered by Nomad, so the secrets come from the variable
// store when the task starts rather than from anything orca writes down.
//
// The admin API binds loopback: tasks in an allocation share a network
// namespace, so the init script reaches it while nothing outside the
// allocation can. The S3 API binds the allocation's own address, which is what
// other services resolve by name.
const garageConfig = `metadata_dir = "/var/lib/garage/meta"
data_dir = "/var/lib/garage/data"
db_engine = "lmdb"

replication_factor = 1

# Written to disk before a write is acknowledged. Garage leaves both off by
# default, trusting a second copy on another node to survive a crash; here
# there is no second copy, and an unsynced metadata database can be corrupt
# after a power loss.
metadata_fsync = true
data_fsync = true

rpc_bind_addr = "0.0.0.0:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "{{ with nomadVar "%[1]s_rpc_secret" }}{{ .value }}{{ end }}"

[s3_api]
s3_region = "orca"
api_bind_addr = "0.0.0.0:3900"
root_domain = ".s3.orca"

[admin]
api_bind_addr = "127.0.0.1:3903"
admin_token = "{{ with nomadVar "%[1]s_admin_token" }}{{ .value }}{{ end }}"
`

// garageInit finishes what starting the process does not.
//
// Every step is idempotent, because this runs on every deploy: the layout is
// skipped once a role is assigned, and creating a bucket or importing a key
// that already exists is not an error worth failing on.
const garageInit = `set -eu
# Quiet when it works. When it does not, what apk said is the only account of
# why: usually that its package servers could not be reached.
if ! OUT=$(apk add --no-cache curl jq 2>&1); then
  echo "could not install curl and jq, which setting Garage up needs:" >&2
  printf '%%s\n' "$OUT" >&2
  exit 1
fi

TOKEN="{{ with nomadVar "%[1]s_admin_token" }}{{ .value }}{{ end }}"
KEY_ID="{{ with nomadVar "%[1]s_key_id" }}{{ .value }}{{ end }}"
SECRET_KEY="{{ with nomadVar "%[1]s_secret_key" }}{{ .value }}{{ end }}"
BUCKET="%[2]s"
API="http://127.0.0.1:3903/v2"

api() {
  METHOD=$1; ROUTE=$2; BODY=${3:-}
  if [ -n "$BODY" ]; then
    curl -sf -X "$METHOD" -H "Authorization: Bearer $TOKEN" \
      -H "Content-Type: application/json" -d "$BODY" "$API/$ROUTE"
  else
    curl -sf -X "$METHOD" -H "Authorization: Bearer $TOKEN" "$API/$ROUTE"
  fi
}

for i in $(seq 1 60); do
  api GET GetClusterStatus >/dev/null 2>&1 && break
  sleep 2
done
api GET GetClusterStatus >/dev/null 2>&1 || { echo "garage admin API never answered" >&2; exit 1; }

# A fresh node holds no data at all until a layout is applied. Without this
# the server accepts writes and stores none of them, which is the failure the
# whole step exists to prevent.
STATUS=$(api GET GetClusterStatus)
NODE=$(printf '%%s' "$STATUS" | jq -r '.nodes[0].id')
ROLE=$(printf '%%s' "$STATUS" | jq -r '.nodes[0].role // empty')

if [ -z "$ROLE" ] || [ "$ROLE" = "null" ]; then
  VER=$(api GET GetClusterLayout | jq -r '.version')
  # The role goes in a "roles" list. The map form {node: {...}} that Garage's
  # v1 API took is still accepted by v2 with a 200 and stages nothing at all,
  # so the apply that follows fails with "number of nodes with positive
  # capacity (0) is smaller than the replication factor", an error about the
  # wrong thing entirely.
  api POST UpdateClusterLayout "$(jq -n --arg n "$NODE" \
    '{roles: [{id: $n, zone: "orca", capacity: 1000000000000, tags: []}]}')" >/dev/null
  api POST ApplyClusterLayout "$(jq -n --argjson v "$((VER + 1))" '{version: $v}')" >/dev/null
  echo "layout applied at version $((VER + 1)) for node $NODE"
else
  echo "layout already assigned"
fi

# Applying a layout is not the same as the cluster having finished acting on
# it. For a short window afterward every other call answers 500 "Layout not
# ready", which under set -e ends the script with curl's exit 22 and no
# explanation at all.
for i in $(seq 1 60); do
  api GET ListKeys >/dev/null 2>&1 && break
  sleep 2
done
api GET ListKeys >/dev/null 2>&1 || { echo "layout never became ready" >&2; exit 1; }

# An S3 endpoint with no access key is one nobody can use, so the template
# hands one back the way the database template hands back a password.
if api GET "GetKeyInfo?id=$KEY_ID" >/dev/null 2>&1; then
  echo "key already imported"
else
  api POST ImportKey "$(jq -n --arg k "$KEY_ID" --arg s "$SECRET_KEY" \
    '{accessKeyId: $k, secretAccessKey: $s, name: "orca"}')" >/dev/null
  echo "imported key $KEY_ID"
fi

BUCKET_ID=$(api GET "GetBucketInfo?globalAlias=$BUCKET" 2>/dev/null | jq -r '.id // empty')
if [ -z "$BUCKET_ID" ]; then
  BUCKET_ID=$(api POST CreateBucket "$(jq -n --arg b "$BUCKET" '{globalAlias: $b}')" | jq -r '.id')
  echo "created bucket $BUCKET"
else
  echo "bucket $BUCKET already exists"
fi

api POST AllowBucketKey "$(jq -n --arg b "$BUCKET_ID" --arg k "$KEY_ID" \
  '{bucketId: $b, accessKeyId: $k, permissions: {read: true, write: true, owner: false}}')" >/dev/null

echo "garage ready: bucket $BUCKET, key $KEY_ID"
`

// redisConfig is rendered by Nomad, so the password comes from the variable
// store when the task starts.
//
// appendonly is the setting that makes this a database: without it Redis
// only snapshots now and then, and a crash loses every write since the last
// one. With it a crash loses at most a second. The snapshot a backup takes is
// separate: redis-cli --rdb has the server produce one on demand.
const redisConfig = `requirepass "{{ with nomadVar "%[1]s_password" }}{{ .value }}{{ end }}"
dir /data
appendonly yes
appendfsync everysec
maxmemory-policy noeviction
`

// redisTune caps the dataset below the allocation.
//
// Past the cap a write is refused with an error the application sees, where
// without one the container is killed by the kernel and restarts, losing
// the last second of writes, and doing it again at the next write. The
// quarter left over is for what Redis does not count as its dataset: the
// copy-on-write pages of the fork that writes a snapshot or rewrites the AOF,
// and fragmentation.
func redisTune(memory Size) []string {
	return []string{"--maxmemory", fmt.Sprintf("%dmb", memory*3/4/Megabyte)}
}

// GeneratedSecretSpec describes one value orca generates for a template.
type GeneratedSecretSpec struct {
	// Suffix names the secret: <service>_<suffix>.
	Suffix string

	// Env is an environment variable to inject the value as, empty when the
	// image reads it from a config file instead.
	Env string

	// Prefix is prepended to the generated value, for a format that demands
	// one: a Garage access key id begins "GK".
	Prefix string

	// Bytes of randomness. Zero takes the default length.
	Bytes int

	// Hex encodes the value as hex rather than alphanumerics, for a format
	// that requires it.
	Hex bool

	// SetAtInit marks a value the image uses only when it initializes an
	// empty volume. The data keeps the value it was created with, so a new
	// one generated over existing data locks everything out of it.
	SetAtInit bool
}

// TemplateFile is a file a template needs on the machine: the config the image
// reads, or the script that finishes setting it up.
type TemplateFile struct {
	// Path is where it lands inside the container. Under secrets/, since
	// every one of them carries a secret: that directory is a private tmpfs,
	// where local/ is the machine's disk and outlives the task.
	Path string

	// Body is rendered by Nomad, so it may read secrets with nomadVar.
	// %[1]s is the service's secret path prefix, %[2]s its bucket name.
	Body string

	// Image runs it, for a file that is a script. Empty uses the service's
	// own image, which only works when that image has a shell. Garage's is
	// the binary and nothing else, which is why this exists.
	Image string
}

// GeneratedSecret is the name of a secret orca creates for a templated
// service. Naming it after the service keeps it visible in `orca secret list`
// and referenceable as ${secret.db_password} from anything that needs it.
func GeneratedSecret(service, suffix string) string {
	return service + "_" + suffix
}

// postgresTune sets the two settings whose defaults are wrong on any modern
// machine.
//
// Postgres ships with a 128MB shared_buffers and a 4GB effective_cache_size
// regardless of what it is running on, so a database given 8GB uses a
// sixteenth of it and plans as though it had half. Everything else is left
// alone: these two are the difference between working and working badly, and
// the rest is tuning that depends on a workload orca knows nothing about.
func postgresTune(memory Size) []string {
	shared := memory / 4
	cache := memory / 2

	// Below Postgres's own floor the setting is refused, so a deliberately
	// tiny database is left at the default rather than failing to start.
	if shared < 16*Megabyte {
		return nil
	}

	return []string{
		"-c", fmt.Sprintf("shared_buffers=%dMB", shared/Megabyte),
		"-c", fmt.Sprintf("effective_cache_size=%dMB", cache/Megabyte),
	}
}

// ParseTemplate reads "name:version" and checks both halves against the
// registry, so an unknown template or an untested version fails at parse time
// rather than as a pull error on the box.
func ParseTemplate(s string) (Template, error) {
	name, version, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || version == "" {
		return Template{}, fmt.Errorf("template %q must be name:version, e.g. postgres:17", s)
	}

	spec, known := Templates[name]
	if !known {
		return Template{}, fmt.Errorf("unknown template %q; known templates: %s", name, strings.Join(templateNames(), ", "))
	}

	var names []string
	for _, v := range spec.Versions {
		if v.Name == version {
			return Template{Name: name, Version: version}, nil
		}
		names = append(names, v.Name)
	}
	return Template{}, fmt.Errorf("template %s has no version %q; available: %s",
		name, version, strings.Join(names, ", "))
}

// Spec returns what this template fixes.
func (t Template) Spec() TemplateSpec { return Templates[t.Name] }

// Image is the concrete image this template runs.
func (t Template) Image() string {
	for _, v := range t.Spec().Versions {
		if v.Name == t.Version {
			return v.Image
		}
	}
	return ""
}

func (t Template) String() string { return t.Name + ":" + t.Version }

func templateNames() []string {
	names := make([]string, 0, len(Templates))
	for n := range Templates {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
