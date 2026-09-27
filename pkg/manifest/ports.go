package manifest

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// PortKind is who can reach a port.
type PortKind string

const (
	// PortInternal is reachable by other services and by nothing else. It is
	// registered in the catalog, so `db.shop` resolves, and it is never
	// published on a public interface.
	PortInternal PortKind = "internal"

	// PortDomain is routed through ingress on a hostname you chose.
	PortDomain PortKind = "domain"

	// PortMetrics is internal, and also scraped: the metric store collects
	// Prometheus metrics from its /metrics path. A kind of port rather than a
	// separate setting because "what is on this port" is exactly what the
	// ports map says, and a scrape target is only ever a port the service
	// already listens on.
	PortMetrics PortKind = "metrics"

	// PortRaw binds a host port with no proxy in the path. This is what a raw
	// TCP or UDP service wants: a proxy in its path is latency nobody asked for.
	PortRaw PortKind = "raw"
)

// Bind is one protocol a raw port answers on, and the host port it binds.
type Bind struct {
	Proto string // "tcp" or "udp"
	Host  int    // 0 means "the same number as the container port"
}

// HostPort resolves the host port this bind takes.
func (b Bind) HostPort(container int) int {
	if b.Host != 0 {
		return b.Host
	}
	return container
}

// Port is the value side of a service's `ports:` map, keyed by container port.
//
// One field answers one question: *what does this service listen on, and who
// can reach each one*. The two halves of that question are not independent.
// A field for the catalog and health check and another for the outside world
// would be the same list seen from two angles, and telling them apart would be
// the first thing anyone asked about.
//
// The value is a single scalar:
//
//	5432: internal              siblings only
//	2112: metrics               siblings only, and scraped for metrics
//	8080: errors.example.com    ingress, at a hostname you chose
//	7777: tcp                   raw host port 7777
//	7777: udp:7778              raw, host port differs from the container's
//
// or a list of protocols, which is the only thing a scalar cannot say, because
// YAML has no way to write the same key twice:
//
//	7777: [tcp, udp]
//	7777: [tcp:7777, udp:7778]
type Port struct {
	Kind PortKind

	Domain string // PortDomain only

	Binds []Bind // PortRaw only
}

// Protocols lists the protocols a raw port binds, ordered, for rendering and
// for messages. Empty unless Kind is PortRaw.
func (p Port) Protocols() []string {
	var out []string
	for _, b := range p.Binds {
		out = append(out, b.Proto)
	}
	return out
}

// Public reports whether this port is reachable from outside the cluster.
//
// Stated as the kinds that are public rather than "not internal", so a Port
// whose kind was never set fails closed.
func (p Port) Public() bool { return p.Kind == PortRaw || p.UsesIngress() }

// UsesIngress reports whether this port is routed through the front door.
func (p Port) UsesIngress() bool { return p.Kind == PortDomain }

func (p *Port) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var raw string
		if err := node.Decode(&raw); err != nil {
			return fmt.Errorf("port must be internal, metrics, http, a hostname, or a protocol like tcp or udp:7778")
		}
		parsed, err := ParsePort(raw)
		if err != nil {
			return err
		}
		*p = parsed
		return nil
	}

	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("port must be a single value, or a list of protocols like [tcp, udp]")
	}

	// A list exists for exactly one case: both protocols on one container
	// port. Everything else a list could express is already a single scalar,
	// so anything but protocols here is a mistake worth naming.
	var out Port
	out.Kind = PortRaw
	seen := map[string]bool{}

	for i := range node.Content {
		var raw string
		if err := node.Content[i].Decode(&raw); err != nil {
			return fmt.Errorf("ports[%d]: expected a protocol like tcp or udp:7778", i)
		}
		parsed, err := ParsePort(raw)
		if err != nil {
			return err
		}
		if parsed.Kind != PortRaw {
			return fmt.Errorf(
				"a list of ports may only hold protocols (tcp, udp); %q belongs on its own", raw)
		}
		b := parsed.Binds[0]
		if seen[b.Proto] {
			return fmt.Errorf("%s is listed twice", b.Proto)
		}
		seen[b.Proto] = true
		out.Binds = append(out.Binds, b)
	}

	if len(out.Binds) == 0 {
		return fmt.Errorf("a list of ports must name at least one protocol")
	}
	sortBinds(out.Binds)
	*p = out
	return nil
}

