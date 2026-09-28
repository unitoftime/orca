package manifest

import (
	"bytes"
	"strings"
	"testing"

	"github.com/unitoftime/orca/pkg/images"
	"gopkg.in/yaml.v3"
)

// A group file using most of the manifest: a template, a raw port, a secret
// in the environment, and several services in one file.
const fullExample = `
name: db
template: postgres:17
memory: 2048M
volume: 20G
---
name: api
image: ghcr.io/you/blog:1.4
cpu: 4
memory: 8192M
ports:
  7777: [tcp, udp]
env:
  SESSION_KEY: ${secret.session_key}
---
name: dev
image: ghcr.io/you/blog:dev
cpu: 1
memory: 2048M
ports:
  7777: tcp:7778
`

func TestParseFullExample(t *testing.T) {
	m, err := ParseGroup("blog", []byte(fullExample), "orca.yaml")
	if err != nil {
		t.Fatalf("the full example must parse: %v", err)
	}

	if m.App != "blog" || len(m.Services) != 3 {
		t.Fatalf("app = %q with %d services", m.App, len(m.Services))
	}

	db, _ := m.Service("db")
	if !db.IsTemplated() {
		t.Error("db should be templated")
	}
	if got := db.ResolvedImage(); got != images.Postgres17 {
		t.Errorf("db image = %q", got)
	}
	// The template supplies the port and the mount so the author does not have
	// to know either.
	if db.PrimaryPort() != 5432 {
		t.Errorf("db port = %d, want 5432 from the template", db.PrimaryPort())
	}
	if db.Volume.Mount != "/var/lib/postgresql/data" {
		t.Errorf("db mount = %q, want the template's", db.Volume.Mount)
	}

	api, _ := m.Service("api")
	if api.PrimaryPort() != 7777 {
		t.Errorf("api port = %d, want the lowest declared", api.PrimaryPort())
	}
	if api.CPU != 4000 {
		t.Errorf("api cpu = %d, want 4000 milli-vCPU", api.CPU)
	}
	if api.Replicas != 1 {
		t.Errorf("api replicas = %d, want the default 1", api.Replicas)
	}
}

