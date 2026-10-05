package main

import (
	"fmt"

	"github.com/unitoftime/orca/internal/manifest"
	"gopkg.in/yaml.v3"
)

// The capabilities orca runs for you, and their settings.
//
// Each is a top-level key in cluster.yaml, named for what it is rather than
// for the machinery behind it: `ingress` and not "traefik", `monitoring` and
// not "victorialogs and victoriametrics and vector". What runs underneath is
// orca's business and can change; what you asked for is yours.
//
// Every one is written as `false` to turn it off, or as a mapping of settings:
//
//	firewall: true
//	dns: true
//	ingress:
//	  https: false
//	monitoring:
//	  domain: example.com
//	  logs:
//	    retention: 14d
//
// An absent capability means on, with defaults. You disable by saying false,
// never by omission, so deleting a settings block never silently removes a
// running component.

// FirewallConfig locks the public interface down to what the manifests ask
// for. It has no settings: what is open is derived from the raw tcp/udp ports
// the manifests declare and from whether ingress runs, which is the whole
// point: a port is open because something asked for it, never because
// someone edited a rule.
type FirewallConfig struct {
	Enabled bool `yaml:"-"`
}

// DNSConfig is the cluster resolver, which is how services find each other by
// name. Turning it off leaves them with no way to, so it has no settings,
// only an on and an off.
type DNSConfig struct {
	Enabled bool `yaml:"-"`
}

// IngressConfig is the HTTP front door: hostnames, TLS, and the one place a
// request from the internet enters the cluster.
//
// None of this is a top-level cluster setting, because none of it describes
// the cluster: every one of these is consumed only by the front door, and
// with ingress off none of them mean anything.
type IngressConfig struct {
	Enabled bool `yaml:"-"`

	// HTTPS is whether ingress gets certificates from Let's Encrypt and
	// serves HTTPS. On unless it is false. Off is for hostnames Let's
	// Encrypt cannot reach to verify (ones that exist only in your own
	// /etc/hosts), where asking for certificates can only fail.
	HTTPS *bool `yaml:"https"`

	// ACMEEmail is an optional contact on the Let's Encrypt account. It
	// does not decide whether there is HTTPS at all, because Let's Encrypt
	// issues certificates to an account with no contact.
	ACMEEmail string `yaml:"acme_email"`

	// ACMEDirectory is the certificate authority that certificates come
	// from, as its ACME directory URL. Empty is Let's Encrypt. Its
	// staging directory is the one to use while trying things out: it has
	// far higher limits, and its certificates are not trusted by browsers.
	ACMEDirectory string `yaml:"acme_directory"`

	// Node is the machine ingress runs on, and so the one your DNS points
	// at. Empty is the first server. See Config.IngressNode.
	Node string `yaml:"node"`
}

// MonitoringConfig is knowing what your services are doing: their logs and
// their metrics, collected and kept.
//
// One capability with two halves, because that is how it is thought about,
// but each half is separately disablable, because wanting logs without metrics
// on a small machine is an ordinary thing to want.
type MonitoringConfig struct {
	Enabled bool `yaml:"-"`

	Logs    LogsConfig    `yaml:"logs"`
	Metrics MetricsConfig `yaml:"metrics"`
	Status  StatusConfig  `yaml:"status"`

	// Domain is where the dashboards are published: "example.com" gives
	// status.example.com, and nomad., logs. and metrics. beside it. Empty
	// publishes none; they are then reachable only over an SSH tunnel.
	// Services name their own hostnames, so nothing else hangs off it.
	Domain string `yaml:"domain"`

	// Node is the machine the log and metric stores and the status page run
	// on. Empty is the first server. See Config.MonitoringNode.
	Node string `yaml:"node"`
}

// StatusConfig is the status page at status.<domain>: every machine and
// service at a glance. It has no settings, only an off switch.
type StatusConfig struct {
	Enabled bool `yaml:"-"`
}

// LogsConfig is the log store plus a shipper on every machine.
type LogsConfig struct {
	Enabled bool `yaml:"-"`

	// Retention is how far back logs are kept.
	Retention string `yaml:"retention"`

	// Disk is a hard ceiling on the log store. When it is reached the oldest
	// days are dropped: logs never grow into the space the database needs.
	Disk string `yaml:"disk"`
}

