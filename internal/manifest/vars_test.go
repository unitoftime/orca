package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tree writes files under a fresh root beside a cluster.yaml.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files[ClusterFile] = "nodes:\n  - host: root@10.0.0.1\n"
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func group(t *testing.T, groups []*Manifest, name string) *Manifest {
	t.Helper()
	for _, m := range groups {
		if m.App == name {
			return m
		}
	}
	t.Fatalf("no group %q", name)
	return nil
}

// The case variables exist for: one commit, several services, one edit.
// Values at the root are shared by every group, and a group's own vars.yaml
// overrides them.
func TestVarsFillInValues(t *testing.T) {
	root := tree(t, map[string]string{
		"vars.yaml":             "public_ip: 203.0.113.10\ncommit: shared\n",
		"blog/prod/vars.yaml":   "commit: 4f2a9c1\n",
		"blog/prod/server.yaml": "name: server\nimage: ghcr.io/x/server:${var.commit}\nenv:\n  PUBLIC_IP: ${var.public_ip}\n",
		"blog/prod/proxy.yaml":  "name: proxy\nimage: ghcr.io/x/proxy:${var.commit}\n",
		"blog/test/server.yaml": "name: server\nimage: ghcr.io/x/server:${var.commit}\n",
		"blog/organisation.txt": "not yaml",
	})
	groups, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}

	prod := group(t, groups, "blog-prod")
	for _, s := range prod.Services {
		if !strings.HasSuffix(s.Image, ":4f2a9c1") {
			t.Errorf("%s image = %q, want the group's commit", s.Name, s.Image)
		}
	}
	server, _ := prod.Service("server")
	if server.Env["PUBLIC_IP"] != "203.0.113.10" {
		t.Errorf("env from a root variable = %q", server.Env["PUBLIC_IP"])
	}
	if prod.Vars["commit"] != "4f2a9c1" || prod.Vars["public_ip"] != "203.0.113.10" {
		t.Errorf("used vars = %v", prod.Vars)
	}

	test := group(t, groups, "blog-test")
	if s, _ := test.Service("server"); s.Image != "ghcr.io/x/server:shared" {
		t.Errorf("test image = %q, want the root's commit", s.Image)
	}
	if _, ok := test.Vars["public_ip"]; ok {
		t.Error("only the variables a group used should be reported")
	}
}

// A directory holding only a vars.yaml is organisation, not a group, and a
// vars.yaml beside cluster.yaml is not a stray service file.
func TestVarsFileIsNotAServiceFile(t *testing.T) {
	root := tree(t, map[string]string{
		"vars.yaml":          "a: 1\n",
		"blog/vars.yaml":     "b: 2\n",
		"blog/prod/web.yaml": "{name: web, image: i:1}",
	})
	groups, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].App != "blog-prod" {
		t.Errorf("groups = %v, want only blog-prod", groups)
	}
}

// Filled in on the parsed document, so an unquoted value takes the type of
// what it holds and a quoted one stays a string, exactly as if written out.
func TestVarsAreTypedLikeTheValueTheyHold(t *testing.T) {
	vars := Vars{"n": "3", "mem": "2G", "cpu": "1.5", "tag": "007"}
	services, err := ParseServices([]byte(`
name: web
image: "i:${var.tag}"
replicas: ${var.n}
memory: ${var.mem}
cpu: ${var.cpu}
env:
  QUOTED: "${var.n}"
`), "web.yaml", vars)
	if err != nil {
		t.Fatal(err)
	}
	s := services[0]
	if s.Replicas != 3 || s.Memory != 2*Gigabyte || s.CPU != 1500 {
		t.Errorf("replicas=%d memory=%v cpu=%v", s.Replicas, s.Memory, s.CPU)
	}
	if s.Image != "i:007" || s.Env["QUOTED"] != "3" {
		t.Errorf("image=%q QUOTED=%q", s.Image, s.Env["QUOTED"])
	}
}

// Variables are filled in before env values are read, so the two kinds of
// reference mix, and $$ still escapes either.
func TestVarsAndSecretsTogether(t *testing.T) {
	services, err := ParseServices([]byte(`
name: web
image: i:1
env:
  DATABASE_URL: postgres://postgres:${secret.db_password}@${var.db_host}:5432/app
  LITERAL: $${var.db_host}
`), "web.yaml", Vars{"db_host": "db.shop"})
	if err != nil {
		t.Fatal(err)
	}
	env := services[0].Env
	parts, err := ParseEnvValue(env["DATABASE_URL"])
	if err != nil {
		t.Fatal(err)
	}
	if !HasSecret(parts) || !strings.Contains(env["DATABASE_URL"], "@db.shop:5432") {
		t.Errorf("DATABASE_URL = %q", env["DATABASE_URL"])
	}
	lit, err := ParseEnvValue(env["LITERAL"])
	if err != nil {
		t.Fatal(err)
	}
	if got := LiteralValue(lit); got != "${var.db_host}" {
		t.Errorf("$${var.x} should be the literal text, got %q", got)
	}
}