func sortBinds(b []Bind) {
	sort.Slice(b, func(i, j int) bool { return b[i].Proto < b[j].Proto })
}

// ParsePort reads one authored scalar.
//
// Keywords win over hostnames, so "internal", "metrics" and the protocol
// forms are reserved and cannot be custom domains.
func ParsePort(s string) (Port, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Port{}, fmt.Errorf(`port value is empty; use "internal", "metrics", a hostname, or a protocol like "tcp"`)
	}

	switch strings.ToLower(raw) {
	case string(PortInternal):
		return Port{Kind: PortInternal}, nil
	case "http":
		// Not a hostname generated under the cluster's domain: a name you did
		// not write is a name you have to look up, so there is only the one
		// you write.
		return Port{}, fmt.Errorf(`port "http": name the hostname it is served on instead, e.g. "web.example.com"`)
	case string(PortMetrics):
		return Port{Kind: PortMetrics}, nil
	}

	// A protocol form is "<proto>" or "<proto>:<hostport>". Split first so a
	// malformed protocol reports as a protocol error rather than being
	// silently accepted as a hostname.
	protoPart, portPart, hasPort := strings.Cut(raw, ":")
	switch strings.ToLower(protoPart) {
	case "tcp", "udp":
		b := Bind{Proto: strings.ToLower(protoPart)}
		if hasPort {
			port, err := strconv.Atoi(strings.TrimSpace(portPart))
			if err != nil {
				return Port{}, fmt.Errorf("port %q: %q is not a port number", raw, portPart)
			}
			if port < 1 || port > 65535 {
				return Port{}, fmt.Errorf("port %q: host port %d is outside 1-65535", raw, port)
			}
			b.Host = port
		}
		return Port{Kind: PortRaw, Binds: []Bind{b}}, nil
	}

	// A colon that did not introduce a valid protocol is a typo, not a
	// hostname: no hostname has one.
	if hasPort {
		return Port{}, fmt.Errorf(`port %q: unknown protocol %q; use "tcp" or "udp"`, raw, protoPart)
	}

	return Port{Kind: PortDomain, Domain: strings.ToLower(raw)}, nil
}

// String renders the port back to its authored form, so a manifest round-trips
// to the text it was written as.
func (p Port) String() string {
	switch p.Kind {
	case PortInternal:
		return string(PortInternal)
	case PortMetrics:
		return string(PortMetrics)
	case PortDomain:
		return p.Domain
	case PortRaw:
		parts := make([]string, 0, len(p.Binds))
		for _, b := range p.Binds {
			if b.Host != 0 {
				parts = append(parts, fmt.Sprintf("%s:%d", b.Proto, b.Host))
				continue
			}
			parts = append(parts, b.Proto)
		}
		if len(parts) == 1 {
			return parts[0]
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return ""
}

// MarshalYAML writes the scalar form, or the list form when a raw port binds
// more than one protocol. orca serializes resolved manifests to hash desired
// state, so what comes back has to be what went in.
func (p Port) MarshalYAML() (any, error) {
	if p.Kind == PortRaw && len(p.Binds) > 1 {
		out := make([]string, 0, len(p.Binds))
		for _, b := range p.Binds {
			if b.Host != 0 {
				out = append(out, fmt.Sprintf("%s:%d", b.Proto, b.Host))
				continue
			}
			out = append(out, b.Proto)
		}
		return out, nil
	}
	return p.String(), nil
}

// sortedPorts returns a port map's keys in ascending order. Map iteration is
// random and the generated jobspec has to be byte-stable, or every apply would
// look like a change and redeploy everything.
func sortedPorts(m map[int]Port) []int {
	out := make([]int, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}
