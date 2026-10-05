package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/unitoftime/orca/pkg/deploy"
	"github.com/unitoftime/orca/pkg/manifest"
)

// bridgeIface is the containers' bridge on every machine.
const bridgeIface = "nomad"

// What a ruleset leaves for the machine to fill in, since only the machine
// knows: which interface faces the internet, which holds its private address,
// and where its sshd answers. None of them is in cluster.yaml, because orca
// needs to know which interface to filter and not what it is called.
const (
	publicIfaceSlot  = "__PUBLIC_IFACE__"
	privateIfaceSlot = "__PRIVATE_IFACE__"
	sshPortsSlot     = "__SSH_PORTS__"
)

// InstallRuleset loads one of orca's rulesets on this machine and keeps it to
// load again at boot, and reports whether anything changed. privateIP is the
// machine's own address on the private network, empty when it has none.
//
// The new ruleset is loaded *before* it replaces the file on disk, so one
// nftables rejects never becomes the one applied at boot.
//
// The SSH ports are the ones sshd is configured to answer and the one this
// very session came in on, so the rule that keeps the machine reachable is
// written from how it is actually being reached.
func (n Node) InstallRuleset(ctx context.Context, rs deploy.Ruleset, privateIP string) (bool, error) {
	script := fmt.Sprintf(`set -e
FILE=/etc/orca/%[1]s.nft
mkdir -p /etc/orca
cat > "$FILE.new"
fail() { echo "$1" >&2; rm -f "$FILE.new"; exit 1; }

if grep -q %[3]s "$FILE.new"; then
  PUB=$(ip route get 1.1.1.1 2>/dev/null | grep -oP 'dev \K\S+' | head -1)
  [ -n "$PUB" ] || fail "could not resolve the default-route interface"
  sed -i "s|%[3]s|${PUB}|g" "$FILE.new"
fi
if grep -q %[4]s "$FILE.new"; then
  PRIV=$(ip -o -4 addr show | awk -v ip=%[6]s '$4 ~ "^"ip"/" {print $2; exit}')
  [ -n "$PRIV" ] || fail "no interface on this machine holds its private address"
  sed -i "s|%[4]s|${PRIV}|g" "$FILE.new"
fi
if grep -q %[5]s "$FILE.new"; then
  PORTS=$({ sshd -T 2>/dev/null | awk '$1 == "port" {print $2}'; echo "${SSH_CONNECTION:-}" | awk '{print $4}'; } \
    | grep -E '^[0-9]+$' | sort -un | paste -sd, -)
  sed -i "s|%[5]s|${PORTS:-22}|g" "$FILE.new"
fi

# Unchanged content and a table that is actually loaded means there is nothing
# to do. Checking the table too makes this self-healing: if the rules were
# flushed by hand or lost, they come back on the next apply.
if cmp -s "$FILE.new" "$FILE" && nft list table inet %[2]s >/dev/null 2>&1; then
  rm -f "$FILE.new"
  echo unchanged
  exit 0
fi

nft -f "$FILE.new"
mv "$FILE.new" "$FILE"
echo changed`, rs.Name, rs.Table, publicIfaceSlot, privateIfaceSlot, sshPortsSlot, shQuote(privateIP))

	out, err := n.RunStdin(ctx, script, []byte(rs.Rules))
	if err != nil {
		return false, fmt.Errorf("install the %s rules: %w", rs.Name, err)
	}
	// Compared exactly, not with Contains: "unchanged" contains "changed".
	return strings.TrimSpace(out) == "changed", nil
}

// peers are the private addresses of the cluster's machines, none on a single
// machine.
func peers(cfg Config) []string {
	var out []string
	for _, nc := range cfg.Nodes {
		if nc.PrivateIP != "" {
			out = append(out, nc.PrivateIP)
		}
	}
	return out
}

// schedulerFirewall is the rules that keep Nomad to the cluster's machines.
// Bootstrap installs them on a machine before Nomad starts on it, and apply
// again, since adding a machine changes them on every other.
func schedulerFirewall(cfg Config) deploy.Ruleset {
	return deploy.SchedulerFirewall(bridgeIface, peers(cfg))
}

// applyFirewall makes every machine answer, from outside the cluster, only
// the ports the manifests ask for, and keeps every machine's scheduler to the
// cluster.
//
// Every machine, not only the one orca is talking to: a cluster where one
// machine is firewalled and the rest are open is not a firewalled cluster, and
// the difference is invisible from the machine that happens to be protected.
// A machine that cannot be reached is warned about and passed over rather than
// failing the apply: that is exactly when you need to deploy around it.
//
// The rules are derived from every group, not only the ones in scope, so a
// narrowed apply does not close a port belonging to an app it was told to
// leave alone. Every machine gets the same ports: an unpinned service can be
// placed anywhere, so the union is the only set that is correct wherever it
// lands. A port open on a machine running nothing behind it is reachable by
// nothing.
//
// With `firewall: false` what a machine answers is yours, but the scheduler's
// rules are still installed.
func applyFirewall(ctx context.Context, cfg Config, all []*manifest.Manifest) error {
	inbound := deploy.NoInboundFirewall()
	var ports deploy.FirewallPorts
	if cfg.Firewall.Enabled {
		var hostPorts []manifest.HostPort
		for _, m := range all {
			hostPorts = append(hostPorts, m.HostPorts()...)
		}
		ports = deploy.PublicPorts(hostPorts, cfg.Ingress.Enabled)
		spec := deploy.InboundSpec{PublicIface: publicIfaceSlot, SSHPorts: sshPortsSlot, Peers: peers(cfg), Ports: ports}
		if len(spec.Peers) > 0 {
			spec.PrivateIface = privateIfaceSlot
		}
		inbound = deploy.InboundFirewall(spec)
	}
	scheduler := schedulerFirewall(cfg)

	anyChanged := false
	for _, nc := range cfg.Nodes {
		node := Node{Host: nc.Host}
		for _, rs := range []deploy.Ruleset{scheduler, inbound} {
			changed, err := node.InstallRuleset(ctx, rs, nc.PrivateIP)
			if unreachable(err) {
				fmt.Printf("warning: node %s is unreachable; its firewall was not updated\n", nc.Name)
				break
			}
			if err != nil {
				return fmt.Errorf("node %s: %w", nc.Name, err)
			}
			anyChanged = anyChanged || changed
		}
	}

	if anyChanged && cfg.Firewall.Enabled {
		fmt.Printf("firewall updated: ssh, tcp %v", ports.TCP)
		if len(ports.UDP) > 0 {
			fmt.Printf(", udp %v", ports.UDP)
		}
		fmt.Println()
	}
	return nil
}
