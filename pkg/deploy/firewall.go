package deploy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/unitoftime/orca/pkg/manifest"
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

func dedupe(ports []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, p := range ports {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Ints(out)
	return out
}

// NomadPorts are the scheduler's own ports: HTTP API, RPC and Serf.
var NomadPorts = []int{NomadHTTPPort, 4647, NomadSerfPort}

// NomadHTTPPort is Nomad's API, which also serves its telemetry.
const NomadHTTPPort = 4646

// NomadSerfPort is Serf's gossip port, which answers on udp as well as tcp.
const NomadSerfPort = 4648

// FirewallSpec is what a machine's ruleset is rendered from.
type FirewallSpec struct {
	// PublicIface is the interface facing the internet, BridgeIface the
	// containers' bridge.
	PublicIface string
	BridgeIface string

	// Public is what the public interface answers beyond the essentials. Nil
	// leaves the public interface alone, for `firewall: false`: the rules
	// that keep the scheduler closed are rendered either way, because
	// Nomad's own configuration relies on them.
	Public *FirewallPorts

	// Peers are the private addresses of the cluster's machines, the only
	// addresses besides the machine itself the scheduler answers.
	Peers []string
}

// Firewall renders the machine's nftables ruleset.
//
// The policy is that the public interface answers only what a manifest asked
// for. Everything orca runs for itself binds the internal network and is
// therefore already unreachable from outside; this is defence in depth, for
// the case where something binds a public address by mistake.
//
// Filtering happens in prerouting rather than input because published
// container ports are destination-NAT'd by the CNI and then traverse the
// forward path, never input. An input-only ruleset looks correct and blocks
// none of them. At this hook the packet still carries the port it was sent to,
// before any rewriting, which is exactly what the rules are written against.
func Firewall(spec FirewallSpec) string {
	var b strings.Builder

	b.WriteString(`#!/usr/sbin/nft -f
# Managed by orca. Regenerated on every apply; edits are lost.
#
# Its own table, deleted and recreated rather than a global flush, so the rules
# Docker and the CNI maintain are left alone.

table inet orca
delete table inet orca

table inet orca {
`)

	if spec.Public != nil {
		publicChain(&b, spec.PublicIface, *spec.Public)
	}

	// The scheduler has no ACLs, so whoever can reach it can run anything on
	// every machine as root. It binds loopback on one machine and the private
	// network above one, and the private network is rarely only the cluster's:
	// every device on a tailnet, or every tenant of a provider's network, is on
	// it too. So it answers the cluster's own machines and nobody else.
	nomad := joinPorts(NomadPorts)
	strangers := ""
	if len(spec.Peers) > 0 {
		strangers = fmt.Sprintf("ip saddr != { %s } ", strings.Join(spec.Peers, ", "))
	}

	// Containers are the one hole the peer rule cannot close. Their traffic
	// to another machine leaves masqueraded, looking like this machine, which
	// is a peer. Nothing that legitimately talks to Nomad does so from inside
	// a container: the agent renders templates itself, and ingress runs on
	// the host's network.
	//
	// Two hooks, because there are two paths. A container reaching its own
	// machine's scheduler arrives on input; one reaching another machine's is
	// routed out through forward and never touches input here.
	fmt.Fprintf(&b, `
  chain host {
    type filter hook input priority filter; policy accept;

    # Containers may reach the resolver and the platform's stores by name, but
    # never the scheduler. A container that could submit jobs could run
    # anything on the machine as root.
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
}
`, spec.BridgeIface, nomad, NomadSerfPort, strangers)

	return b.String()
}

// publicChain filters what arrives on the public interface.
func publicChain(b *strings.Builder, iface string, ports FirewallPorts) {
	fmt.Fprintf(b, `  chain public {
    type filter hook prerouting priority -150; policy accept;

    # Anything not arriving from outside is not this chain's business: the
    # container bridge, the private network, loopback.
    iifname != %q accept

    # Replies to connections this machine made, and to connections already
    # allowed by the rules below. Placed first so a rule change can never cut
    # off the session applying it.
    ct state established,related accept
    ct state invalid drop

    # SSH, unconditionally. A firewall that can lock you out of the machine it
    # is protecting is a worse outage than the one it prevents.
    tcp dport 22 accept

    # Ping, and the ICMP that path-MTU discovery and connection errors depend
    # on. Dropping it does not buy security and does break things subtly.
    meta l4proto { icmp, ipv6-icmp } accept

    # DHCP replies, for a machine that leases its address.
    udp sport 67 udp dport 68 accept
    udp sport 547 udp dport 546 accept
`, iface)

	if tcp := dedupe(ports.TCP); len(tcp) > 0 {
		fmt.Fprintf(b, "\n    # A manifest's raw tcp/udp ports, and ingress.\n")
		fmt.Fprintf(b, "    tcp dport { %s } accept\n", joinPorts(tcp))
	}
	if udp := dedupe(ports.UDP); len(udp) > 0 {
		fmt.Fprintf(b, "    udp dport { %s } accept\n", joinPorts(udp))
	}

	b.WriteString(`
    # Everything else arriving from outside.
    drop
  }
`)
}

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