// MetricsConfig is the metric store, which both scrapes and keeps.
type MetricsConfig struct {
	Enabled bool `yaml:"-"`

	// Retention is how far back metrics are kept. Unlike the log store, the
	// metric store has no byte ceiling, so time is the bound on how large it
	// gets; MinFree is the backstop that keeps it from filling the disk.
	Retention string `yaml:"retention"`

	// MinFree is the free space at which it stops accepting new data rather
	// than consuming the last of the disk.
	MinFree string `yaml:"min_free"`
}

// Capability defaults. Deliberately modest: this is a personal cluster, and
// knowing what your services are doing should be a rounding error next to the
// workloads, not a second tenant.
const (
	DefaultLogRetention    = "14d"
	DefaultLogDisk         = "10G"
	DefaultMetricRetention = "30d"
	DefaultMetricMinFree   = "2G"
)

// DefaultCapabilities is what a cluster.yaml that mentions none of them gets.
func DefaultCapabilities() (FirewallConfig, DNSConfig, IngressConfig, MonitoringConfig) {
	return FirewallConfig{Enabled: true},
		DNSConfig{Enabled: true},
		IngressConfig{Enabled: true},
		MonitoringConfig{
			Enabled: true,
			Logs:    LogsConfig{Enabled: true, Retention: DefaultLogRetention, Disk: DefaultLogDisk},
			Metrics: MetricsConfig{Enabled: true, Retention: DefaultMetricRetention, MinFree: DefaultMetricMinFree},
			Status:  StatusConfig{Enabled: true},
		}
}

// decodeToggle implements the shared "false, or a mapping of settings" shape.
// A capability is enabled unless it is explicitly false.
func decodeToggle(node *yaml.Node, what string, allowed []string, out any) (bool, error) {
	if node == nil || node.Kind == 0 {
		return true, nil
	}

	if node.Kind == yaml.ScalarNode {
		var on bool
		if err := node.Decode(&on); err != nil {
			return false, fmt.Errorf("%s must be false, or a mapping of settings", what)
		}
		return on, nil
	}

	if node.Kind != yaml.MappingNode {
		return false, fmt.Errorf("%s must be false, or a mapping of settings", what)
	}
	// node.Decode drops the parent decoder's KnownFields setting, so strictness
	// is enforced by hand or a typo silently takes a default.
	if err := checkCapabilityFields(node, what, allowed); err != nil {
		return false, err
	}
	if err := node.Decode(out); err != nil {
		return false, err
	}
	return true, nil
}

// hasKey reports whether a mapping sets key.
func hasKey(node *yaml.Node, key string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return true
		}
	}
	return false
}

func checkCapabilityFields(node *yaml.Node, what string, allowed []string) error {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if key := node.Content[i].Value; !ok[key] {
			return fmt.Errorf("%s: unknown field %q", what, key)
		}
	}
	return nil
}

func (c *FirewallConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain FirewallConfig
	var out plain
	on, err := decodeToggle(node, "firewall", nil, &out)
	if err != nil {
		return err
	}
	*c = FirewallConfig(out)
	c.Enabled = on
	return nil
}

func (c *DNSConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain DNSConfig
	var out plain
	on, err := decodeToggle(node, "dns", nil, &out)
	if err != nil {
		return err
	}
	*c = DNSConfig(out)
	c.Enabled = on
	return nil
}

// TLS is whether ingress serves HTTPS with certificates it gets itself.
func (c IngressConfig) TLS() bool {
	return c.Enabled && (c.HTTPS == nil || *c.HTTPS)
}

func (c *IngressConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain IngressConfig
	var out plain
	if hasKey(node, "domain") {
		return fmt.Errorf("ingress: domain has moved to monitoring.domain, and is only where the dashboards are published; a service names its own hostname")
	}
	// The dashboards' password is the cluster's, generated at bootstrap, and
	// is changed the way a secret is: in the cluster, not in a file.
	if hasKey(node, "admin_password") {
		return fmt.Errorf("ingress: admin_password is no longer set in cluster.yaml; the dashboards have a password generated for the cluster. " +
			"`orca password` shows it and `orca password set` changes it")
	}
	on, err := decodeToggle(node, "ingress", []string{"https", "acme_email", "acme_directory", "node"}, &out)
	if err != nil {
		return err
	}
	*c = IngressConfig(out)
	c.Enabled = on
	return nil
}

