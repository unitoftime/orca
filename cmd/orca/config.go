package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/unitoftime/orca/pkg/manifest"
	"gopkg.in/yaml.v3"
)

// Fixed facts about every orca cluster.
//
// These are constants, not settings. Neither is a decision worth asking
// anyone to make: the datacenter name is invisible unless you read a job
// spec, and a data directory is better placed by mounting the disk you want at
// this path than by teaching orca a second path to worry about.
const (
	// Datacenter is the Nomad datacenter every job is scheduled in.
	Datacenter = "orca"

	// DataDir is where every byte orca persists on a machine lives: Docker's
	// data-root, Nomad's state, and every volume. One path, so "how full is
	// the box" has exactly one answer.
	DataDir = "/var/orca"
)

// Config is cluster.yaml: the machines, and the handful of cluster-wide facts
// that are not derivable from them or from the directory it sits in.
//
// What is *deployed* is not here. That comes from the directories beside this
// file, one group per directory, so the cluster is a thing you can see whole
// rather than a config file pointing at a list of other files.
type Config struct {
	// Name tells this cluster apart from others where they share something:
	// its backups are kept under <name>/ in their bucket, so two clusters
	// backing up to one bucket never prune or restore each other's. Optional;
	// without it backups sit at the top of the bucket.
	Name string `yaml:"name"`

	Nodes []NodeConfig `yaml:"nodes"`

	// The capabilities orca runs for you. Each is absent for "on with
	// defaults", `false` to turn off, or a mapping of settings. See
	// capabilities.go.
	//
	// Nothing about *what is deployed* appears here, and nothing here is named
	// after the software behind it. cluster.yaml is the machines, and what you
	// asked orca to provide on them.
	Firewall   FirewallConfig   `yaml:"firewall"`
	DNS        DNSConfig        `yaml:"dns"`
	Ingress    IngressConfig    `yaml:"ingress"`
	Monitoring MonitoringConfig `yaml:"monitoring"`

	// Root is the directory holding cluster.yaml. Not a config field: it is
	// where the groups are found.
	Root string `yaml:"-"`
}

// NodeRole is what a machine does in the cluster. At one machine this is always
// server (it is both the scheduler and the thing running workloads); it exists
// now so that adding a second machine is a config edit and not a schema change.
type NodeRole string

const (
	RoleServer NodeRole = "server" // runs the Nomad server (raft voter) and workloads
	RoleClient NodeRole = "client" // runs workloads only
)

type NodeConfig struct {
	// Host is how orca reaches this machine over SSH: "user@address".
	Host string `yaml:"host"`

	// Name is the Nomad node name. Defaults to the host portion of Host.
	Name string `yaml:"name"`

	// PrivateIP is this machine's address on the private network, and the only
	// address the cluster ever talks to itself on. It is named for what it
	// must be rather than what it does, because putting a public address here
	// would put an unauthenticated scheduler API on the internet.
	//
	// Leave it empty on a single-machine cluster: with no peers there is
	// nothing to reach, and everything binds loopback instead.
	PrivateIP string `yaml:"private_ip"`

	// Role defaults to server for the first node listed and client for the rest.
	Role NodeRole `yaml:"role"`
}

// LoadConfigFrom finds the cluster root by walking up from dir, then reads the
// cluster.yaml there. Working like git's search for .git means orca commands
// work from anywhere inside the tree.
func LoadConfigFrom(dir string) (Config, error) {
	root, err := manifest.FindRoot(dir)
	if err != nil {
		return Config{}, err
	}
	return LoadConfig(filepath.Join(root, manifest.ClusterFile))
}

// LoadConfig reads and validates a cluster.yaml. Unknown keys are rejected so a
// typo never silently takes a default.
func LoadConfig(path string) (Config, error) {
	var cfg Config

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}

	// Seeded before decoding: an absent capability key never reaches its
	// unmarshaler at all, so without this a config that mentions none of them
	// would come back with everything switched off.
	cfg.Firewall, cfg.DNS, cfg.Ingress, cfg.Monitoring = DefaultCapabilities()

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return cfg, err
	}
	cfg.Root = filepath.Dir(abs)

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	c.Monitoring.applyDefaults()
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if n.Name == "" {
			n.Name = strings.ReplaceAll(Node{Host: n.Host}.IP(), ".", "-")
		}
		if n.Role == "" {
			if i == 0 {
				n.Role = RoleServer
			} else {
				n.Role = RoleClient
			}
		}
	}
}

