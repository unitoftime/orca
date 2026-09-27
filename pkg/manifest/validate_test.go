package manifest

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			// `services:` was the old whole-app wrapper. A file is now a
			// stream of service documents, so the key is simply unknown.
			"the old services wrapper",
			"services:\n  - {name: x, image: i:1}\n",
			"field services not found",
		},
		{
			"empty file",
			"",
			"no services defined",
		},
		{
			"two metrics ports",
			"{name: x, image: i:1, ports: {2112: metrics, 2113: metrics}}",
			"declares 2 metrics ports",
		},
		{
			"service without a name",
			"{image: i:1}",
			"name is required",
		},
		{
			"duplicate service names",
			"{name: x, image: i:1}\n---\n{name: x, image: i:2}",
			`duplicate service name "x"`,
		},
		{
			"neither image nor template",
			"{name: x}",
			"needs an image, a template or a target",
		},
		{
			"both image and template",
			"{name: x, image: i:1, template: postgres:17, volume: 1G}",
			"use one",
		},
		{
			"unknown template",
			"{name: x, template: mysql:8}",
			"unknown template",
		},
		{
			"unknown template version",
			"{name: x, template: postgres:99, volume: 1G}",
			"has no version",
		},
		{
			"template without a version",
			"{name: x, template: postgres, volume: 1G}",
			"must be name:version",
		},
		{
			"template needs a volume",
			"{name: x, template: postgres:17}",
			"needs a volume",
		},
		{
			"contradicting the template's port",
			"{name: x, template: postgres:17, volume: 1G, ports: {1234: internal}}",
			"listens on 5432",
		},
		{
			"contradicting the template's mount",
			"name: x\ntemplate: postgres:17\nvolume: {size: 1G, mount: /wrong}",
			"fixes the volume mount",
		},
		{
			"volume without a mount",
			"{name: x, image: i:1, volume: 1G}",
			"needs a mount path",
		},
		{
			"relative volume mount",
			"name: x\nimage: i:1\nvolume: {size: 1G, mount: data}",
			"must be an absolute path",
		},
		{
			"volume with replicas",
			"name: x\nimage: i:1\nreplicas: 3\nvolume: {size: 1G, mount: /data}",
			"cannot be shared",
		},
		{
			"zero replicas",
			"{name: x, image: i:1, replicas: 0}",
			"",
		},
		{
			"negative replicas",
			"{name: x, image: i:1, replicas: -1}",
			"replicas must be at least 1",
		},
		{
			"bad env key",
			"name: x\nimage: i:1\nenv: {\"not-a-key\": v}",
			"env key",
		},
		{
			"near-miss secret reference",
			"name: x\nimage: i:1\nenv: {A: \"${secrets.token}\"}",
			"is not a secret reference",
		},
		{
			"two http exposures on one service",
			"name: x\nimage: i:1\nports:\n  8080: a.example.com\n  9090: b.example.com",
			"a service gets one hostname",
		},
		{
			"host port taken twice",
			"name: one\nimage: i:1\nports: {7777: tcp:7777}\n---\nname: two\nimage: i:1\nports: {8888: tcp:7777}",
			`already used by service "one"`,
		},
		{
			"hostname without a dot",
			"name: x\nimage: i:1\nports: {8080: localhost}",
			"needs a dot",
		},
		{
			"port out of range",
			"{name: x, image: i:1, ports: {70000: internal}}",
			"outside 1-65535",
		},
		{
			"memory without a unit",
			"{name: x, image: i:1, memory: 512}",
			"no unit",
		},
		{
			"cpu of zero",
			"{name: x, image: i:1, cpu: 0}",
			"cpu must be at least 0.001",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseGroup("a", []byte(tc.body), "orca.yaml")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected this to be accepted, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v\nwant it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// Every problem should be reported in one run, so fixing a manifest is not a
// sequence of one-typo-per-invocation.
func TestAllErrorsReportedAtOnce(t *testing.T) {
	_, err := ParseGroup("a", []byte(`
name: one
image: i:1
replicas: -1
---
name: two
replicas: -5
`), "orca.yaml")
	if err == nil {
		t.Fatal("expected errors")
	}
	got := err.Error()
	for _, want := range []string{"one", "two", "replicas must be at least 1", "needs an image, a template or a target"} {
		if !strings.Contains(got, want) {
			t.Errorf("combined error should mention %q, got:\n%s", want, got)
		}
	}
}

// Syntax errors abort decoding, so only the first is reported even when the
// manifest has several. This records the behaviour deliberately: semantic
// errors accumulate, syntax errors do not.
func TestSyntaxErrorsDoNotAccumulate(t *testing.T) {
	_, err := ParseGroup("a", []byte(`
name: one
image: i:1
memory: 512
---
name: two
image: i:1
memory: 256
`), "orca.yaml")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Count(err.Error(), "no unit") != 1 {
		t.Errorf("expected exactly one reported syntax error, got:\n%v", err)
	}
}

// The group name comes from the directory, so an unusable one means a
// directory that needs renaming — and the error has to say that, since there
// is no field in any file to go and fix.
func TestBadGroupName(t *testing.T) {
	_, err := ParseGroup("My_Group", []byte("{name: x, image: i:1}"), "My_Group/x.yaml")
	if err == nil {
		t.Fatal("expected an error for a group name that is not a DNS label")
	}
	for _, want := range []string{"My_Group", "rename the directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got: %v", want, err)
		}
	}
}

// Errors name the file and the service, because they are read in a CI log with
// no other context.
func TestErrorsArePrefixed(t *testing.T) {
	_, err := ParseGroup("blog", []byte("{name: api, image: i:1, replicas: -1}"), "blog/api.yaml")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.HasPrefix(err.Error(), `blog/api.yaml: service "api": `) {
		t.Errorf("error should be prefixed with file and service, got: %v", err)
	}
}

// normalize is the only step that changes a manifest. Validate reads, so
// checking twice — or checking at all — cannot change what gets deployed.
func TestValidateDoesNotMutate(t *testing.T) {
	m, err := ParseGroup("shop", []byte(`
name: db
template: postgres:17
volume: 5G
backup: {to: storage/offsite}
---
name: offsite
target: s3
endpoint: https://x
bucket: b
`), "shop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := yaml.Marshal(m.Services)
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	m.normalize()
	after, _ := yaml.Marshal(m.Services)
	if string(before) != string(after) {
		t.Errorf("validate or a second normalize changed the manifest:\n%s\nvs\n%s", before, after)
	}
}

func TestSizesNomadWouldRefuse(t *testing.T) {
	for body, want := range map[string]string{
		"{name: x, image: i:1, cpu: 0.0001}": "cpu must be at least 0.001",
		"{name: x, image: i:1, memory: 1K}":  "memory must be at least 10M",
	} {
		if _, err := ParseGroup("a", []byte(body), "a.yaml"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", body, err, want)
		}
	}
	if c, _ := ParseCPU(0.29); c != 290 {
		t.Errorf("0.29 cpu = %d milli, want 290", c)
	}
}