func TestDefaults(t *testing.T) {
	m, err := ParseGroup("a", []byte("{name: bot, image: img:1}"), "orca.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := m.Services[0]
	if s.Replicas != DefaultReplicas || s.CPU != DefaultCPU || s.Memory != DefaultMemory {
		t.Errorf("defaults not applied: replicas=%d cpu=%d memory=%v", s.Replicas, s.CPU, s.Memory)
	}
	// No ports at all means a worker: nothing listens, nothing registers.
	if s.PrimaryPort() != 0 {
		t.Errorf("port = %d, want 0 for a service that listens for nothing", s.PrimaryPort())
	}
}

func TestSecrets(t *testing.T) {
	m, err := ParseGroup("a", []byte(`
name: one
image: img:1
env:
  A: ${secret.token}
  B: "prefix-${secret.token}-suffix"
  C: ${secret.other}
  D: plain
---
name: two
image: img:1
env:
  E: ${secret.token}
`), "orca.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got := m.Secrets()
	want := []string{"other", "token"} // sorted and deduplicated
	if len(got) != len(want) {
		t.Fatalf("Secrets() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Secrets() = %v, want %v", got, want)
			break
		}
	}
}

// $$ escapes a dollar, so an env value can contain a literal ${...} without
// being read as a secret reference.
func TestEscapedInterpolationIsNotASecret(t *testing.T) {
	m, err := ParseGroup("a", []byte("{name: one, image: img:1, env: {A: \"$${not.a.secret}\"}}"), "orca.yaml")
	if err != nil {
		t.Fatalf("an escaped interpolation should be allowed: %v", err)
	}
	if got := m.Secrets(); len(got) != 0 {
		t.Errorf("Secrets() = %v, want none", got)
	}
}

func TestHostPorts(t *testing.T) {
	m, err := ParseGroup("blog", []byte(`
name: api
image: img:1
ports:
  7777: [tcp, udp]
---
name: dev
image: img:1
ports:
  7777: tcp:7778
---
name: web
image: img:1
ports:
  8080: web.example.com
`), "orca.yaml")
	if err != nil {
		t.Fatal(err)
	}

	got := m.HostPorts()
	// Sorted by port then protocol, and a hostname port claims no host port at all.
	want := []HostPort{
		{Port: 7777, Proto: "tcp", App: "blog", Service: "api"},
		{Port: 7777, Proto: "udp", App: "blog", Service: "api"},
		{Port: 7778, Proto: "tcp", App: "blog", Service: "dev"},
	}
	if len(got) != len(want) {
		t.Fatalf("HostPorts() = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("HostPorts()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	cases := map[string]string{
		"top level":     "nodes: []\n---\n{name: x, image: i:1}",
		"service":       "{name: x, image: i:1, tier: app}",
		"inside volume": "name: x\nimage: i:1\nvolume: {size: 1G, mount: /d, mode: rw}",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseGroup("a", []byte(body), "orca.yaml"); err == nil {
				t.Fatal("expected an error for an unknown field")
			}
		})
	}
}

// yaml.Node.Decode drops the parent decoder's KnownFields setting, so the
// custom unmarshalers have to enforce strictness themselves. An unknown key
// inside `volume:` must be an error that names it, not silently ignored.
func TestVolumeStrictnessIsEnforcedByHand(t *testing.T) {
	_, err := ParseGroup("a", []byte("name: x\nimage: i:1\nvolume: {size: 1G, mount: /d, typo: 1}"), "orca.yaml")
	if err == nil {
		t.Fatal("expected an unknown-field error inside volume")
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestVolumeScalarAndMappingForms(t *testing.T) {
	m, err := ParseGroup("a", []byte("name: x\nimage: i:1\nvolume: {size: 5G, mount: /data}"), "orca.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if v := m.Services[0].Volume; v.Size != 5*Gigabyte || v.Mount != "/data" {
		t.Errorf("volume = %+v", v)
	}

	// The bare-size form is only meaningful with a template, which supplies the
	// mount; without one it must be rejected rather than mounted somewhere
	// arbitrary.
	if _, err := ParseGroup("a", []byte("{name: x, image: i:1, volume: 5G}"), "orca.yaml"); err == nil {
		t.Fatal("a bare volume size without a template should be rejected")
	}
}

// Services must re-parse from what orca writes: plan diffs serialize resolved
// services, and a parser that rejects its own output is a bug.
func TestServiceRoundTrip(t *testing.T) {
	m, err := ParseGroup("blog", []byte(fullExample), "orca.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var docs [][]byte
	for _, s := range m.Services {
		out, err := yaml.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, out)
	}

	again, err := ParseGroup("blog", bytes.Join(docs, []byte("---\n")), "orca.yaml")
	if err != nil {
		t.Fatalf("a marshalled service must parse again:\n%s\nerror: %v", bytes.Join(docs, []byte("---\n")), err)
	}
	if len(again.Services) != len(m.Services) {
		t.Fatalf("round trip changed the service count: %d vs %d", len(again.Services), len(m.Services))
	}
	for i := range m.Services {
		a, b := m.Services[i], again.Services[i]
		if a.Name != b.Name || a.Memory != b.Memory || a.CPU != b.CPU || a.PrimaryPort() != b.PrimaryPort() {
			t.Errorf("service %d differs after round trip: %+v vs %+v", i, a, b)
		}
	}
}

func TestParseEnvValue(t *testing.T) {
	parts, err := ParseEnvValue("a$$b${secret.pw}c$${secret.x}")
	if err != nil {
		t.Fatal(err)
	}
	want := []EnvPart{{Literal: "a$b"}, {Secret: "pw"}, {Literal: "c${secret.x}"}}
	if len(parts) != len(want) {
		t.Fatalf("parts = %+v, want %+v", parts, want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Errorf("parts[%d] = %+v, want %+v", i, parts[i], want[i])
		}
	}

	for _, bad := range []string{"${secrets.x}", "${secret.}", "${secret.a b}", "${unclosed", "x${}"} {
		if _, err := ParseEnvValue(bad); err == nil {
			t.Errorf("ParseEnvValue(%q) should be refused", bad)
		}
	}
	for _, ok := range []string{"", "$", "a$b", "$$", "cost: $5"} {
		if _, err := ParseEnvValue(ok); err != nil {
			t.Errorf("ParseEnvValue(%q) = %v", ok, err)
		}
	}
}
