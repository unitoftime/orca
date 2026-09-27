package deploy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"text/template"

	"github.com/hashicorp/go-envparse"
)

// A secret must reach the container from Nomad's variable store, so it never
// appears in the jobspec, which anyone who can read the cluster can read back.
func TestSecretsBecomeATemplateNotAnEnvVar(t *testing.T) {
	job := buildOneIn(t, "blog", `
name: api
image: i:1
env:
  PLAIN: hello
  TOKEN: ${secret.session_key}
`, "api", defaultOpts())

	task := job.TaskGroups[0].Tasks[0]

	if task.Env["PLAIN"] != "hello" {
		t.Errorf("a plain env var should stay in the jobspec, got %v", task.Env)
	}
	if _, leaked := task.Env["TOKEN"]; leaked {
		t.Error("a secret-bearing env var must not be in the jobspec")
	}

	if len(task.Templates) != 1 {
		t.Fatalf("want one template, got %d", len(task.Templates))
	}
	tmpl := *task.Templates[0].EmbeddedTmpl
	// One variable per secret, not one per group: listing which secrets are
	// set is then a listing of paths, which returns no values at all.
	if !strings.Contains(tmpl, `nomadVar "orca/blog/session_key"`) {
		t.Errorf("template should read the secret's own variable path, got:\n%s", tmpl)
	}
	// A backslash here means the quotes inside the template action were
	// escaped, which makes the template unparseable and no secret would ever
	// render. Worth asserting directly: the symptom on a real box is an opaque
	// template error, not a missing variable.
	if strings.Contains(tmpl, `\"`) {
		t.Errorf("template action must not be escaped, got:\n%s", tmpl)
	}
	if !strings.Contains(tmpl, ".value }}") {
		t.Errorf("template should read the variable's value item, got:\n%s", tmpl)
	}
	if !*task.Templates[0].Envvars {
		t.Error("the template must be rendered as environment variables")
	}
}

// A secret embedded in a larger string has to keep the surrounding text.
func TestSecretInsideALargerValue(t *testing.T) {
	job := buildOneIn(t, "blog", `
name: api
image: i:1
env:
  DSN: "postgres://app:${secret.pw}@db:5432/app"
`, "api", defaultOpts())

	tmpl := *job.TaskGroups[0].Tasks[0].Templates[0].EmbeddedTmpl
	if !strings.Contains(tmpl, "postgres://app:") || !strings.Contains(tmpl, "@db:5432/app") {
		t.Errorf("literal text around the secret was lost:\n%s", tmpl)
	}
}

// Siblings are found by name through the resolver. Their addresses were once
// rendered as ORCA_<SVC>_ADDR with change_mode restart, which restarted every
// service in a group whenever any other moved.
func TestNoSiblingAddressesAreRendered(t *testing.T) {
	job := buildOneIn(t, "blog", `
name: db
template: postgres:17
volume: 20G
---
name: api
image: i:1
ports:
  7777: tcp:7777
`, "api", defaultOpts())

	if got := job.TaskGroups[0].Tasks[0].Templates; len(got) != 0 {
		t.Errorf("api needs no template, got %q", *got[0].EmbeddedTmpl)
	}
}

// An escaped $${secret.x} is literal text. Validation and rendering once read
// the syntax differently, so this validated as a literal and was rendered as
// a lookup of a secret nobody set — blocking the task forever.
func TestEscapedReferenceIsLiteralEverywhere(t *testing.T) {
	job := buildOneIn(t, "blog", `
name: api
image: i:1
env:
  LIT: "$${secret.x}"
`, "api", defaultOpts())
	task := job.TaskGroups[0].Tasks[0]
	if len(task.Templates) != 0 {
		t.Errorf("a literal needs no template, got %q", *task.Templates[0].EmbeddedTmpl)
	}
	if task.Env["LIT"] != "${secret.x}" {
		t.Errorf("LIT = %q, want the literal ${secret.x}", task.Env["LIT"])
	}
}

// What the template renders is what the container gets: values that break an
// env-file line — quotes, backslashes, newlines — survive, and a {{ in the
// manifest is text rather than a template action. Rendered with stand-ins for
// Nomad's functions and parsed with the parser Nomad itself uses.
func TestEnvTemplateRoundTrips(t *testing.T) {
	job := buildOneIn(t, "blog", `
name: api
image: i:1
secrets:
  - name: pem
    env: PEM
env:
  DSN: "postgres://app:${secret.pw}@db/{{app}}?x=\"y\""
`, "api", defaultOpts())

	secrets := map[string]string{
		"orca/blog/pw":  `p"a\ss`,
		"orca/blog/pem": "-----BEGIN-----\nabc\n-----END-----",
	}
	got := renderEnv(t, *job.TaskGroups[0].Tasks[0].Templates[0].EmbeddedTmpl, secrets)

	if want := `postgres://app:p"a\ss@db/{{app}}?x="y"`; got["DSN"] != want {
		t.Errorf("DSN = %q, want %q", got["DSN"], want)
	}
	if got["PEM"] != secrets["orca/blog/pem"] {
		t.Errorf("PEM = %q, want %q", got["PEM"], secrets["orca/blog/pem"])
	}
}

// renderEnv executes an env template with stand-ins for consul-template's
// functions, then parses the result the way Nomad does.
func renderEnv(t *testing.T, tmpl string, secrets map[string]string) map[string]string {
	t.Helper()
	funcs := template.FuncMap{
		"nomadVar": func(path string) map[string]string {
			if v, ok := secrets[path]; ok {
				return map[string]string{SecretItemKey: v}
			}
			return nil
		},
		"nomadService": func(string) []struct {
			Address string
			Port    int
		} {
			return nil
		},
		"toJSON": func(v any) (string, error) {
			b, err := json.Marshal(v)
			return string(b), err
		},
	}
	parsed, err := template.New("env").Funcs(funcs).Parse(tmpl)
	if err != nil {
		t.Fatalf("template does not parse: %v\n%s", err, tmpl)
	}
	var out bytes.Buffer
	if err := parsed.Execute(&out, nil); err != nil {
		t.Fatalf("template does not execute: %v", err)
	}
	env, err := envparse.Parse(&out)
	if err != nil {
		t.Fatalf("Nomad could not parse the rendered env file: %v\n%s", err, out.String())
	}
	return env
}

func TestNoTemplateWhenNothingNeedsOne(t *testing.T) {
	job := buildOne(t, "{name: solo, image: i:1, env: {A: b}}", "solo", defaultOpts())
	if got := job.TaskGroups[0].Tasks[0].Templates; len(got) != 0 {
		t.Errorf("a service with no secrets and no siblings needs no template, got %d", len(got))
	}
}