func TestVarErrors(t *testing.T) {
	cases := []struct {
		name, body string
		vars       Vars
		want       []string
	}{
		{"undefined", "{name: web, image: 'i:${var.commit}'}", Vars{"tag": "1"}, []string{"${var.commit} is not defined", "tag", "line 1"}},
		{"nothing in scope", "{name: web, image: 'i:${var.commit}'}", nil, []string{"no vars.yaml is in scope"}},
		{"in a key", "name: web\nimage: i:1\nenv:\n  ${var.k}: v\n", Vars{"k": "K"}, []string{"line 4", "is a key"}},
		{"near miss", "{name: web, image: 'i:${vars.commit}'}", Vars{"commit": "1"}, []string{"${var.NAME}"}},
		{"unclosed", "{name: web, image: 'i:${var.commit'}", Vars{"commit": "1"}, []string{"not closed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseServices([]byte(c.body), "web.yaml", c.vars)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error should mention %q, got: %v", w, err)
				}
			}
		})
	}
}

func TestParseVars(t *testing.T) {
	good, err := ParseVars([]byte("commit: 4f2a9c1\nreplicas: 3\n# a comment\n"), "vars.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if good["commit"] != "4f2a9c1" || good["replicas"] != "3" {
		t.Errorf("vars = %v", good)
	}
	if empty, err := ParseVars([]byte("# nothing yet\n"), "vars.yaml"); err != nil || len(empty) != 0 {
		t.Errorf("an empty vars file should be no vars, got %v, %v", empty, err)
	}

	bad := map[string]struct{ body, want string }{
		"a list":           {"tags: [a, b]\n", "single value"},
		"a mapping":        {"db: {host: x}\n", "single value"},
		"nothing":          {"commit:\n", "single value"},
		"a dollar":         {"x: ${secret.pw}\n", "may not contain $"},
		"a bad name":       {"\"a b\": 1\n", "not a variable name"},
		"twice":            {"a: 1\na: 2\n", "defined twice"},
		"two documents":    {"a: 1\n---\nb: 2\n", "more than one document"},
		"not a mapping":    {"- a\n", "mapping"},
		"really a service": {"name: web\nimage: i:1\n", "looks like a service"},
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := ParseVars([]byte(c.body), "prod/vars.yaml")
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "prod/vars.yaml") {
				t.Errorf("error should name the file and say %q, got: %v", c.want, err)
			}
		})
	}
}

func TestVarsYmlIsNamedAsTheMistake(t *testing.T) {
	root := tree(t, map[string]string{
		"blog/vars.yml": "commit: 1\n",
		"blog/web.yaml": "{name: web, image: 'i:1'}",
	})
	_, err := Discover(root)
	if err == nil || !strings.Contains(err.Error(), "variables go in vars.yaml") {
		t.Errorf("want vars.yml pointed at vars.yaml, got: %v", err)
	}
}

// A service is decoded from a node so variables can be filled in first, and
// Node.Decode does not carry KnownFields, so the strictness the decoder would
// give is checked by hand. Unknown fields and duplicate keys must still be
// refused, or a typo in a service file is silently ignored.
func TestServiceStrictnessSurvivesNodeDecoding(t *testing.T) {
	_, err := ParseServices([]byte("name: web\nimage: i:1\ntier: app\n"), "web.yaml", nil)
	if err == nil || !strings.Contains(err.Error(), "line 3: field tier not found") {
		t.Errorf("unknown field: got %v", err)
	}
	_, err = ParseServices([]byte("name: web\nimage: i:1\nname: other\n"), "web.yaml", nil)
	if err == nil || !strings.Contains(err.Error(), "already defined") {
		t.Errorf("duplicate key: got %v", err)
	}
	// A trailing separator and an all-comments document are still nothing.
	services, err := ParseServices([]byte("# header\n---\nname: web\nimage: i:1\n---\n"), "web.yaml", nil)
	if err != nil || len(services) != 1 {
		t.Errorf("empty documents: got %d services, %v", len(services), err)
	}
}