func (c Config) validate() error {
	if len(c.Nodes) == 0 {
		return fmt.Errorf("no nodes defined")
	}
	if c.Name != "" && !manifest.IsDNSLabel(c.Name) {
		return fmt.Errorf("name %q must be lowercase letters, digits and dashes", c.Name)
	}
	if err := c.Monitoring.validate(); err != nil {
		return err
	}

	seenName := map[string]string{}
	seenHost := map[string]bool{}
	servers := 0

	for _, n := range c.Nodes {
		if n.Host == "" {
			return fmt.Errorf("node %q: host is required", n.Name)
		}
		if seenHost[n.Host] {
			return fmt.Errorf("duplicate node host %q", n.Host)
		}
		seenHost[n.Host] = true

		if prev, ok := seenName[n.Name]; ok {
			return fmt.Errorf("duplicate node name %q (hosts %s and %s)", n.Name, prev, n.Host)
		}
		seenName[n.Name] = n.Host

		switch n.Role {
		case RoleServer:
			servers++
		case RoleClient:
		default:
			return fmt.Errorf("node %q: role must be %q or %q, got %q", n.Name, RoleServer, RoleClient, n.Role)
		}
	}

	if servers == 0 {
		return fmt.Errorf("no node has role %q; at least one is required", RoleServer)
	}
	// Raft tolerates (n-1)/2 failures, so an even count buys nothing over the
	// odd number below it while adding a machine that can break.
	for _, p := range []struct{ what, name string }{
		{"ingress.node", c.Ingress.Node},
		{"monitoring.node", c.Monitoring.Node},
	} {
		if _, ok := seenName[p.name]; p.name != "" && !ok {
			return fmt.Errorf("%s: no node named %q in nodes", p.what, p.name)
		}
	}

	if servers%2 == 0 {
		return fmt.Errorf("%d server nodes: raft needs an odd count (1, 3, 5)", servers)
	}
	// Multi-node means the cluster talks to itself over a network, and orca
	// only ever lets it do that on a private one. Without a private_ip there
	// is nowhere safe to bind, so this is a hard requirement rather than a
	// default that could quietly fall back to a public address.
	if len(c.Nodes) > 1 {
		for _, n := range c.Nodes {
			if n.PrivateIP == "" {
				return fmt.Errorf(
					"node %q: private_ip is required once the cluster has more than one machine: "+
						"the machines must share a private network, and orca will not bind the cluster to a public one",
					n.Name)
			}
		}
	}

	return nil
}

// HostNode is the first server: where a service with a volume lives unless
// its manifest says `node:`, and where ingress and monitoring run unless
// cluster.yaml says otherwise.
//
// On one machine it is the only machine, so everything that keeps data on
// one disk is already there when a second is added, and stays.
func (c Config) HostNode() (NodeConfig, error) {
	servers := c.Servers()
	if len(servers) == 0 {
		return NodeConfig{}, fmt.Errorf("no server node to run orca's own jobs on")
	}
	return servers[0], nil
}

// IngressNode is the machine ingress runs on, and so the one your DNS points
// at: `ingress: {node: ...}`, or the first server.
func (c Config) IngressNode() (NodeConfig, error) { return c.placed(c.Ingress.Node) }

// MonitoringNode is the machine the log and metric stores and the status page
// run on: `monitoring: {node: ...}`, or the first server.
//
// Separate from ingress so that the two things every other machine sends
// through one machine (every request, and every log line and metric) can
// be on different ones: a flood of logs does not slow the front door, and
// losing one machine does not take away both the traffic and the means to see
// why.
func (c Config) MonitoringNode() (NodeConfig, error) { return c.placed(c.Monitoring.Node) }

func (c Config) placed(name string) (NodeConfig, error) {
	if name == "" {
		return c.HostNode()
	}
	for _, n := range c.Nodes {
		if n.Name == name {
			return n, nil
		}
	}
	return NodeConfig{}, fmt.Errorf("no node named %q", name)
}

// MultiNode reports whether the cluster spans more than one machine, which is
// what decides whether anything binds a network address at all.
func (c Config) MultiNode() bool { return len(c.Nodes) > 1 }

// Servers returns the nodes running a Nomad server, in config order.
func (c Config) Servers() []NodeConfig {
	var out []NodeConfig
	for _, n := range c.Nodes {
		if n.Role == RoleServer {
			out = append(out, n)
		}
	}
	return out
}

// FindNode returns the node whose host or name matches ref.
func (c Config) FindNode(ref string) (NodeConfig, bool) {
	for _, n := range c.Nodes {
		if n.Host == ref || n.Name == ref {
			return n, true
		}
	}
	return NodeConfig{}, false
}
