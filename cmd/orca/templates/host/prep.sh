#!/usr/bin/env bash
#
# orca host prep: packages, kernel settings, and the one data directory.
#
# Deliberately minimal: orca runs code you wrote, so there is no tenant
# isolation to set up (no project quotas, KVM modules or custom CNI).

set -e
set -o pipefail

if [ "$EUID" -ne 0 ]; then
  echo "Error: run this script as root." >&2
  exit 1
fi

DATA_DIR="{{DATA_DIR}}"

echo "=== 1. Updating system packages ==="
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get -y -o Dpkg::Options::="--force-confdef" -o Dpkg::Options::="--force-confold" upgrade

echo "=== 2. Installing utilities ==="
apt-get install -y -o Dpkg::Options::="--force-confdef" -o Dpkg::Options::="--force-confold" \
  curl ca-certificates gnupg wget rsync jq unzip iptables iproute2 nftables

echo "=== 3. Unattended security updates ==="
# The boring-OS half of the stability story: security patches land on their own
# so the box does not rot, but only from the security pocket, so a routine
# upgrade never swaps out something running underneath the cluster. Docker and
# Nomad are pinned static binaries installed outside apt, so they are immune to
# this by construction.
apt-get install -y unattended-upgrades
cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF

echo "=== 4. Kernel parameters ==="
# IPv4 forwarding is required for container egress masquerading. The rest is
# ordinary network hardening; nothing here is load-bearing for orca itself.
cat > /etc/sysctl.d/99-orca.conf <<'EOF'
net.ipv4.ip_forward=1
net.ipv4.tcp_syncookies=1
net.ipv4.conf.all.accept_redirects=0
net.ipv4.conf.default.accept_redirects=0
net.ipv4.conf.all.send_redirects=0
net.ipv4.conf.default.send_redirects=0
net.ipv4.conf.all.accept_source_route=0
net.ipv4.conf.default.accept_source_route=0
net.ipv6.conf.all.accept_redirects=0
net.ipv6.conf.all.accept_source_route=0
kernel.kptr_restrict=2
kernel.dmesg_restrict=1
EOF
# Only our file, not --system: that reprocesses every file in /etc/sysctl.d and
# a single unrelated bad entry (a stale setting, a missing module) would fail
# the whole bootstrap under set -e.
sysctl -q -p /etc/sysctl.d/99-orca.conf

echo "=== 5. Verifying cgroups v2 ==="
if [ -e /sys/fs/cgroup/cgroup.controllers ]; then
  echo "cgroups v2 unified hierarchy active."
else
  echo "ERROR: cgroups v2 is not active. Nomad's memory limits depend on it." >&2
  exit 1
fi

echo "=== 6. Creating the data directory ==="
# Everything orca persists lives under one path: docker's data-root and every
# host volume. One path means "how full is the box" has exactly one answer, and
# moving the whole install to a bigger disk is one mount.
mkdir -p "${DATA_DIR}/docker" "${DATA_DIR}/volumes"
chmod 0711 "${DATA_DIR}"

echo "=== 7. Ensuring the container bridge exists ==="
# The cluster resolver binds this bridge's gateway: the one address every
# container on the machine can reach and nothing outside it can, and one that
# does not move when the resolver restarts.
#
# Nomad creates the bridge when its first bridge-mode allocation runs, so on a
# fresh machine the resolver would crash-loop until something else happened to
# start, and on a machine running nothing else, indefinitely. Creating it up
# front makes that deterministic; Nomad's CNI reuses a bridge that already
# exists. The address is Nomad's own default for this bridge.
#
# A unit, run at every boot, because a bridge made with `ip link` does not
# survive a reboot. Making it only here is not enough: a machine running only
# host-networked jobs (orca's own, say) would come back from a reboot with no
# bridge and a resolver failing on every start, since nothing there ever asks
# CNI for one.
cat > /etc/systemd/system/orca-bridge.service <<'EOF'
[Unit]
Description=orca container bridge
DefaultDependencies=no
After=network-pre.target
Before=network.target docker.service nomad.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'ip link show nomad >/dev/null 2>&1 || ip link add name nomad type bridge; ip addr replace 172.26.64.1/20 dev nomad; ip link set nomad up'

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable orca-bridge >/dev/null 2>&1
systemctl restart orca-bridge
ip -brief addr show nomad

echo "=== 8. Installing the firewall unit ==="
# The ruleset itself is written by `orca apply`, because what is open is
# derived from the manifests. This unit only re-applies whatever was last
# written, so the rules survive a reboot. With no ruleset yet it does nothing.
cat > /etc/systemd/system/orca-firewall.service <<'EOF'
[Unit]
Description=orca firewall
DefaultDependencies=no
After=network-pre.target
Wants=network-pre.target
Before=network.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c '[ -f /etc/orca/firewall.nft ] && exec /usr/sbin/nft -f /etc/orca/firewall.nft || exit 0'

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable orca-firewall >/dev/null 2>&1 || true

echo "=== Host prep complete ==="
