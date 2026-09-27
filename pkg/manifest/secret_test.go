package manifest

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// notifier is a small background service with no ports. It reads its secrets
// as files from /secrets, which is the case this feature exists for, so if this
// stops working the feature has lost its reason.
const notifier = `
name: notifier
image: ghcr.io/you/notifier:latest
cpu: 0.1
memory: 32M
secrets:
  - apiToken
  - signingKey
`

func TestBareSecretIsAFile(t *testing.T) {
	m, err := ParseGroup("notifier", []byte(notifier), "notifier.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	s, _ := m.Service("notifier")
	if len(s.Secrets) != 2 {
		t.Fatalf("got %d secrets, want 2", len(s.Secrets))
	}

	for _, want := range []struct{ name, path string }{
		{"apiToken", "/secrets/apiToken"},
		{"signingKey", "/secrets/signingKey"},
	} {
		sec, ok := findSecret(s, want.name)
		if !ok {
			t.Fatalf("secret %q missing", want.name)
		}
		if !sec.IsFile() {
			t.Errorf("secret %q should be delivered as a file by default", want.name)
		}
		if got := sec.MountPath(); got != want.path {
			t.Errorf("secret %q lands at %q, want %q", want.name, got, want.path)
		}
	}

	// Declaring a secret is what makes apply demand it and `orca secret list`
	// report it. Without this the service deploys and crashes on a missing file.
	want := []string{"apiToken", "signingKey"}
	if got := m.Secrets(); !equalStrings(got, want) {
		t.Errorf("Secrets() = %v, want %v", got, want)
	}
}

func TestSecretDeliveryOverrides(t *testing.T) {
	body := `
name: app
image: i:1
secrets:
  - db_password
  - name: session_key
    env: SESSION_KEY
  - name: gh_key
    path: github.pem
`
	m, err := ParseGroup("shop", []byte(body), "app.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, _ := m.Service("app")

	pw, _ := findSecret(s, "db_password")
	if !pw.IsFile() || pw.MountPath() != "/secrets/db_password" {
		t.Errorf("db_password should default to /secrets/db_password, got file=%v path=%q",
			pw.IsFile(), pw.MountPath())
	}

	sk, _ := findSecret(s, "session_key")
	if sk.IsFile() {
		t.Error("session_key asked for an env var and should not be a file")
	}
	if sk.Env != "SESSION_KEY" {
		t.Errorf("session_key env = %q", sk.Env)
	}

	gh, _ := findSecret(s, "gh_key")
	if !gh.IsFile() || gh.MountPath() != "/secrets/github.pem" {
		t.Errorf("gh_key should land at /secrets/github.pem, got %q", gh.MountPath())
	}
	// The name is what you set; the path is only where it lands.
	if gh.Name != "gh_key" {
		t.Errorf("gh_key name = %q, want the secret's name, not its file", gh.Name)
	}
}

// Interpolation is the case `secrets:` deliberately does not replace: a secret
// in the middle of a larger value has nowhere to be a file.
func TestSecretsAndInterpolationCoexist(t *testing.T) {
	body := `
name: app
image: i:1
secrets:
  - api_key
env:
  DATABASE_URL: postgres://postgres:${secret.db_password}@db:5432/postgres
`
	m, err := ParseGroup("shop", []byte(body), "app.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"api_key", "db_password"}
	if got := m.Secrets(); !equalStrings(got, want) {
		t.Errorf("Secrets() = %v, want both the declared and the interpolated: %v", got, want)
	}
}

// orca serializes resolved manifests to hash desired state, so a secret that
// marshalled back differently than it was written would make every plan show a
// change that is not one.
func TestSecretRoundTrip(t *testing.T) {
	body := `name: app
image: i:1
cpu: 0.5
memory: 512M
secrets:
    - bare
    - name: as_env
      env: AS_ENV
    - name: as_file
      path: nested/file.pem
`
	m, err := ParseGroup("shop", []byte(body), "app.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(4)
	if err := enc.Encode(m.Services[0]); err != nil {
		t.Fatalf("encode: %v", err)
	}
	enc.Close()

	again, err := ParseGroup("shop", buf.Bytes(), "app.yaml")
	if err != nil {
		t.Fatalf("re-parse what we wrote: %v\n%s", err, buf.String())
	}

	for i, sec := range m.Services[0].Secrets {
		got := again.Services[0].Secrets[i]
		if got != sec {
			t.Errorf("secret %d round-tripped to %+v, want %+v", i, got, sec)
		}
	}
	// A bare name must stay bare, or the re-encoded form differs from the
	// authored one even though nothing changed.
	if !strings.Contains(buf.String(), "- bare\n") {
		t.Errorf("a bare secret should round-trip as a bare name:\n%s", buf.String())
	}
}

func TestSecretValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			"unnamed secret",
			"{name: x, image: i:1, secrets: [{env: TOKEN}]}",
			"name is required",
		},
		{
			"name with a slash",
			"{name: x, image: i:1, secrets: [a/b]}",
			"may contain only letters, digits, dashes and underscores",
		},
		{
			"the same secret twice",
			"{name: x, image: i:1, secrets: [tok, tok]}",
			"declared twice",
		},
		{
			"both destinations",
			"{name: x, image: i:1, secrets: [{name: tok, env: TOK, path: tok.pem}]}",
			"use one",
		},
		{
			"unknown field in the mapping form",
			"{name: x, image: i:1, secrets: [{name: tok, file: tok.pem}]}",
			`unknown field "file"`,
		},
		{
			"a secret is not a list",
			"{name: x, image: i:1, secrets: {tok: v}}",
			"cannot unmarshal",
		},
		{
			"bad env key",
			"{name: x, image: i:1, secrets: [{name: tok, env: 9LIVES}]}",
			"must start with a letter or underscore",
		},
		{
			// Both would write the same variable and the secret would win
			// silently, so the manifest says two things and one happens.
			"env collides with a plain env value",
			"{name: x, image: i:1, env: {TOKEN: hi}, secrets: [{name: tok, env: TOKEN}]}",
			"already set in env:",
		},
		{
			"two secrets to one env var",
			"{name: x, image: i:1, secrets: [{name: a, env: TOKEN}, {name: b, env: TOKEN}]}",
			`already used by secret "a"`,
		},
		{
			"two secrets to one file",
			"{name: x, image: i:1, secrets: [tok, {name: other, path: tok}]}",
			`already used by secret "tok"`,
		},
		{
			// The path is joined onto a destination on the machine, so this
			// would write a file that runs as root.
			"path escaping the secrets directory",
			"{name: x, image: i:1, secrets: [{name: tok, path: ../../etc/cron.d/x}]}",
			"must stay inside /secrets",
		},
		{
			"absolute path",
			"{name: x, image: i:1, secrets: [{name: tok, path: /etc/passwd}]}",
			"must be relative",
		},
		{
			"unclean path",
			"{name: x, image: i:1, secrets: [{name: tok, path: a//b}]}",
			"plain relative path",
		},
		{
			// orca renders the service's environment to this file; a secret
			// delivered there would overwrite it or be overwritten by it.
			"path taken by orca's env file",
			"{name: x, image: i:1, secrets: [{name: tok, path: orca.env}]}",
			"where orca writes the service's environment",
		},
		{
			"path naming no file",
			"{name: x, image: i:1, secrets: [{name: tok, path: .}]}",
			"must name a file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseGroup("app", []byte(tt.body), "app.yaml")
			if err == nil {
				t.Fatalf("expected an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// A nested path is allowed; only escaping upward is not.
func TestSecretNestedPathAllowed(t *testing.T) {
	m, err := ParseGroup("app", []byte("{name: x, image: i:1, secrets: [{name: tok, path: tls/key.pem}]}"), "app.yaml")
	if err != nil {
		t.Fatalf("a nested path should be allowed: %v", err)
	}
	s, _ := m.Service("x")
	if got := s.Secrets[0].MountPath(); got != "/secrets/tls/key.pem" {
		t.Errorf("mount path = %q", got)
	}
}

func findSecret(s *Service, name string) (Secret, bool) {
	for _, sec := range s.Secrets {
		if sec.Name == name {
			return sec, true
		}
	}
	return Secret{}, false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// orca files its own jobs under this group, so a directory of the same name
// would put your services in the same namespace, and a service called traefik
// or dns would collide with the real one outright.
func TestReservedGroupIsRefused(t *testing.T) {
	_, err := ParseGroup(ReservedGroup, []byte("{name: web, image: i:1}"), "web.yaml")
	if err == nil {
		t.Fatal("a group named orca should be refused")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("the error should say the name is reserved, got %v", err)
	}
}
