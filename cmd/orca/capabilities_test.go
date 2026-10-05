package main

import (
	"strings"
	"testing"
)

// A config that mentions no capability gets all of them. The unmarshalers are
// never called for an absent key, so this is the case that would silently
// disable everything if the defaults were not seeded first.
func TestCapabilityDefaultsWhenAbsent(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "nodes:\n  - host: root@10.0.0.1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Firewall.Enabled || !cfg.DNS.Enabled || !cfg.Ingress.Enabled {
		t.Error("firewall, dns and ingress should default on")
	}
	if !cfg.Monitoring.Logs.Enabled || !cfg.Monitoring.Metrics.Enabled || !cfg.Monitoring.Status.Enabled {
		t.Errorf("monitoring should default on, got %+v", cfg.Monitoring)
	}
	if cfg.Monitoring.Logs.Disk != defaultLogDisk || cfg.Monitoring.Logs.Retention != defaultLogRetention {
		t.Errorf("log defaults not applied: %+v", cfg.Monitoring.Logs)
	}
}

func TestCapabilityDisable(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `
ingress: false
nodes:
  - host: root@10.0.0.1
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ingress.Enabled {
		t.Error("ingress: false should disable it")
	}
	// Disabling one capability must not disturb the others.
	if !cfg.Monitoring.Logs.Enabled || !cfg.Monitoring.Metrics.Enabled || !cfg.DNS.Enabled {
		t.Error("other capabilities should be untouched")
	}
}

// Settings are written as a mapping, and a capability with settings is on.
func TestCapabilitySettings(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `
monitoring:
  domain: example.com
  logs:
    retention: 7d
    disk: 2G
ingress:
  acme_email: me@example.com
nodes:
  - host: root@10.0.0.1
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Monitoring.Logs.Enabled || cfg.Monitoring.Logs.Retention != "7d" {
		t.Errorf("logs = %+v", cfg.Monitoring.Logs)
	}
	if got := cfg.Monitoring.LogDiskBytes(); got != 2<<30 {
		t.Errorf("disk bytes = %d, want %d", got, 2<<30)
	}
	// Only the front door reads this, so it lives under ingress rather than
	// at the top level.
	if cfg.Ingress.ACMEEmail != "me@example.com" {
		t.Errorf("ingress = %+v", cfg.Ingress)
	}
	if cfg.Monitoring.Domain != "example.com" {
		t.Errorf("monitoring domain = %q", cfg.Monitoring.Domain)
	}
	// An unmentioned setting keeps its default rather than becoming zero.
	if cfg.Monitoring.Metrics.Retention != defaultMetricRetention {
		t.Errorf("metrics retention = %q, want the default", cfg.Monitoring.Metrics.Retention)
	}
}

// Monitoring has two halves and they are separately disablable: wanting logs
// without metrics on a small machine is an ordinary thing to want.
func TestMonitoringHalvesAreIndependent(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `
monitoring:
  metrics: false
nodes:
  - host: root@10.0.0.1
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Monitoring.Metrics.Enabled {
		t.Error("metrics: false should disable metrics")
	}
	// The half that was not mentioned never reaches its own unmarshaler, so
	// without seeding inside MonitoringConfig this would come back off too.
	if !cfg.Monitoring.Logs.Enabled {
		t.Error("logs should stay on when only metrics was disabled")
	}
	if cfg.Monitoring.Logs.Retention != defaultLogRetention {
		t.Errorf("logs retention = %q, want the default", cfg.Monitoring.Logs.Retention)
	}
}

// Turning the whole capability off takes both halves with it, so everything
// downstream asks one question instead of two.
func TestMonitoringFalseDisablesBoth(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `
monitoring: false
nodes:
  - host: root@10.0.0.1
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Monitoring.Logs.Enabled || cfg.Monitoring.Metrics.Enabled || cfg.Monitoring.Status.Enabled {
		t.Errorf("monitoring: false should disable all of it, got %+v", cfg.Monitoring)
	}
}

