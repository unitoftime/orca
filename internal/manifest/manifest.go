// Package manifest parses and validates a group: one directory and the services
// in it. It is pure data (no Nomad, no SSH, no network), so the whole surface
// is testable without a machine.
package manifest

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"

	"gopkg.in/yaml.v3"
)

// CPU is fractional vCPU held as thousandths, so `cpu: 0.5` is 500. Written as
// a decimal because "half a core" is how people think about it, and stored as
// an integer because comparing deploys for equality must not depend on float
// representation.
type CPU int

const MilliCPU CPU = 1

func ParseCPU(f float64) (CPU, error) {
	// Rounded rather than truncated, and refused below one thousandth: less
	// would become 0, which reads as "not set" and would be quietly replaced
	// by the default half a core.
	c := CPU(math.Round(f * 1000))
	if c < 1 {
		return 0, fmt.Errorf("cpu must be at least 0.001, got %v", f)
	}
	return c, nil
}

func (c CPU) Float() float64 { return float64(c) / 1000 }

func (c CPU) String() string {
	return strconv.FormatFloat(c.Float(), 'f', -1, 64)
}

func (c *CPU) UnmarshalYAML(node *yaml.Node) error {
	var f float64
	if err := node.Decode(&f); err != nil {
		return fmt.Errorf("cpu must be a number of vCPU, e.g. 0.5 or 2")
	}
	v, err := ParseCPU(f)
	if err != nil {
		return err
	}
	*c = v
	return nil
}

func (c CPU) MarshalYAML() (any, error) { return c.Float(), nil }

// Manifest is one group: the services in one directory. The files are desired
// state: a service removed from them is stopped on the next apply, so there
// is no enabled field and no way to express "declared but off".
//
// Neither field is written in a file. App comes from the directory name, so
// nothing inside repeats it and two groups cannot collide because two
// directories cannot share a path; Services are merged from every file in that
// directory.
type Manifest struct {
	App      string
	Services []*Service
	Path     string

	// Vars are the variables the group's files used, with the values they
	// were given, so what a ${var.NAME} became can be shown rather than
	// worked out.
	Vars Vars
}

// Service is one container in an app.
type Service struct {
	Name string `yaml:"name"`

	// Exactly one of Image, Template or Target. Image is something you built;
	// Template is infrastructure orca operates for you; Target is somewhere
	// outside the cluster that orca puts things, and runs nothing at all.
	Image    string `yaml:"image,omitempty"`
	Template string `yaml:"template,omitempty"`
	Target   string `yaml:"target,omitempty"`

	// Endpoint, Bucket, Path and Region belong to a Target and to nothing
	// else. Path is a folder in the bucket to keep everything under, which
	// is what lets two clusters share a bucket: without one, both would keep
	// a database of the same name in the same place, each pruning and
	// restoring the other's backups.
	Endpoint string `yaml:"endpoint,omitempty"`
	Bucket   string `yaml:"bucket,omitempty"`
	Path     string `yaml:"path,omitempty"`
	Region   string `yaml:"region,omitempty"`

	// Backup is where this service's dumps go. Only a templated service whose
	// template knows how to dump itself may have one.
	Backup *Backup `yaml:"backup,omitempty"`

	Replicas int    `yaml:"replicas,omitempty"`
	Cmd      string `yaml:"cmd,omitempty"`

	CPU    CPU     `yaml:"cpu,omitempty"`
	Memory Size    `yaml:"memory,omitempty"`
	Volume *Volume `yaml:"volume,omitempty"`

	Env map[string]string `yaml:"env,omitempty"`

	// Ports the service listens on, keyed by container port, each saying who
	// can reach it. A service with no ports listens for nothing and registers
	// nothing, which is most bots.
	Ports map[int]Port `yaml:"ports,omitempty"`

	// Secrets the service needs at run time, delivered as files under
	// /secrets unless one asks for an environment variable. Declaring them
	// here is what makes them exist: `orca secret list` reads this, and apply
	// refuses to deploy until every one is set.
	//
	// Separate from Env's ${secret.NAME}, which stays for the case this cannot
	// express: interpolating a secret into the middle of a larger value, as
	// in postgres://user:${secret.pw}@db/app.
	Secrets []Secret `yaml:"secrets,omitempty"`

	// TLS is a hostname the service serves TLS for by itself, on a raw port,
	// instead of behind ingress. orca gets the certificate and delivers it as
	// /secrets/tls/cert.pem and /secrets/tls/key.pem, rewritten in place when
	// it renews, so the service has to reload them from disk.
	TLS string `yaml:"tls,omitempty"`

	// Node pins the service to a named machine from cluster.yaml. Leave it
	// empty on a one-machine cluster. A service with a volume is pinned
	// whether or not this is set, because its data is on one disk; this only
	// says which machine, for when there is more than one to choose from.
	Node string `yaml:"node,omitempty"`

	// tmpl is the parsed Template, resolved during validation.
	tmpl *Template

	// srcFile is the file this service was read from. A group is merged from
	// several files, so the directory alone is not enough to point someone at
	// the line they need to fix.
	srcFile string
}

