package deploy

import (
	"fmt"
	"slices"
	"strings"

	"github.com/unitoftime/orca/internal/manifest"
)

// FirewallPorts is what the machine is allowed to answer on its public
// interface, beyond the fixed essentials.
type FirewallPorts struct {
	TCP []int
	UDP []int
}

// Add records a port claim.
func (f *FirewallPorts) Add(proto string, port int) {
	switch proto {
	case "tcp":
		f.TCP = append(f.TCP, port)
	case "udp":
		f.UDP = append(f.UDP, port)
	}
}

// NomadPorts are the scheduler's own ports: HTTP API, RPC and Serf.
var NomadPorts = []int{NomadHTTPPort, 4647, NomadSerfPort}

// NomadHTTPPort is Nomad's API, which also serves its telemetry.
const NomadHTTPPort = 4646

// NomadSerfPort is Serf's gossip port, which answers on udp as well as tcp.
const NomadSerfPort = 4648

// dedupe is the ports in order with no repeats, so an unchanged cluster
// renders an unchanged ruleset.
func dedupe(ports []int) []int {
	return slices.Compact(slices.Sorted(slices.Values(ports)))
}

// Ruleset is one nftables table orca keeps on a machine. Each is its own
// table, deleted and recreated rather than a global flush, so the rules
// Docker and the CNI maintain are left alone.
type Ruleset struct {
	// Name is what the machine keeps it as, to load again at boot.
	Name  string
	Table string
	Rules string
}

// There are two, because they are written from different things at different
// times: the scheduler's from cluster.yaml alone, as soon as a machine is
// bootstrapped, and the inbound one from the service files, by apply.
func ruleset(name, table, chains string) Ruleset {
	return Ruleset{Name: name, Table: table, Rules: fmt.Sprintf(`#!/usr/sbin/nft -f
# Managed by orca. Regenerated on every apply; edits are lost.

table inet %[1]s
delete table inet %[1]s

table inet %[1]s {
%[2]s}
`, table, chains)}
}

// SchedulerFirewall renders the rules that keep Nomad's ports to the
// cluster's own machines. peers are their private addresses, none on a single
// machine.
//
// Nomad's API asks for a token, but the ports its machines talk to each other
// on do not, and whatever joins them is handed work to run. Nomad binds
// loopback on one machine and the private network above one, and the private
// network is rarely only the cluster's: every device on a tailnet, or every
// tenant of a provider's network, is on it too. So those ports answer the
// cluster's own machines and nobody else, from before Nomad first starts:
// bootstrap writes this, and with `firewall: false` it is still written.
func SchedulerFirewall(bridgeIface string, peers []string) Ruleset {
	nomad := joinPorts(NomadPorts)
	strangers := ""
	if len(peers) > 0 {
		strangers = fmt.Sprintf("ip saddr != { %s } ", strings.Join(peers, ", "))
	}

	// Containers are the one hole the peer rule cannot close. Their traffic
	// to another machine leaves masqueraded, looking like this machine, which
	// is a peer. Nothing that legitimately talks to Nomad does so from inside
	// a container: the agent renders templates itself, and what does call the
	// API runs on the host's network.
	//
	// Two hooks, because there are two paths. A container reaching its own
	// machine's scheduler arrives on input; one reaching another machine's is
	// routed out through forward and never touches input here.
	return ruleset("scheduler", "orca-scheduler", fmt.Sprintf(`  chain host {
    type filter hook input priority filter; policy accept;

    # Containers may reach the resolver and the platform's stores by name, but
    # never the scheduler.
    iifname %[1]q tcp dport { %[2]s } drop
    iifname %[1]q udp dport %[3]d drop

    # Nor may anything else but this machine and the cluster's others.
    iifname != "lo" %[4]stcp dport { %[2]s } drop
    iifname != "lo" %[4]sudp dport %[3]d drop
    iifname != "lo" meta nfproto ipv6 tcp dport { %[2]s } drop
    iifname != "lo" meta nfproto ipv6 udp dport %[3]d drop
  }

  chain forward {
    type filter hook forward priority filter; policy accept;

    # The same rule for every other machine's scheduler.
    iifname %[1]q tcp dport { %[2]s } drop
    iifname %[1]q udp dport %[3]d drop
  }
`, bridgeIface, nomad, NomadSerfPort, strangers))
}

