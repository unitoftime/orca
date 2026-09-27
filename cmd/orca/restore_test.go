package main

import (
	"strings"
	"testing"

	"github.com/unitoftime/orca/pkg/deploy"
)

func hostileSpec() deploy.BackupSpec {
	return deploy.BackupSpec{
		Endpoint:        `https://e$(id)`,
		Bucket:          `b$(id)`,
		Region:          "auto",
		Image:           "rclone/rclone:1.71.0",
		SecretGroup:     "storage",
		KeyIDSecret:     "offsite_key_id",
		SecretKeySecret: "offsite_secret_key",
	}
}

// A backup's filename comes from listing the bucket, so it is remote input,
// and the restore script must quote it. Inside a double-quoted shell string
// $(...) still runs, so writing a file into the backup bucket would be
// command execution as root on the machine, taken at the moment someone runs
// a restore, when things are already going badly.
func TestRestoreScriptQuotesRemoteInput(t *testing.T) {
	evil := `x$(touch /tmp/pwned).pgc`
	script := restoreScript(hostileSpec(), "shop", "db", "shop/db", evil, "postgres:17-alpine")

	// Inside single quotes the shell expands nothing at all, so the payload
	// may appear only there.
	if !strings.Contains(script, `NAME='x$(touch /tmp/pwned).pgc'`) {
		t.Errorf("the name should be a single-quoted assignment, got:\n%s", script)
	}
	if !strings.Contains(script, `BUCKET='b$(id)'`) {
		t.Errorf("the bucket should be a single-quoted assignment, got:\n%s", script)
	}
	// And nowhere else: every use has to go through the variable.
	if strings.Contains(script, `"store:b$(id)`) || strings.Contains(script, `/work/x$(`) {
		t.Errorf("remote input reached a double-quoted string:\n%s", script)
	}
	if n := strings.Count(script, "$(touch /tmp/pwned)"); n != 1 {
		t.Errorf("the payload appears %d times; it should appear once, inside its quoted assignment:\n%s", n, script)
	}
}

// The endpoint and region are manifest-controlled and reach the same shell.
func TestRcloneEnvQuotesTheEndpoint(t *testing.T) {
	script := rcloneEnvScript(hostileSpec())
	if !strings.Contains(script, `S3_ENDPOINT='https://e$(id)'`) {
		t.Errorf("the endpoint should be a single-quoted assignment, got:\n%s", script)
	}
	if strings.Contains(script, `ENDPOINT=https://e$(id)`) {
		t.Errorf("the endpoint reached the script unquoted:\n%s", script)
	}
}

// A value holding a single quote must not be able to close the quoting and
// escape into the script.
func TestShQuoteEscapesEmbeddedQuotes(t *testing.T) {
	got := shQuote(`a'; touch /tmp/pwned; #`)
	if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
		t.Fatalf("not a quoted word: %s", got)
	}
	// The embedded quote has to become the close-escape-reopen dance, or the
	// rest of the value is live shell.
	if !strings.Contains(got, `'\''`) {
		t.Errorf("embedded quote not escaped: %s", got)
	}
}

// The credential error names the group the credentials actually live in.
// Any other group would send whoever reads it to a path that does not exist.
func TestCredentialErrorNamesTheRealSecretPath(t *testing.T) {
	script := rcloneEnvScript(hostileSpec())
	if !strings.Contains(script, "orca secret set storage/offsite_key_id") {
		t.Errorf("should name <target group>/<secret>, got:\n%s", script)
	}
	if strings.Contains(script, "platform/") {
		t.Errorf("still points at the group that no longer exists:\n%s", script)
	}
}

func TestCheckBackupName(t *testing.T) {
	if err := checkBackupName("shop-db-20260925T120000Z.pgc", "pgc"); err != nil {
		t.Errorf("a name orca wrote should be accepted: %v", err)
	}
	for _, bad := range []string{
		`x$(touch /tmp/pwned).pgc`,
		`shop-db-20260925T120000Z.pgc; rm -rf /`,
		`../../etc/passwd`,
		`shop-db.pgc`,
		``,
	} {
		if err := checkBackupName(bad, "pgc"); err == nil {
			t.Errorf("checkBackupName(%q) should have been refused", bad)
		}
	}

	// The right shape and the wrong kind: a Postgres dump handed to a Redis
	// restore would stop the service to load something it cannot.
	if err := checkBackupName("shop-db-20260925T120000Z.pgc", "rdb"); err == nil {
		t.Error("a pgc dump should be refused where an rdb is expected")
	}
	if err := checkBackupName("blog-redis-20260925T120000Z.rdb", "rdb"); err != nil {
		t.Errorf("an rdb snapshot orca wrote should be accepted: %v", err)
	}
}

// The Redis restore interpolates the same remote input, so it gets the same
// check: the payload may appear only inside single quotes.
func TestRedisRestoreScriptQuotesRemoteInput(t *testing.T) {
	evil := `x$(touch /tmp/pwned).rdb`
	script := redisRestoreScript(hostileSpec(), "blog", "redis", "blog/redis", evil, "redis:8.10-alpine")

	if !strings.Contains(script, `NAME='x$(touch /tmp/pwned).rdb'`) {
		t.Errorf("the name should be a single-quoted assignment, got:\n%s", script)
	}
	if strings.Count(script, "x$(touch") != 1 {
		t.Errorf("the name should be used only through its variable, got:\n%s", script)
	}
}

// Redis with appendonly on ignores dump.rdb and starts empty when it finds no
// append-only file (checked against redis 8.10 rather than assumed). So the
// restore has to load the snapshot with the AOF off and switch it on, and the
// order of those steps is the restore.
func TestRedisRestoreLoadsWithoutAOFThenWritesIt(t *testing.T) {
	script := redisRestoreScript(hostileSpec(), "blog", "redis", "blog/redis", "blog-redis-20260925T120000Z.rdb", "redis:8.10-alpine")

	steps := []string{
		"nomad job inspect",
		"nomad job stop",
		`mv "$DIR" "$KEEP"`,
		"--appendonly no",
		"CONFIG SET appendonly yes",
		"aof_rewrite_in_progress:0",
		"redis-cli SHUTDOWN",
		"nomad job run",
	}
	at := 0
	for _, step := range steps {
		i := strings.Index(script[at:], step)
		if i < 0 {
			t.Fatalf("step %q missing or out of order in:\n%s", step, script)
		}
		at += i
	}

	// The data it replaced is kept, outside the volume root, and a failure
	// once the service is down puts it back.
	if !strings.Contains(script, "KEEP='/var/orca/pre-restore/blog-redis'-$STAMP") {
		t.Errorf("previous data should be kept under pre-restore, got:\n%s", script)
	}
	if !strings.Contains(script, `mv "$KEEP" "$DIR"`) {
		t.Error("a failed restore should put the previous data back")
	}
	if !strings.Contains(script, "DIR='/var/orca/volumes/services/blog/redis'") {
		t.Errorf("should swap the service's own volume, got:\n%s", script)
	}
}
