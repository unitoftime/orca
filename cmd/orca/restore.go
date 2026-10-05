package main

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/unitoftime/orca/internal/deploy"
	"github.com/unitoftime/orca/internal/manifest"
)

// hostNomad starts every script that asks Nomad something from on the
// machine, where the work is next to the data and its secrets need not
// leave: a restore, mostly. It reads the cluster's token for Nomad's own CLI,
// and defines nomad_get, which reads one API path.
//
// The token goes to curl on its stdin, not in its arguments: an argument list
// is visible in the process table for as long as the command runs.
const hostNomad = `NOMAD_TOKEN=$(cat ` + tokenPath + `)
export NOMAD_TOKEN
nomad_get() { printf 'X-Nomad-Token: %s\n' "$NOMAD_TOKEN" | curl -sf --max-time 10 -H @- "` + nomadAddr + `$1"; }
`

// rcloneEnvScript writes the S3 credentials to a private file on the machine
// and prints its path.
//
// A file rather than command arguments, for the reason the token is not one.
// The file is created with a restrictive mode and removed by the caller.
func rcloneEnvScript(spec deploy.BackupSpec) string {
	return hostNomad + fmt.Sprintf(`ENVFILE=$(mktemp)
chmod 600 "$ENVFILE"
S3_ENDPOINT=%[4]s
S3_REGION=%[5]s
KEY=$(nomad_get /v1/var/%[1]s | jq -r '.Items.value')
SEC=$(nomad_get /v1/var/%[2]s | jq -r '.Items.value')
if [ -z "$KEY" ] || [ "$KEY" = "null" ] || [ -z "$SEC" ] || [ "$SEC" = "null" ]; then
  echo "backup credentials are not set; run: orca secret set %[3]s" >&2
  rm -f "$ENVFILE"; exit 1
fi
{
  echo "RCLONE_CONFIG_STORE_TYPE=s3"
  echo "RCLONE_CONFIG_STORE_PROVIDER=Other"
  echo "RCLONE_CONFIG_STORE_ENDPOINT=$S3_ENDPOINT"
  echo "RCLONE_CONFIG_STORE_REGION=$S3_REGION"
  echo "RCLONE_CONFIG_STORE_NO_CHECK_BUCKET=true"
  echo "RCLONE_CONFIG_STORE_ACCESS_KEY_ID=$KEY"
  echo "RCLONE_CONFIG_STORE_SECRET_ACCESS_KEY=$SEC"
} > "$ENVFILE"`,
		deploy.SecretPath(spec.SecretGroup, spec.KeyIDSecret),
		deploy.SecretPath(spec.SecretGroup, spec.SecretKeySecret),
		// The group the credentials actually live in, so the message sends
		// whoever reads it to a path that exists.
		spec.SecretGroup+"/"+spec.KeyIDSecret,
		shQuote(spec.Endpoint), shQuote(spec.Region))
}

