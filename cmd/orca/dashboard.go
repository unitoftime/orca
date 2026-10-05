package main

import (
	"context"
	"fmt"
	"os"

	"github.com/unitoftime/orca/pkg/deploy"
	"github.com/unitoftime/orca/pkg/manifest"
	"golang.org/x/crypto/bcrypt"
)

// The dashboards' password belongs to the cluster, not to a file: bootstrap
// generates one and keeps it in Nomad's variable store, `orca password` reads
// it back, and `orca password set` changes it, the way a secret is set.

// adminPassword is the password the dashboards are behind: the cluster's,
// generated now if it has none, which is what a cluster bootstrapped before
// there was one has. A plan stores nothing, so it makes one up that apply
// would replace.
func adminPassword(ctx context.Context, cluster *Cluster, planOnly bool) (string, error) {
	if planOnly {
		v, ok, err := cluster.AdminPassword(ctx)
		if err != nil || ok {
			return v, err
		}
		return generateSecret(manifest.GeneratedSecretSpec{})
	}
	return ensureAdminPassword(ctx, cluster)
}

// ensureAdminPassword returns the cluster's password, generating it if there
// is none. Only ever creates: a password you have looked up and saved is not
// replaced behind your back.
func ensureAdminPassword(ctx context.Context, cluster *Cluster) (string, error) {
	if v, ok, err := cluster.AdminPassword(ctx); err != nil || ok {
		return v, err
	}
	v, err := generateSecret(manifest.GeneratedSecretSpec{})
	if err != nil {
		return "", err
	}
	created, err := cluster.CreateAdminPassword(ctx, v)
	if err != nil {
		return "", err
	}
	if !created {
		// Made between the read and now; that one is the one to keep.
		v, _, err = cluster.AdminPassword(ctx)
		return v, err
	}
	fmt.Fprintln(os.Stderr, "generated the dashboard password; `orca password` shows it")
	return v, nil
}

// cmdPassword implements `orca password` and `orca password set`.
func cmdPassword(ctx context.Context, cfg Config, args []string) error {
	set := false
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "set":
		set = true
	default:
		return fmt.Errorf("usage: orca password [set]")
	}

	cluster, err := clusterFor(ctx, cfg)
	if err != nil {
		return err
	}
	if set {
		return passwordSet(ctx, cluster)
	}

	password, ok, err := cluster.AdminPassword(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("the cluster has no dashboard password yet; `orca bootstrap` or `orca apply` generates one")
	}
	// Only the password goes to stdout, so it can be piped; who to log in as,
	// and where, go to stderr.
	where := ""
	if cfg.Ingress.Enabled && cfg.Monitoring.Domain != "" && cfg.Monitoring.Status.Enabled {
		scheme := "http"
		if cfg.Ingress.TLS() {
			scheme = "https"
		}
		where = " at " + scheme + "://status." + cfg.Monitoring.Domain
	}
	fmt.Fprintf(os.Stderr, "user admin%s, password:\n", where)
	fmt.Println(password)
	return nil
}

// minPasswordLength is the shortest dashboard password passwordSet accepts,
// and maxPasswordLength the longest: bcrypt, which ingress checks it with,
// refuses anything longer, and every apply after it would fail on that.
const (
	minPasswordLength = 16
	maxPasswordLength = 72
)

// passwordSet replaces the password with one you give it, read the way a
// secret is: prompted, or piped, never an argument.
func passwordSet(ctx context.Context, cluster *Cluster) error {
	value, err := readSecretValue("New dashboard password")
	if err != nil {
		return err
	}
	// One shared password stands between the internet and every log line
	// and metric, and nothing limits how fast it can be guessed.
	if len(value) < minPasswordLength || len(value) > maxPasswordLength {
		return fmt.Errorf("the password must be %d to %d characters; nothing changed", minPasswordLength, maxPasswordLength)
	}
	if err := cluster.PutAdminPassword(ctx, value); err != nil {
		return err
	}
	// Ingress carries a hash of it, made at apply, so that is when it changes.
	fmt.Fprintln(os.Stderr, "set; the dashboards use it from the next `orca apply`")
	return nil
}

// resolveAuthHash returns the bcrypt hash to put in Traefik's basic-auth
// configuration.
//
// bcrypt salts randomly, so hashing the password afresh on every apply would
// change the job spec every time and redeploy ingress forever, breaking the
// guarantee that an unchanged cluster is a no-op. So the hash already deployed
// is reused whenever it still matches the configured password, and a new one is
// generated only when the password actually changed.
//
// The hash is carried in the job's own metadata rather than kept anywhere else,
// which means the cluster remembers it and orca stays stateless. current is
// the cluster's jobs as apply read them, which failed the apply if they could
// not be read, so a cluster that did not answer never passes for one with no
// hash yet and restarts ingress for nothing.
func resolveAuthHash(current map[string]deploy.JobState, password string) (string, error) {
	if password == "" {
		return "", nil
	}

	if existing := current[deploy.JobID(deploy.OrcaApp, "traefik")].AuthHash; existing != "" {
		if bcrypt.CompareHashAndPassword([]byte(existing), []byte(password)) == nil {
			return existing, nil
		}
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash admin password: %w", err)
	}
	return string(hash), nil
}