func (c *MonitoringConfig) UnmarshalYAML(node *yaml.Node) error {
	// Seeded before decoding for the same reason the top-level config is: an
	// absent `logs:` inside a present `monitoring:` never reaches the halves'
	// unmarshalers, so without this "monitoring: {metrics: false}" would come
	// back with logs switched off too.
	_, _, _, d := DefaultCapabilities()
	out := plainMonitoring{Logs: d.Logs, Metrics: d.Metrics, Status: d.Status}

	on, err := decodeToggle(node, "monitoring", []string{"logs", "metrics", "status", "domain", "node"}, &out)
	if err != nil {
		return err
	}
	*c = MonitoringConfig(out)
	c.Enabled = on

	// `monitoring: false` turns off both halves, so everything downstream can
	// ask one question ("are logs on?") instead of two.
	if !on {
		c.Logs.Enabled = false
		c.Metrics.Enabled = false
		c.Status.Enabled = false
	}
	return nil
}

// plainMonitoring avoids recursing into MonitoringConfig's own unmarshaler.
type plainMonitoring struct {
	Enabled bool          `yaml:"-"`
	Logs    LogsConfig    `yaml:"logs"`
	Metrics MetricsConfig `yaml:"metrics"`
	Status  StatusConfig  `yaml:"status"`
	Domain  string        `yaml:"domain"`
	Node    string        `yaml:"node"`
}

func (c *LogsConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain LogsConfig
	var out plain
	on, err := decodeToggle(node, "monitoring.logs", []string{"retention", "disk"}, &out)
	if err != nil {
		return err
	}
	*c = LogsConfig(out)
	c.Enabled = on
	return nil
}

func (c *StatusConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain StatusConfig
	var out plain
	on, err := decodeToggle(node, "monitoring.status", nil, &out)
	if err != nil {
		return err
	}
	*c = StatusConfig(out)
	c.Enabled = on
	return nil
}

func (c *MetricsConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain MetricsConfig
	var out plain
	on, err := decodeToggle(node, "monitoring.metrics", []string{"retention", "min_free"}, &out)
	if err != nil {
		return err
	}
	*c = MetricsConfig(out)
	c.Enabled = on
	return nil
}

// applyDefaults fills unset settings for whatever is enabled.
func (m *MonitoringConfig) applyDefaults() {
	_, _, _, d := DefaultCapabilities()
	if m.Logs.Retention == "" {
		m.Logs.Retention = d.Logs.Retention
	}
	if m.Logs.Disk == "" {
		m.Logs.Disk = d.Logs.Disk
	}
	if m.Metrics.Retention == "" {
		m.Metrics.Retention = d.Metrics.Retention
	}
	if m.Metrics.MinFree == "" {
		m.Metrics.MinFree = d.Metrics.MinFree
	}
}

// validate resolves the size settings, so a bad value is reported when the
// config is read rather than as a container that exits 2 on the machine.
func (m MonitoringConfig) validate() error {
	if m.Logs.Enabled {
		if _, err := manifest.ParseSize(m.Logs.Disk); err != nil {
			return fmt.Errorf("monitoring.logs.disk: %w", err)
		}
	}
	if m.Metrics.Enabled {
		if _, err := manifest.ParseSize(m.Metrics.MinFree); err != nil {
			return fmt.Errorf("monitoring.metrics.min_free: %w", err)
		}
	}
	return nil
}

// LogDiskBytes is the log store ceiling in bytes. Only valid after validate.
func (m MonitoringConfig) LogDiskBytes() int64 {
	n, _ := manifest.ParseSize(m.Logs.Disk)
	return int64(n)
}

// MetricsMinFreeBytes is the metrics free-space floor in bytes.
func (m MonitoringConfig) MetricsMinFreeBytes() int64 {
	n, _ := manifest.ParseSize(m.Metrics.MinFree)
	return int64(n)
}