// The status page has only an off switch, and turning it off leaves the
// stores it reads alone.
func TestMonitoringStatusSwitch(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `
monitoring:
  status: false
nodes:
  - host: root@10.0.0.1
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Monitoring.Status.Enabled {
		t.Error("status: false should disable the status page")
	}
	if !cfg.Monitoring.Logs.Enabled || !cfg.Monitoring.Metrics.Enabled {
		t.Error("the stores should stay on")
	}
	for _, j := range buildPlatformJobs(cfg, "", nil) {
		if j.Meta["orca.service"] == "status" {
			t.Error("status is disabled but a job was built for it")
		}
	}

	if _, err := loadConfig(writeConfig(t, `
monitoring:
  status:
    port: 80
nodes:
  - host: root@10.0.0.1
`)); err == nil || !strings.Contains(err.Error(), "port") {
		t.Errorf("status takes no settings; want an error naming the key, got %v", err)
	}
}

// The status job runs the build apply ships, found on the machine by its hash.
func TestStatusJobRunsTheShippedBuild(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "nodes:\n  - host: root@10.0.0.1\n"))
	if err != nil {
		t.Fatal(err)
	}
	bin := &statusBinary{Local: "/tmp/orca", Sum: strings.Repeat("ab", 32)}
	opts := platformOptions(cfg, "", bin)
	if opts.Status == nil {
		t.Fatal("status should be on by default")
	}
	if opts.Status.Binary != "/var/orca/bin/orca-abababababababab" {
		t.Errorf("binary = %q", opts.Status.Binary)
	}
	if !strings.HasPrefix(opts.Status.Image, "alpine:") {
		t.Errorf("image = %q", opts.Status.Image)
	}
}

// A typo inside a capability must not silently take a default: yaml.Node.Decode
// drops the parent decoder's strictness, so it is enforced by hand.
func TestCapabilityRejectsUnknownFields(t *testing.T) {
	_, err := loadConfig(writeConfig(t, `
monitoring:
  logs:
    retentionn: 7d
nodes:
  - host: root@10.0.0.1
`))
	if err == nil {
		t.Fatal("expected an error for an unknown field")
	}
	if !strings.Contains(err.Error(), "retentionn") {
		t.Errorf("error should name the key, got: %v", err)
	}
}

// A bad size is reported when the config is read, not as a container that
// exits 2 on the machine with its usage text and no error.
func TestCapabilityRejectsBadSize(t *testing.T) {
	_, err := loadConfig(writeConfig(t, `
monitoring:
  logs:
    disk: loads
nodes:
  - host: root@10.0.0.1
`))
	if err == nil {
		t.Fatal("expected an error for an unparseable size")
	}
	if !strings.Contains(err.Error(), "monitoring.logs.disk") {
		t.Errorf("error should name the setting, got: %v", err)
	}
}

// A disabled capability produces no job at all, which is how the ordinary
// "stop what is no longer declared" rule removes it from a running cluster.
func TestDisabledCapabilityProducesNoJobs(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `
ingress: false
monitoring:
  metrics: false
nodes:
  - host: root@10.0.0.1
    name: box0
`))
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	for _, j := range buildPlatformJobs(cfg, "", nil) {
		names[j.Meta["orca.service"]] = true
	}

	for _, off := range []string{"traefik", "victoriametrics"} {
		if names[off] {
			t.Errorf("%s is disabled but a job was built for it", off)
		}
	}
	for _, on := range []string{"victorialogs", "vector"} {
		if !names[on] {
			t.Errorf("%s is enabled but no job was built for it", on)
		}
	}
}

// HTTPS is on whenever ingress is, with or without an email: Let's Encrypt
// issues certificates to an account with no contact. Off is said outright.
func TestIngressHTTPS(t *testing.T) {
	for _, c := range []struct {
		yaml string
		want bool
	}{
		{"ingress: {}\n", true},
		{"ingress:\n  acme_email: me@example.com\n", true},
		{"ingress:\n  https: false\n", false},
		{"ingress: false\n", false},
	} {
		cfg, err := loadConfig(writeConfig(t, c.yaml+"nodes:\n  - host: root@10.0.0.1\n"))
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Ingress.TLS(); got != c.want {
			t.Errorf("%q: TLS = %v, want %v", c.yaml, got, c.want)
		}
	}
}
