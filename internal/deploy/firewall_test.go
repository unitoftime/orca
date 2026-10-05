package deploy

import (
	"strings"
	"testing"

	"github.com/unitoftime/orca/internal/manifest"
)

func rules(t *testing.T, ports FirewallPorts) string {
	t.Helper()
	return InboundFirewall(InboundSpec{PublicIface: "eth0", SSHPorts: "22", Ports: ports}).Rules
}

// A firewall that can lock you out of the machine it protects is a worse
// outage than the one it prevents. These two rules are the reason that cannot
// happen, and they must come before any drop.
func TestSSHAndEstablishedComeFirst(t *testing.T) {
	out := rules(t, FirewallPorts{})

	ssh := strings.Index(out, "tcp dport { 22 } accept")
	established := strings.Index(out, "ct state established,related accept")
	drop := strings.Index(out, "\n    drop\n")

	switch {
	case ssh < 0:
		t.Fatal("ssh is never allowed")
	case established < 0:
		t.Fatal("established traffic is never allowed")
	case drop < 0:
		t.Fatal("nothing is ever dropped, so this is not a firewall")
	case established > drop || ssh > drop:
		t.Error("ssh and established must be accepted before anything is dropped")
	}
}

// Published container ports are destination-NAT'd and traverse forward, never
// input. A ruleset hooked only on input looks right and blocks none of them.
func TestFiltersInPrerouting(t *testing.T) {
	out := rules(t, FirewallPorts{})
	if !strings.Contains(out, "hook prerouting") {
		t.Errorf("public filtering must happen before destination NAT:\n%s", out)
	}
}

func TestDeclaredPortsAreAllowed(t *testing.T) {
	out := rules(t, FirewallPorts{TCP: []int{443, 80, 7777, 80}, UDP: []int{7777}})

	// Deduplicated and ordered, so an unchanged cluster renders an unchanged
	// ruleset.
	if !strings.Contains(out, "tcp dport { 80, 443, 7777 } accept") {
		t.Errorf("tcp ports wrong:\n%s", out)
	}
	if !strings.Contains(out, "udp dport { 7777 } accept") {
		t.Errorf("udp ports wrong:\n%s", out)
	}
}

func TestNoDeclaredPortsStillWorks(t *testing.T) {
	out := rules(t, FirewallPorts{})
	if strings.Contains(out, "dport {  }") {
		t.Errorf("an empty port set must not render an empty rule:\n%s", out)
	}
}

// The one hole the binding rules cannot close: above one machine the scheduler
// binds the private network, which a container reaches through the bridge.
func TestContainersCannotReachTheScheduler(t *testing.T) {
	out := SchedulerFirewall("nomad", nil).Rules
	if !strings.Contains(out, `iifname "nomad" tcp dport { 4646, 4647, 4648 } drop`) {
		t.Errorf("containers must not reach the scheduler:\n%s", out)
	}
}

// Traffic from the bridge is not this chain's business, or a machine's
// containers could not talk to each other.
func TestOnlyOutsideTrafficIsFiltered(t *testing.T) {
	out := rules(t, FirewallPorts{})
	if !strings.Contains(out, `iifname != "eth0" accept`) {
		t.Errorf("internal traffic must pass:\n%s", out)
	}
}

// Above one machine an internal port is published on the private network,
// which is rarely only the cluster's. The cluster's machines pass, and
// everything else there is outside.
func TestPrivateNetworkAnswersOnlyTheCluster(t *testing.T) {
	out := InboundFirewall(InboundSpec{PublicIface: "eth0", PrivateIface: "ens10", SSHPorts: "22",
		Peers: []string{"10.0.0.1", "10.0.0.2"}}).Rules

	outside := strings.Index(out, `iifname != { "eth0", "ens10" } accept`)
	peers := strings.Index(out, `iifname "ens10" ip saddr { 10.0.0.1, 10.0.0.2 } accept`)
	drop := strings.Index(out, "\n    drop\n")
	if outside < 0 || peers < outside || drop < peers {
		t.Errorf("the private interface should pass the cluster's machines and filter the rest:\n%s", out)
	}
}

// Its own table, deleted and recreated rather than a global flush, so Docker's
// and the CNI's rules survive.
func TestDoesNotFlushOtherRules(t *testing.T) {
	out := rules(t, FirewallPorts{})
	if strings.Contains(out, "flush ruleset") {
		t.Errorf("a global flush would take out the container datapath:\n%s", out)
	}
	if !strings.Contains(out, "delete table inet orca") {
		t.Errorf("the table should be replaced wholesale:\n%s", out)
	}
}

func TestPublicPortsFromManifests(t *testing.T) {
	got := PublicPorts([]manifest.HostPort{
		{Port: 7777, Proto: "tcp"},
		{Port: 7777, Proto: "udp"},
		{Port: 8080, Proto: "tcp"},
	}, true)

	want := []int{80, 443, 7777, 8080}
	if len(got.TCP) != len(want) {
		t.Fatalf("tcp = %v, want %v", got.TCP, want)
	}
	for i := range want {
		if got.TCP[i] != want[i] {
			t.Errorf("tcp = %v, want %v", got.TCP, want)
			break
		}
	}
	if len(got.UDP) != 1 || got.UDP[0] != 7777 {
		t.Errorf("udp = %v, want [7777]", got.UDP)
	}
}

// With ingress off, nothing should open 80 and 443.
func TestIngressPortsOnlyWhenIngressRuns(t *testing.T) {
	got := PublicPorts(nil, false)
	if len(got.TCP) != 0 {
		t.Errorf("tcp = %v, want nothing opened", got.TCP)
	}
}

// A container reaching another machine's scheduler is routed through forward,
// never input. Guarding only input would leave every other machine's Nomad open.
func TestSchedulerIsClosedToContainersOnBothPaths(t *testing.T) {
	rules := SchedulerFirewall("nomad", nil).Rules
	for _, hook := range []string{"hook input", "hook forward"} {
		i := strings.Index(rules, hook)
		if i < 0 {
			t.Fatalf("no %s chain:\n%s", hook, rules)
		}
		chain := rules[i:]
		chain = chain[:strings.Index(chain, "\n  }")]
		if !strings.Contains(chain, `iifname "nomad" tcp dport { 4646, 4647, 4648 } drop`) {
			t.Errorf("%s does not close the scheduler to containers:\n%s", hook, chain)
		}
	}
}

// The ports Nomad's machines talk to each other on ask for no token, so the
// private network reaching them could join the cluster. They answer the
// cluster's machines, and that does not depend on the inbound rules at all.
func TestSchedulerAnswersOnlyTheCluster(t *testing.T) {
	multi := SchedulerFirewall("nomad", []string{"10.0.0.1", "10.0.0.2"}).Rules
	if !strings.Contains(multi, `iifname != "lo" ip saddr != { 10.0.0.1, 10.0.0.2 } tcp dport { 4646, 4647, 4648 } drop`) {
		t.Errorf("the scheduler should refuse all but the cluster's machines:\n%s", multi)
	}

	off := NoInboundFirewall().Rules
	if strings.Contains(off, "chain") {
		t.Errorf("firewall: false leaves what the machine answers alone:\n%s", off)
	}
}
