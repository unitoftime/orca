package main

import (
	"strings"
	"testing"

	"github.com/unitoftime/orca/internal/deploy"
	"github.com/unitoftime/orca/internal/manifest"
)

func TestParseSecretRef(t *testing.T) {
	group, name, err := parseSecretRef("blog/session_key")
	if err != nil || group != "blog" || name != "session_key" {
		t.Errorf("got %q/%q, %v", group, name, err)
	}

	for _, bad := range []string{"", "blog", "/name", "blog/", "blog/a/b"} {
		if _, _, err := parseSecretRef(bad); err == nil {
			t.Errorf("parseSecretRef(%q) should be an error", bad)
		}
	}
}

// One variable per secret, so listing which secrets exist is a listing of
// paths and never returns a value.
func TestSecretPathIsPerSecret(t *testing.T) {
	a := deploy.SecretPath("blog", "session_key")
	b := deploy.SecretPath("blog", "other")
	if a == b {
		t.Fatal("two secrets in one group must not share a path")
	}
	if !strings.HasPrefix(a, deploy.SecretPrefix+"/") {
		t.Errorf("path %q should live under orca's prefix", a)
	}
}

func groupWith(t *testing.T, name, body string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.ParseGroup(name, []byte(body), name+"/svc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A referenced secret with no value makes Nomad block the task, so the failure
// would otherwise arrive as a health timeout naming a raft path. This check is
// what turns it into a message naming the secret, before anything is submitted.
func TestCheckSecretsNamesWhatIsMissing(t *testing.T) {
	groups := []*manifest.Manifest{
		groupWith(t, "blog", "{name: api, image: i:1, env: {A: \"${secret.token}\", B: \"${secret.other}\"}}"),
	}

	err := checkMissing(neededSecrets(groups, nil), map[string]bool{
		deploy.SecretPath("blog", "token"): true,
	})
	if err == nil {
		t.Fatal("expected an error for the unset secret")
	}
	if !strings.Contains(err.Error(), "blog/other") {
		t.Errorf("error should name the missing secret, got: %v", err)
	}
	if strings.Contains(err.Error(), "blog/token") {
		t.Errorf("error should not name the secret that is set, got: %v", err)
	}
	if !strings.Contains(err.Error(), "orca secret set") {
		t.Errorf("error should say how to fix it, got: %v", err)
	}
}

func TestCheckSecretsPassesWhenAllSet(t *testing.T) {
	groups := []*manifest.Manifest{
		groupWith(t, "blog", "{name: api, image: i:1, env: {A: \"${secret.token}\"}}"),
	}
	if err := checkMissing(neededSecrets(groups, nil), map[string]bool{deploy.SecretPath("blog", "token"): true}); err != nil {
		t.Errorf("all secrets set should pass, got: %v", err)
	}
}

// A group that references nothing must not need the store consulted at all.
func TestCheckSecretsIgnoresGroupsWithout(t *testing.T) {
	groups := []*manifest.Manifest{groupWith(t, "bot", "{name: bot, image: i:1}")}
	if err := checkMissing(neededSecrets(groups, nil), nil); err != nil {
		t.Errorf("a group with no secrets should pass, got: %v", err)
	}
}

// A backup target's credentials live in the target's group, which a scoped
// apply of the database's group does not otherwise look at. They must still
// be required, or a backup job with no way to authenticate is deployed and
// fails where nobody is watching.
func TestPreflightCoversBackupTargetsInOtherGroups(t *testing.T) {
	shop := groupWith(t, "shop", "{name: db, template: postgres:17, volume: 1G, backup: {to: storage/offsite}}")
	storage := groupWith(t, "storage", "{name: offsite, target: s3, endpoint: https://x, bucket: b}")

	backups, err := collectBackups([]*manifest.Manifest{shop, storage}, []*manifest.Manifest{shop})
	if err != nil {
		t.Fatal(err)
	}
	_, err = missingGenerated(neededSecrets([]*manifest.Manifest{shop}, backups), generatedSecrets([]*manifest.Manifest{shop}), nil)
	if err == nil || !strings.Contains(err.Error(), "storage/offsite_key_id") {
		t.Fatalf("an apply of shop alone must still require the target's credentials, got %v", err)
	}
}

// A generated secret that does not exist yet is one apply will create, so it
// is not missing; plan reports it rather than creating it.
func TestGeneratedSecretsCountAsSetButAreNotCreatedByTheCheck(t *testing.T) {
	shop := groupWith(t, "shop", "{name: db, template: postgres:17, volume: 1G}\n---\n{name: app, image: i:1, env: {URL: \"${secret.db_password}\"}}")
	groups := []*manifest.Manifest{shop}

	toGenerate, err := missingGenerated(neededSecrets(groups, nil), generatedSecrets(groups), map[string]bool{})
	if err != nil {
		t.Fatalf("a secret about to be generated is not missing, got %v", err)
	}
	if len(toGenerate) != 1 || toGenerate[0].String() != "shop/db_password" {
		t.Errorf("want shop/db_password to generate, got %v", toGenerate)
	}

	set := map[string]bool{deploy.SecretPath("shop", "db_password"): true}
	if toGenerate, _ := missingGenerated(nil, generatedSecrets(groups), set); len(toGenerate) != 0 {
		t.Errorf("an existing generated secret is never regenerated, got %v", toGenerate)
	}
}

// A PEM key piped in is stored whole, not as its first line.
func TestPipedSecretIsReadWhole(t *testing.T) {
	pem := "-----BEGIN KEY-----\nabc\ndef\n-----END KEY-----\n"
	got, err := readPipedSecret(strings.NewReader(pem))
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.TrimSuffix(pem, "\n") {
		t.Errorf("got %q", got)
	}
	if got, _ := readPipedSecret(strings.NewReader("token\r\n")); got != "token" {
		t.Errorf("the newline echo adds is not part of the value, got %q", got)
	}
}

// Setting or removing a generated password locks the database out of the
// data it was initialised with, so it takes --force.
func TestGeneratedSecretsAreGuarded(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cluster.yaml": "nodes:\n  - host: root@10.0.0.1\n",
		"shop/db.yaml": "{name: db, template: postgres:17, volume: 1G}",
	})
	cfg, err := loadConfigFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := guardGenerated(cfg, []secretRef{{"shop", "db_password"}}, "remove it", false); err == nil {
		t.Error("removing a generated secret must be refused")
	}
	if err := guardGenerated(cfg, []secretRef{{"shop", "db_password"}}, "remove it", true); err != nil {
		t.Errorf("--force overrides, got %v", err)
	}
	if err := guardGenerated(cfg, []secretRef{{"shop", "api_token"}}, "change it", false); err != nil {
		t.Errorf("a secret you set yourself is yours to change, got %v", err)
	}
}