// SourceFile is the file this service was declared in.
func (s *Service) SourceFile() string { return s.srcFile }

// Defaults applied to any service that does not size itself. Generous rather
// than tight: the target machine has tens of gigabytes, and a service killed by
// a too-small default is a much worse first experience than one using more
// memory than it needs.
const (
	DefaultReplicas = 1
	DefaultCPU      = 500 * MilliCPU
	DefaultMemory   = 512 * Megabyte

	// MinMemory is the smallest allocation Nomad accepts. Anything less is
	// refused when the job is submitted, long after the manifest validated.
	MinMemory = 10 * Megabyte
)

// isEmpty reports a service document with nothing in it, which is what a
// trailing `---` or an all-comments file decodes to. Asked of the whole value
// rather than a list of fields, so a field added later cannot be forgotten.
func (s *Service) isEmpty() bool { return reflect.ValueOf(*s).IsZero() }

// Tmpl returns the resolved template, or nil for an image service.
func (s *Service) Tmpl() *Template { return s.tmpl }

// IsTemplated reports whether this service comes from a template.
func (s *Service) IsTemplated() bool { return s.tmpl != nil }

// ResolvedImage is the image this service runs, from the template when it has
// one. Tags are turned into digests later, at apply time.
func (s *Service) ResolvedImage() string {
	if s.tmpl != nil {
		return s.tmpl.Image()
	}
	return s.Image
}

// PortNumbers lists the service's container ports in ascending order. Map
// iteration is random and the generated jobspec has to be byte-stable, or every
// apply would look like a change and redeploy everything.
func (s *Service) PortNumbers() []int { return sortedPorts(s.Ports) }

// PrimaryPort is the port the service registers in the catalog (so that
// `db.shop` resolves to something) and the one the health check targets.
//
// The port routed through ingress when there is one, otherwise the lowest
// declared. Ingress routes to the registered port, so registering anything
// else would send a service's web traffic to whatever it listens on below it:
// a metrics port on 2112 would take the requests meant for the app on 3000. A
// service has at most one routed port, so this is never a choice.
//
// Without one, the lowest port that is not a metrics port: a server
// declaring `2113: metrics` and `9000: internal` is found by name at 9000's
// address and health-checked there, not on the side door it reports through.
// A metrics port is primary only when it is all the service listens on.
//
// Zero means nothing listens, so nothing is registered and nothing is checked.
func (s *Service) PrimaryPort() int {
	ports := s.PortNumbers()
	for _, p := range ports {
		if s.Ports[p].UsesIngress() {
			return p
		}
	}
	for _, p := range ports {
		if s.Ports[p].Kind != PortMetrics {
			return p
		}
	}
	if len(ports) > 0 {
		return ports[0]
	}
	return 0
}

// MetricsPort is the container port the metric store scrapes, or zero when
// the service declares none. Validation allows at most one.
func (s *Service) MetricsPort() int {
	for _, p := range s.PortNumbers() {
		if s.Ports[p].Kind == PortMetrics {
			return p
		}
	}
	return 0
}

// UsesIngress reports whether any of the service's ports is routed through
// the front door.
func (s *Service) UsesIngress() bool {
	for _, p := range s.Ports {
		if p.UsesIngress() {
			return true
		}
	}
	return false
}

// PublicPorts lists the container ports reachable from outside the cluster.
func (s *Service) PublicPorts() []int {
	var out []int
	for _, cport := range s.PortNumbers() {
		if s.Ports[cport].Public() {
			out = append(out, cport)
		}
	}
	return out
}

// normalize fills in every default, and resolves the template that some
// defaults depend on. It is the only step that changes a parsed manifest:
// Validate only reads. A default set anywhere else, least of all inside a
// validator, would make what a check sees depend on which ran first.
//
// Idempotent, and it never fails: a template that does not parse is left
// unresolved, for Validate to report.
func (m *Manifest) normalize() {
	for _, s := range m.Services {
		if s != nil {
			s.normalize()
		}
	}
}

