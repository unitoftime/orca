package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/unitoftime/orca/internal/deploy"
	"github.com/unitoftime/orca/internal/registry"
)

func TestCheckRegistries(t *testing.T) {
	private := map[string][]string{
		"ghcr.io":   {"ghcr.io/you/web:latest", "ghcr.io/you/api:latest", "ghcr.io/you/web:latest"},
		"docker.io": {"you/bot:latest"},
	}

	if err := checkRegistries(private, map[string]bool{"ghcr.io": true, "docker.io": true}); err != nil {
		t.Errorf("every registry logged in to: %v", err)
	}

	err := checkRegistries(private, map[string]bool{"docker.io": true})
	if err == nil {
		t.Fatal("a private image with no credentials for its registry was allowed")
	}
	msg := err.Error()
	for _, want := range []string{"ghcr.io", "ghcr.io/you/api:latest, ghcr.io/you/web:latest", "orca registry login"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "you/bot") {
		t.Errorf("names a registry that is logged in to:\n%s", msg)
	}
	if strings.Count(msg, "ghcr.io/you/web:latest") != 1 {
		t.Errorf("an image used twice is listed twice:\n%s", msg)
	}

	if err := checkRegistries(nil, nil); err != nil {
		t.Errorf("no private images: %v", err)
	}
}

func TestParseLoginArgs(t *testing.T) {
	parse := func(args ...string) (string, string, error) {
		line, err := parseCommandLine(append([]string{"registry", "login"}, args...))
		if err != nil {
			return "", "", err
		}
		return loginArgs(line.in)
	}

	host, user, err := parse("GHCR.io", "-u", "you")
	if err != nil || host != "ghcr.io" || user != "you" {
		t.Errorf("got %q %q %v", host, user, err)
	}
	host, user, err = parse("--username=you", "index.docker.io")
	if err != nil || host != "docker.io" || user != "you" {
		t.Errorf("got %q %q %v", host, user, err)
	}
	for _, bad := range [][]string{nil, {"-u"}, {"ghcr.io", "extra"}, {"ghcr.io", "-p", "x"}, {"ghcr.io/you/app"}} {
		if _, _, err := parse(bad...); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

// TestCredentialHelper runs the helper script as Nomad would, against a stub
// curl standing in for Nomad's variable store. The contract that matters is
// Nomad's: exit zero always, JSON always, and {} whenever there is nothing to
// give: a non-zero exit fails the pull of every public image too.
func TestCredentialHelper(t *testing.T) {
	for _, tool := range []string{"sh", "jq", "tr"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}

	vars, err := nodeVars(Config{Nodes: []NodeConfig{{Host: "root@203.0.113.10", Name: "box0", Role: roleServer}}},
		NodeConfig{Host: "root@203.0.113.10", Name: "box0", Role: roleServer})
	if err != nil {
		t.Fatal(err)
	}
	script, err := renderTemplate("templates/host/docker-credential-orca", vars)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	helper := filepath.Join(dir, "docker-credential-orca")
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// The stub serves one variable, and records every URL it was asked for.
	// Anything else is a 404, which is what curl -f turns into a failure.
	stub := `#!/bin/sh
for a; do url="$a"; done
echo "$url" >> "` + filepath.Join(dir, "urls") + `"
case "$url" in
  */v1/var/orca-registry/ghcr_io) echo '{"Path":"orca-registry/ghcr_io","Items":{"username":"you","password":"to\"ken"}}' ;;
  */v1/var/orca-registry/localhost~5000) echo '{"Path":"orca-registry/localhost~5000","Items":{"username":"u","password":"p"}}' ;;
  *) exit 22 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command(helper, args...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("helper %v with %q: %v", args, stdin, err)
		}
		return strings.TrimSpace(string(out))
	}

	for stdin, want := range map[string]string{
		"ghcr.io":        `{"Username":"you","Secret":"to\"ken"}`,
		"GHCR.IO":        `{"Username":"you","Secret":"to\"ken"}`,
		"localhost:5000": `{"Username":"u","Secret":"p"}`,
		"docker.io":      `{}`, // no credentials: anonymous
		"":               `{}`,
		"a/../../v1/job": `{}`, // nothing but a host reaches the URL
	} {
		if got := run(stdin, "get"); got != want {
			t.Errorf("get %q = %s, want %s", stdin, got, want)
		}
	}

	urls, _ := os.ReadFile(filepath.Join(dir, "urls"))
	for _, u := range strings.Fields(string(urls)) {
		if !strings.HasPrefix(u, nomadAddr+"/v1/var/orca-registry/") || strings.Count(u, "/") != 6 {
			t.Errorf("helper asked for %s", u)
		}
	}

	// docker login's verbs are not this helper's business, and must not fail.
	for _, verb := range []string{"store", "erase", "list"} {
		run("", verb)
	}
}

type failingRegistry struct{ err error }

func (f failingRegistry) Resolve(context.Context, string) (registry.Pinned, error) {
	return registry.Pinned{}, f.err
}

// A registry that cannot be asked keeps what runs; one that answers "no such
// image" is believed, and a tag nothing runs yet has nothing to fall back to.
func TestUnreachableRegistryKeepsTheRunningDigest(t *testing.T) {
	current := map[string]deploy.JobState{"shop-web": {ImageRef: "ghcr.io/x/web:latest", Image: "ghcr.io/x/web@sha256:aaa"}}

	down := newImageResolver(context.Background(), failingRegistry{errors.New("dial tcp: i/o timeout")}, current)
	if got, err := down.Pin("ghcr.io/x/web:latest"); err != nil || got != "ghcr.io/x/web@sha256:aaa" {
		t.Errorf("registry down: got %q, %v; want the running digest", got, err)
	}
	if _, err := down.Pin("ghcr.io/x/new:1"); err == nil {
		t.Error("an image nothing runs has nothing to fall back to")
	}

	missing := newImageResolver(context.Background(), failingRegistry{&transport.Error{StatusCode: 404}}, current)
	if _, err := missing.Pin("ghcr.io/x/web:latest"); err == nil {
		t.Error("a registry that answers is believed, even for a running image")
	}
}
