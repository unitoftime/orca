package deploy

import (
	"strings"
	"testing"

	nomad "github.com/hashicorp/nomad/api"
)

// findTemplate returns the template writing to dest.
func findTemplate(t *testing.T, task *nomad.Task, dest string) *nomad.Template {
	t.Helper()
	for _, tm := range task.Templates {
		if tm.DestPath != nil && *tm.DestPath == dest {
			return tm
		}
	}
	var got []string
	for _, tm := range task.Templates {
		got = append(got, *tm.DestPath)
	}
	t.Fatalf("no template writing to %q; got %v", dest, got)
	return nil
}

// The whole point of the feature: a service that reads /secrets/apiToken gets
// that file, with no environment variable anywhere and no change to the image.
func TestDeclaredSecretsBecomeFiles(t *testing.T) {
	job := buildOneIn(t, "notifier", `
name: notifier
image: ghcr.io/you/notifier:latest
cpu: 0.1
memory: 32M
secrets:
  - apiToken
  - signingKey
`, "notifier", defaultOpts())

	task := job.TaskGroups[0].Tasks[0]

	// No env template at all: this service needs nothing in its environment,
	// so it should not get an orca.env it would never read.
	for _, tm := range task.Templates {
		if *tm.DestPath == "secrets/orca.env" {
			t.Error("a service with only file secrets should get no env template")
		}
	}
	if len(task.Templates) != 2 {
		t.Fatalf("want two templates, one per secret, got %d", len(task.Templates))
	}

	tm := findTemplate(t, task, "secrets/apiToken")
	body := *tm.EmbeddedTmpl
	if !strings.Contains(body, `nomadVar "orca/notifier/apiToken"`) {
		t.Errorf("should read the secret's own variable path, got:\n%s", body)
	}
	// The file has to *be* the secret. A trailing newline or a KEY=value
	// wrapper is a value the application then reads wrong.
	if strings.Contains(body, "=") || strings.Contains(body, "\n") {
		t.Errorf("the file should hold the bare value, got:\n%q", body)
	}
	if tm.Envvars != nil && *tm.Envvars {
		t.Error("a secret file must not also be parsed into the environment")
	}
	// distroless images run as nonroot and the postgres image as postgres, so
	// a mode only root can read fails at startup on a guess orca cannot make.
	if tm.Perms == nil || *tm.Perms != "0444" {
		t.Errorf("perms = %v, want 0444 so a non-root container can read it", tm.Perms)
	}
	if tm.ChangeMode == nil || *tm.ChangeMode != "restart" {
		t.Errorf("change mode = %v, want restart so a rotated secret takes effect", tm.ChangeMode)
	}

	// Nothing secret in the jobspec itself, which anyone who can read the
	// cluster can read back.
	if len(task.Env) != 0 {
		t.Errorf("no env expected, got %v", task.Env)
	}
}

func TestSecretEnvOverrideStaysInTheEnvTemplate(t *testing.T) {
	job := buildOneIn(t, "shop", `
name: app
image: i:1
secrets:
  - db_password
  - name: session_key
    env: SESSION_KEY
`, "app", defaultOpts())

	task := job.TaskGroups[0].Tasks[0]

	env := findTemplate(t, task, "secrets/orca.env")
	got := renderEnv(t, *env.EmbeddedTmpl, map[string]string{"orca/shop/session_key": "k"})
	if got["SESSION_KEY"] != "k" {
		t.Errorf("session_key should render as an env var, got:\n%s", *env.EmbeddedTmpl)
	}
	if strings.Contains(*env.EmbeddedTmpl, "db_password") {
		t.Errorf("a file secret must not also reach the environment, got:\n%s", *env.EmbeddedTmpl)
	}

	// And the file one is still a file.
	findTemplate(t, task, "secrets/db_password")

	// A secret delivered as an env var needs no file.
	for _, tm := range task.Templates {
		if *tm.DestPath == "secrets/session_key" {
			t.Error("an env-delivered secret should not also be written as a file")
		}
	}
}

func TestSecretPathOverridesTheFileName(t *testing.T) {
	job := buildOneIn(t, "shop", `
name: app
image: i:1
secrets:
  - name: gh_key
    path: tls/github.pem
`, "app", defaultOpts())

	task := job.TaskGroups[0].Tasks[0]
	tm := findTemplate(t, task, "secrets/tls/github.pem")
	// The name is what was set in the store; the path is only where it lands.
	if !strings.Contains(*tm.EmbeddedTmpl, `nomadVar "orca/shop/gh_key"`) {
		t.Errorf("should read the secret's name from the store, got:\n%s", *tm.EmbeddedTmpl)
	}
}

// Nomad redeploys a job whose spec changed, so template ordering that follows
// the manifest's ordering would redeploy every service whose secrets were
// merely reordered.
func TestSecretFileOrderIsStable(t *testing.T) {
	forward := buildOneIn(t, "shop", `
name: app
image: i:1
secrets: [alpha, beta, gamma]
`, "app", defaultOpts())

	reversed := buildOneIn(t, "shop", `
name: app
image: i:1
secrets: [gamma, beta, alpha]
`, "app", defaultOpts())

	a := forward.TaskGroups[0].Tasks[0].Templates
	b := reversed.TaskGroups[0].Tasks[0].Templates
	if len(a) != len(b) {
		t.Fatalf("template counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if *a[i].DestPath != *b[i].DestPath {
			t.Errorf("template %d: %q vs %q — reordering the manifest changed the job",
				i, *a[i].DestPath, *b[i].DestPath)
		}
	}
}

// A templated service still gets its generated secrets the way its template
// wires them, and can take declared ones alongside.
func TestDeclaredSecretsAlongsideATemplate(t *testing.T) {
	job := buildOneIn(t, "shop", `
name: db
template: postgres:17
volume: 1G
secrets:
  - name: extra
    path: extra.conf
`, "db", defaultOpts())

	task := job.TaskGroups[0].Tasks[0]
	findTemplate(t, task, "secrets/extra.conf")

	env := findTemplate(t, task, "secrets/orca.env")
	if !strings.Contains(*env.EmbeddedTmpl, "POSTGRES_PASSWORD") {
		t.Errorf("the template's own generated secret should still be wired, got:\n%s", *env.EmbeddedTmpl)
	}
}