// InboundSpec is what a machine answers from outside the cluster.
type InboundSpec struct {
	// PublicIface is the interface facing the internet. PrivateIface is the
	// one the cluster's machines reach each other on, empty on a single
	// machine, and Peers are their addresses on it.
	PublicIface  string
	PrivateIface string
	Peers        []string

	// SSHPorts is where the machine's sshd answers, as nftables writes a set
	// of ports.
	SSHPorts string

	// Ports is what a manifest asked to be reachable, and ingress.
	Ports FirewallPorts
}

// InboundFirewall renders what a machine answers from outside the cluster:
// only what a manifest asked for.
//
// Outside is the internet, and it is also everything on the private network
// that is not one of the cluster's machines. An `internal` port is published
// on that network once there is more than one machine, so that the others
// can reach it, and without this it would answer every device on a tailnet
// and every other tenant of a provider's network as well.
//
// Filtering happens in prerouting rather than input because published
// container ports are destination-NAT'd by the CNI and then traverse the
// forward path, never input. An input-only ruleset looks correct and blocks
// none of them. At this hook the packet still carries the port it was sent to,
// before any rewriting, which is exactly what the rules are written against.
func InboundFirewall(spec InboundSpec) Ruleset {
	var b strings.Builder

	outside := fmt.Sprintf("%q", spec.PublicIface)
	if spec.PrivateIface != "" {
		outside = fmt.Sprintf("{ %q, %q }", spec.PublicIface, spec.PrivateIface)
	}
	fmt.Fprintf(&b, `  chain inbound {
    type filter hook prerouting priority -150; policy accept;

    # Anything not arriving from outside is not this chain's business: the
    # container bridge, loopback.
    iifname != %s accept
`, outside)

	if spec.PrivateIface != "" {
		fmt.Fprintf(&b, `
    # The cluster's own machines, which is how services on different ones
    # reach each other.
    iifname %q ip saddr { %s } accept
`, spec.PrivateIface, strings.Join(spec.Peers, ", "))
	}

	fmt.Fprintf(&b, `
    # Replies to connections this machine made, and to connections already
    # allowed by the rules below. Placed first so a rule change can never cut
    # off the session applying it.
    ct state established,related accept
    ct state invalid drop

    # SSH, unconditionally, on the ports this machine's sshd answers. A
    # firewall that can lock you out of the machine it is protecting is a
    # worse outage than the one it prevents.
    tcp dport { %s } accept

    # Ping, and the ICMP that path-MTU discovery and connection errors depend
    # on. Dropping it does not buy security and does break things subtly.
    meta l4proto { icmp, ipv6-icmp } accept

    # DHCP replies, for a machine that leases its address.
    udp sport 67 udp dport 68 accept
    udp sport 547 udp dport 546 accept
`, spec.SSHPorts)

	if tcp := dedupe(spec.Ports.TCP); len(tcp) > 0 {
		fmt.Fprintf(&b, "\n    # A manifest's raw tcp/udp ports, and ingress.\n")
		fmt.Fprintf(&b, "    tcp dport { %s } accept\n", joinPorts(tcp))
	}
	if udp := dedupe(spec.Ports.UDP); len(udp) > 0 {
		fmt.Fprintf(&b, "    udp dport { %s } accept\n", joinPorts(udp))
	}

	b.WriteString(`
    # Everything else arriving from outside.
    drop
  }
`)
	return ruleset(inboundName, inboundTable, b.String())
}

// NoInboundFirewall is the inbound table with nothing in it, for
// `firewall: false`: what the machine answers is then yours to decide.
func NoInboundFirewall() Ruleset { return ruleset(inboundName, inboundTable, "") }

const (
	inboundName  = "firewall"
	inboundTable = "orca"
)

func joinPorts(ports []int) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, fmt.Sprint(p))
	}
	return strings.Join(parts, ", ")
}

// PublicPorts collects every port the cluster's manifests ask to be reachable
// from outside: raw tcp/udp ports, plus ingress if it is running.
func PublicPorts(hostPorts []manifest.HostPort, ingress bool) FirewallPorts {
	var out FirewallPorts
	for _, hp := range hostPorts {
		out.Add(hp.Proto, hp.Port)
	}
	if ingress {
		out.Add("tcp", 80)
		out.Add("tcp", 443)
	}
	out.TCP = dedupe(out.TCP)
	out.UDP = dedupe(out.UDP)
	return out
}