func (s *Service) normalize() {
	if s.IsTarget() {
		// A target runs no container, so it has nothing to size, and
		// leaving those fields alone is what lets Validate report any that
		// were set.
		if s.Region == "" {
			s.Region = DefaultRegion
		}
		return
	}

	if s.Replicas == 0 {
		s.Replicas = DefaultReplicas
	}
	if s.CPU == 0 {
		s.CPU = DefaultCPU
	}

	if s.Template != "" && s.Image == "" {
		if t, err := ParseTemplate(s.Template); err == nil {
			s.tmpl = &t
			spec := t.Spec()
			// The template supplies the port so the author does not have to
			// know it. Declaring it yourself is still allowed (that is how
			// you publish a database deliberately), and only the number is
			// fixed, not the reach.
			if len(s.Ports) == 0 {
				s.Ports = map[int]Port{spec.Port: {Kind: PortInternal}}
			}
			if s.Memory == 0 {
				s.Memory = spec.DefaultMemory
			}
			// Filled only when empty, so a contradicting mount is still there
			// for Validate to report rather than being silently replaced.
			if s.Volume != nil && s.Volume.Mount == "" {
				s.Volume.Mount = spec.VolumeMount
			}
		}
	}
	if s.Memory == 0 {
		s.Memory = DefaultMemory
	}

	if s.Backup != nil {
		if s.Backup.Schedule == "" {
			s.Backup.Schedule = DefaultBackupSchedule
		}
		if s.Backup.Keep == 0 {
			s.Backup.Keep = DefaultBackupKeep
		}
	}
}

// Service returns the named service.
func (m *Manifest) Service(name string) (*Service, bool) {
	for _, s := range m.Services {
		if s != nil && s.Name == name {
			return s, true
		}
	}
	return nil, false
}

// GeneratedSecrets lists the secrets orca creates for this group's templated
// services. They are not referenced anywhere (the template wires them in
// directly), so they have to be reported rather than derived from usage.
func (m *Manifest) GeneratedSecrets() []string {
	var out []string
	for _, s := range m.Services {
		t := s.Tmpl()
		if t == nil {
			continue
		}
		for _, sec := range t.Spec().Secrets {
			out = append(out, GeneratedSecret(s.Name, sec.Suffix))
		}
	}
	sort.Strings(out)
	return out
}

// Secrets lists every secret the manifest asks for, whether declared in
// `secrets:` or referenced through ${secret.NAME}, sorted and deduplicated.
// Apply uses it to check that every one exists before deploying anything,
// rather than after half the app is already down.
func (m *Manifest) Secrets() []string {
	seen := map[string]bool{}
	for _, s := range m.Services {
		if s == nil {
			continue
		}
		for _, sec := range s.Secrets {
			seen[sec.Name] = true
		}
		if s.IsTarget() {
			seen[TargetKeyID(s.Name)] = true
			seen[TargetSecretKey(s.Name)] = true
		}
		for _, v := range s.Env {
			for _, name := range secretRefs(v) {
				seen[name] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// HostPort is one claim on the machine's port space.
type HostPort struct {
	Port    int
	Proto   string // "tcp" or "udp"
	App     string
	Service string
}

// HostPorts lists every raw host port this manifest claims, sorted. Host ports
// are a single namespace across every app on the machine, so apply collects
// these from all manifests to reject a collision before deploying rather than
// letting it surface as a placement failure. The firewall is generated from
// exactly this list.
func (m *Manifest) HostPorts() []HostPort {
	var out []HostPort
	for _, s := range m.Services {
		if s == nil {
			continue
		}
		for _, cport := range s.PortNumbers() {
			e := s.Ports[cport]
			if e.Kind != PortRaw {
				continue
			}
			for _, b := range e.Binds {
				out = append(out, HostPort{
					Port:    b.HostPort(cport),
					Proto:   b.Proto,
					App:     m.App,
					Service: s.Name,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Proto < out[j].Proto
	})
	return out
}

// ReservedGroup is the group name orca keeps for the jobs it runs for you:
// ingress, the resolver, the log and metric stores.
//
// It is the reserved name rather than a hidden one: those jobs show up in
// `orca status` and `orca logs` as orca/traefik, orca/dns and so on, so what
// is running is one list and not two. A directory of this name is refused,
// because it would file your services under the same prefix.
const ReservedGroup = "orca"