// ListBackups returns the backups held for one database, oldest first.
func (n Node) ListBackups(ctx context.Context, spec deploy.BackupSpec) ([]string, error) {
	// Every non-zero exit is an error, including 3.
	//
	// Checked against rclone rather than assumed: listing an empty prefix in a
	// bucket that exists exits 0 with no output, a missing bucket exits 3, and
	// bad credentials exit 1. So "no backups yet" is exactly the empty exit-0
	// case and nothing else. Treating 3 as empty too would answer "no backups
	// yet for shop/db" to someone whose bucket name is wrong: the
	// reassuring-but-false answer this command exists to avoid giving.
	//
	// Only what rclone prints as the listing is the listing. Everything said
	// on the way to it is kept apart and shown only when it failed: Docker
	// pulling the image the first time, and rclone's own notices, would
	// otherwise each be read as the name of a backup.
	script := fmt.Sprintf(`set -eu
%[1]s
BUCKET=%[3]s
PREFIX=%[4]s
ERR=$(mktemp)
set +e
OUT=$(docker run --rm --network host --env-file "$ENVFILE" %[2]s lsf "store:$BUCKET/$PREFIX/" 2>"$ERR")
CODE=$?
set -e
rm -f "$ENVFILE"
if [ $CODE -ne 0 ]; then
  cat "$ERR" >&2
  rm -f "$ERR"
  exit $CODE
fi
rm -f "$ERR"
printf '%%s\n' "$OUT"`,
		rcloneEnvScript(spec), shQuote(spec.Image), shQuote(spec.Bucket), shQuote(spec.Prefix))

	out, err := n.RunOutput(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("list backups in %s/%s: %w", spec.Bucket, spec.Prefix, err)
	}

	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if n := strings.TrimSpace(line); n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// RestoreBackup downloads a backup and loads it into the running database.
//
// It runs where the database is and reaches it the same way anything else
// does, so a restore exercises the same path a normal connection takes rather
// than a special one that only works when someone is watching.
func (n Node) RestoreBackup(ctx context.Context, spec deploy.BackupSpec, group, service, name, pgImage string) error {
	script := restoreScript(spec, group, service, name, pgImage)

	// Streamed, not buffered: a multi-gigabyte download followed by a restore
	// prints nothing for minutes otherwise, and a working restore is
	// indistinguishable from a hang.
	if err := n.Run(ctx, script); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	fmt.Printf("restored %s into %s/%s\n", name, group, service)
	return nil
}

// restoreScript builds the restore, separately from running it.
//
// Split out so the most dangerous string orca produces can be asserted on
// without a machine: it interpolates a filename that came from listing a
// bucket, which is the one input here that someone else can choose.
func restoreScript(spec deploy.BackupSpec, group, service, name, pgImage string) string {
	return fmt.Sprintf(`set -eu
%[1]s
BUCKET=%[3]s
PREFIX=%[4]s
NAME=%[6]s

WORK=$(mktemp -d)
PGENV=$(mktemp)
chmod 600 "$PGENV"
trap 'rm -rf "$WORK" "$ENVFILE" "$PGENV"' EXIT
chmod 755 "$WORK"

echo "downloading $NAME"
docker run --rm --network host --env-file "$ENVFILE" -v "$WORK:/work" %[2]s \
  copyto "store:$BUCKET/$PREFIX/$NAME" "/work/$NAME"

# Host and port both come from the catalog. Assuming 5432 is right only while
# the database registers its own address; once a port is published instead,
# the catalog carries a dynamic one and the assumption silently connects to
# nothing.
SVC=$(nomad_get /v1/service/%[8]s)
DBHOST=$(printf '%%s' "$SVC" | jq -r '.[0].Address // empty')
DBPORT=$(printf '%%s' "$SVC" | jq -r '.[0].Port // empty')
if [ -z "$DBHOST" ] || [ -z "$DBPORT" ]; then
  echo "database %[8]s is not registered; is it running?" >&2
  exit 1
fi

# The connection goes in a file, not the argument list, which is visible in the
# host's process table for as long as the restore runs.
{
  printf 'PGPASSWORD=%%s\n' "$(nomad_get /v1/var/%[7]s | jq -r '.Items.value')"
  printf 'PGHOST=%%s\nPGPORT=%%s\nPGUSER=postgres\n' "$DBHOST" "$DBPORT"
} > "$PGENV"

cat > "$WORK/restore.sh" <<'RESTORE'
%[9]s
RESTORE

echo "restoring into $DBHOST:$DBPORT"
docker run --rm --network host -v "$WORK:/work" --env-file "$PGENV" \
  -e NAME="$NAME" -e STAMP="$(date -u +%%Y%%m%%dT%%H%%M%%SZ)" %[5]s sh /work/restore.sh`,
		rcloneEnvScript(spec), shQuote(spec.Image), shQuote(spec.Bucket), shQuote(spec.Prefix),
		shQuote(pgImage), shQuote(name),
		deploy.SecretPath(group, manifest.GeneratedSecret(service, manifest.PasswordSuffix)),
		deploy.CatalogName(group, service),
		pgRestoreScript)
}

// pgRestoreScript runs inside the database's own image, next to the
// downloaded backup in /work, and loads it one database at a time.
//
// Each is restored into a new database first, and only once that has
// succeeded does it take the real one's name, the real one being renamed to
// <name>_before_restore_<time> and kept on the server. So a restore that fails
// part way leaves every database as it was, one that succeeds leaves each
// exactly as the backup had it (not the backup laid over whatever was there),
// and what it replaced is one rename away.
//
// Database names come from the files in the backup, so they are only ever
// handed to psql as variables, which quotes them, never pasted into SQL.
const pgRestoreScript = `set -eu
cd /work
SUFFIX=` + deploy.PreRestoreSuffix + `
# Postgres narrates every IF EXISTS that did not; only warnings are news here.
export PGOPTIONS='-c client_min_messages=warning'

sql() { psql -X -q -v ON_ERROR_STOP=1 -d template1 "$@"; }

case "$NAME" in
  *.tar)
    mkdir x
    tar -xf "$NAME" -C x
    # Roles first, so owners and grants have someone to belong to. One that
    # exists already is only reported, and keeps what it has.
    if [ -f x/globals.sql ]; then
      psql -X -q -d template1 -f x/globals.sql 2>&1 | grep -v 'already exists' || true
    fi
    OPTS=""
    ;;
  *)
    # A single dump of the postgres database, from before backups carried
    # roles: restored without owners and grants, which may name roles this
    # server does not have.
    mkdir x
    mv "$NAME" x/postgres.pgc
    OPTS="--no-owner --no-privileges"
    ;;
esac

# Every database is loaded before any is swapped in, so a backup that fails
# to load leaves the server as it was rather than half restored. Each loads
# into a new database created from template0, as pg_restore expects, which
# also works while this script is connected to template1.
scratch() { echo "orca_restore_$1_$STAMP"; }
N=0
discard() {
  i=1
  while [ "$i" -le "$N" ]; do
    printf 'DROP DATABASE IF EXISTS :"new";\n' | sql -v new="$(scratch "$i")" || true
    i=$((i + 1))
  done
}

for f in x/*.pgc; do
  [ -f "$f" ] || { echo "$NAME holds no databases" >&2; exit 1; }
  N=$((N + 1))
  NEW=$(scratch "$N")
  echo "loading $(basename "$f" .pgc)"
  printf 'DROP DATABASE IF EXISTS :"new";\nCREATE DATABASE :"new" TEMPLATE template0;\n' | sql -v new="$NEW"
  if ! pg_restore --single-transaction $OPTS -d "$NEW" "$f"; then
    discard
    echo "loading $(basename "$f" .pgc) failed; nothing was changed" >&2
    exit 1
  fi
done

N=0
for f in x/*.pgc; do
  N=$((N + 1))
  NEW=$(scratch "$N")
  DB=$(basename "$f" .pgc)
  # Postgres cuts a name at 63 bytes, so the database's own name is what gives
  # way: cutting the suffix could make two databases' copies one name.
  OLD="$(printf '%.30s' "$DB")$SUFFIX$STAMP"

  # The swap. New connections are refused and open ones ended first, or the
  # rename is refused; the applications reconnect to the restored database.
  # Both renames are one transaction, so the name never points at nothing.
  if ! sql -v db="$DB" -v old="$OLD" -v new="$NEW" <<'SQL'
SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = :'db') AS exists \gset
\if :exists
  ALTER DATABASE :"db" WITH ALLOW_CONNECTIONS false;
  SELECT count(pg_terminate_backend(pid, 10000)) FROM pg_stat_activity WHERE datname = :'db' \g /dev/null
\endif
BEGIN;
\if :exists
  ALTER DATABASE :"db" RENAME TO :"old";
\endif
ALTER DATABASE :"new" RENAME TO :"db";
COMMIT;
\if :exists
  \echo '  the database it replaced is kept, closed to connections, as' :old
\endif
SQL
  then
    printf 'ALTER DATABASE :"db" WITH ALLOW_CONNECTIONS true;\n' | sql -v db="$DB" >/dev/null 2>&1 || true
    discard
    echo "swapping in the restored $DB failed; it is as it was, and so is every database after it" >&2
    exit 1
  fi
  echo "restored $DB"
done`

// RestoreRedis replaces a Redis service's data with a backup.
//
// Unlike Postgres there is no loading a dump into a running server, so this
// is the one restore with downtime: the service is stopped, its data swapped,
// and started again. It runs on the machine that holds the volume, which is
// the one place the data can be swapped.
func (n Node) RestoreRedis(ctx context.Context, spec deploy.BackupSpec, group, service, name, image string) error {
	if err := n.Run(ctx, redisRestoreScript(spec, group, service, name, image)); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	fmt.Printf("restored %s into %s/%s\n", name, group, service)
	return nil
}

// preRestorePrefix is where a restore leaves the data it replaced, suffixed
// with the time of the restore. Outside the volume root, so it is never
// mistaken for a volume of its own. It is kept rather than deleted, because
// deleting data is purge's job and nothing else's.
func preRestorePrefix(group, service string) string {
	return path.Join(dataDir, "pre-restore", group+"-"+service)
}

// redisRestoreScript builds the Redis restore, separately from running it, for
// the reason restoreScript is: the backup name is remote input.
//
// A snapshot cannot simply be dropped into the volume. With appendonly on,
// Redis loads the append-only file and ignores dump.rdb; finding none, it
// starts empty and writes an empty one, so the restore would appear to work
// and hold nothing. So the snapshot is loaded by a throwaway server with the
// AOF off, which is then switched on: that rewrites the AOF from the loaded
// data, and the service starts from it as it always does.
//
// If anything fails once the service is down, the previous data is put back
// and the service started again, so a failed restore costs a restart rather
// than the database.
func redisRestoreScript(spec deploy.BackupSpec, group, service, name, image string) string {
	return fmt.Sprintf(`set -eu
%[1]s
BUCKET=%[3]s
PREFIX=%[4]s
NAME=%[6]s
IMAGE=%[5]s
JOB=%[7]s
DIR=%[8]s
STAMP=$(date -u +%%Y%%m%%dT%%H%%M%%SZ)
KEEP=%[9]s-$STAMP
TMP=orca-restore-$JOB

WORK=$(mktemp -d)
SPEC=$(mktemp)
STOPPED=0
SWAPPED=0
DONE=0

cleanup() {
  docker rm -f "$TMP" >/dev/null 2>&1 || true
  if [ "$DONE" = 0 ] && [ "$SWAPPED" = 1 ]; then
    echo "restore failed; putting the previous data back" >&2
    rm -rf "$DIR" && mv "$KEEP" "$DIR"
  fi
  if [ "$DONE" = 0 ] && [ "$STOPPED" = 1 ]; then
    echo "starting $JOB again" >&2
    nomad job run -detach -json "$SPEC" >/dev/null || echo "could not start $JOB; run orca apply" >&2
  fi
  rm -rf "$WORK" "$ENVFILE" "$SPEC"
}
trap cleanup EXIT
chmod 755 "$WORK"

echo "downloading $NAME"
docker run --rm --network host --env-file "$ENVFILE" -v "$WORK:/work" %[2]s \
  copyto "store:$BUCKET/$PREFIX/$NAME" "/work/dump.rdb"

# The job exactly as it runs now, to start it again as it was.
nomad job inspect "$JOB" > "$SPEC"

echo "stopping $JOB"
STOPPED=1
nomad job stop -detach "$JOB" >/dev/null
for i in $(seq 1 60); do
  LIVE=$(nomad_get "/v1/job/$JOB/allocations" | jq '[.[] | select(.ClientStatus == "running" or .ClientStatus == "pending")] | length')
  [ "$LIVE" = 0 ] && break
  sleep 2
done
[ "$LIVE" = 0 ] || { echo "$JOB did not stop" >&2; exit 1; }

mkdir -p "$(dirname "$KEEP")"
mv "$DIR" "$KEEP"
SWAPPED=1
mkdir "$DIR"
chown --reference="$KEEP" "$DIR"
cp "$WORK/dump.rdb" "$DIR/dump.rdb"

# No network: nothing but this script can reach it, so it needs no password.
echo "loading $NAME"
docker run -d --name "$TMP" --network none -v "$DIR:/data" "$IMAGE" \
  redis-server --appendonly no --dir /data >/dev/null
until [ "$(docker exec "$TMP" redis-cli PING 2>/dev/null)" = PONG ]; do
  docker inspect -f '{{.State.Running}}' "$TMP" | grep -q true || { docker logs "$TMP" >&2; exit 1; }
  sleep 2
done
KEYS=$(docker exec "$TMP" redis-cli DBSIZE)
echo "loaded $KEYS keys"

docker exec "$TMP" redis-cli CONFIG SET appendonly yes >/dev/null
for i in $(seq 1 300); do
  P=$(docker exec "$TMP" redis-cli INFO persistence | tr -d '\r')
  if printf '%%s\n' "$P" | grep -qx 'aof_rewrite_in_progress:0' &&
     printf '%%s\n' "$P" | grep -qx 'aof_rewrite_scheduled:0' &&
     ls "$DIR"/appendonlydir/*.manifest >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
printf '%%s\n' "$P" | grep -qx 'aof_last_bgrewrite_status:ok' || { echo "writing the append-only file failed" >&2; exit 1; }
docker exec "$TMP" redis-cli SHUTDOWN >/dev/null 2>&1 || true
docker wait "$TMP" >/dev/null
docker rm "$TMP" >/dev/null

echo "starting $JOB"
nomad job run -detach -json "$SPEC" >/dev/null
DONE=1
echo "restored $NAME ($KEYS keys); the data it replaced is at $KEEP"`,
		rcloneEnvScript(spec), shQuote(spec.Image), shQuote(spec.Bucket), shQuote(spec.Prefix),
		shQuote(image), shQuote(name),
		shQuote(deploy.JobID(group, service)),
		shQuote(deploy.VolumePath(dataDir, group, service)),
		shQuote(preRestorePrefix(group, service)))
}
