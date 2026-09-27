#!/usr/bin/env bash
#
# orca nomad install: pinned static binary, our own systemd unit.
#
# No mutual TLS: at one node Nomad binds loopback and is not reachable from
# anywhere; above one it binds a private network and nothing else.

set -e
set -o pipefail

if [ "$EUID" -ne 0 ]; then
  echo "Error: run this script as root." >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NOMAD_VERSION="{{NOMAD_VERSION}}"
DATA_DIR="{{DATA_DIR}}"

echo "=== 1. Installing Nomad ${NOMAD_VERSION} ==="
# From the upstream release zip rather than the HashiCorp apt repo, for the same
# reason as docker: `apt-get upgrade` must never move the scheduler.
if nomad version 2>/dev/null | grep -qF "Nomad v${NOMAD_VERSION}"; then
  echo "nomad ${NOMAD_VERSION} already installed, skipping download."
else
  NOMAD_TMP=$(mktemp -d)
  curl -fsSL "https://releases.hashicorp.com/nomad/${NOMAD_VERSION}/nomad_${NOMAD_VERSION}_linux_amd64.zip" -o "${NOMAD_TMP}/nomad.zip"
  unzip -oq "${NOMAD_TMP}/nomad.zip" -d "${NOMAD_TMP}"
  install -m 0755 "${NOMAD_TMP}/nomad" /usr/local/bin/nomad
  rm -rf "${NOMAD_TMP}"
fi

echo "=== 2. Installing CNI reference plugins ${CNI_VERSION} ==="
# Nomad's bridge network mode is implemented with the upstream CNI plugins.
# Without them every allocation fails placement on the constraint
# ${attr.plugins.cni.version.bridge} and nothing is ever scheduled, with no
# error anywhere except the job's placement failure.
CNI_VERSION="{{CNI_VERSION}}"
if [ -x /opt/cni/bin/bridge ] && /opt/cni/bin/bridge 2>&1 | grep -qF "${CNI_VERSION#v}"; then
  echo "cni plugins ${CNI_VERSION} already installed, skipping download."
else
  mkdir -p /opt/cni/bin
  CNI_TMP=$(mktemp -d)
  curl -fsSL "https://github.com/containernetworking/plugins/releases/download/${CNI_VERSION}/cni-plugins-linux-amd64-${CNI_VERSION}.tgz" -o "${CNI_TMP}/cni.tgz"
  tar -xzf "${CNI_TMP}/cni.tgz" -C /opt/cni/bin
  rm -rf "${CNI_TMP}"
fi

echo "=== 3. Resolving bind address ==="
# Empty BIND_IP means a single-machine cluster: bind loopback so nothing is
# exposed at all. orca drives Nomad over SSH, so it needs no network listener.
# Once a second node exists, cluster.yaml carries each node's ip and that address
# is bound instead.
BIND_IP="{{BIND_IP}}"
if [ -z "${BIND_IP}" ]; then
  BIND_IP="127.0.0.1"
  echo "single-node cluster: binding Nomad to loopback (${BIND_IP})"
else
  echo "binding Nomad to ${BIND_IP}"
fi

echo "=== 4. Creating platform volume directories ==="
# These back the platform's stateful jobs, which bind-mount them. Created now
# so the first deploy does not have Docker create them as root on demand.
for v in victorialogs victoriametrics traefik vector; do
  mkdir -p "${DATA_DIR}/volumes/${v}"
done

echo "=== 5. Writing Nomad configuration and the registry credential helper ==="
mkdir -p /etc/nomad.d "${DATA_DIR}/nomad"
# Bind HTTP on loopback plus the cluster address, deduplicated so the
# single-machine case does not list 127.0.0.1 twice.
HTTP_ADDRS="127.0.0.1"
if [ "${BIND_IP}" != "127.0.0.1" ]; then
  HTTP_ADDRS="127.0.0.1 ${BIND_IP}"
fi

# The public interface is the one holding the default route. It is resolved
# here rather than written in cluster.yaml: orca never needs to know the
# address, only which interface not to put its own services on.
PUBLIC_IFACE=$(ip route get 1.1.1.1 2>/dev/null | grep -oP 'dev \K\S+' | head -1)
if [ -z "${PUBLIC_IFACE}" ]; then
  echo "ERROR: could not resolve the default-route interface" >&2
  exit 1
fi
echo "public interface: ${PUBLIC_IFACE}"

# Everything the cluster runs for itself binds this. On one machine it is the
# container bridge, which every container can reach and nothing outside can.
# Once there is a cluster it is the private NIC, which additionally reaches the
# other machines. Neither is ever the public interface.
INTERNAL_IFACE="nomad"

if [ "${BIND_IP}" != "127.0.0.1" ]; then
  PRIVATE_IFACE=$(ip -o -4 addr show | awk -v ip="${BIND_IP}" '$4 ~ "^"ip"/" {print $2; exit}')
  if [ -z "${PRIVATE_IFACE}" ]; then
    echo "ERROR: no interface on this machine holds the private address ${BIND_IP}" >&2
    exit 1
  fi
  if [ "${PRIVATE_IFACE}" = "${PUBLIC_IFACE}" ]; then
    echo "ERROR: private_ip ${BIND_IP} is on ${PRIVATE_IFACE}, which is also the default-route interface." >&2
    echo "       orca will not bind the cluster to a public interface. The machines need a separate private network." >&2
    exit 1
  fi
  echo "private interface: ${PRIVATE_IFACE} (${BIND_IP})"
  INTERNAL_IFACE="${PRIVATE_IFACE}"
fi

# A lone server whose address changes has to be told so. Raft keeps the
# address every server had, and a second machine moves the first from loopback
# to its private address. Nomad still elects the one server leader (raft
# counts its vote by ID), but the old address stays in the raft configuration,
# and every reconcile then tries to replace it by removing the only voter,
# which raft refuses: an error every ten seconds, for as long as it runs.
# peers.json is Nomad's own recovery file: read once at the next start, it
# replaces the configuration and is deleted.
#
# One server only. With several, peers.json has to list them all with the
# addresses each will have, written on every server while all are stopped,
# which is not something one machine's bootstrap can do alone.
OLD_RPC=$(grep -oP '^\s*rpc\s*=\s*"\K[^"]+' /etc/nomad.d/nomad.hcl 2>/dev/null || true)
RAFT_DIR="${DATA_DIR}/nomad/server/raft"
if [ "{{SERVER_COUNT}}" = "1" ] && [ "{{ROLE}}" = "server" ] \
  && [ -n "${OLD_RPC}" ] && [ "${OLD_RPC}" != "${BIND_IP}" ] \
  && [ -f "${RAFT_DIR}/raft.db" ] && [ -s "${DATA_DIR}/nomad/server/node-id" ]; then
  SERVER_ID=$(cat "${DATA_DIR}/nomad/server/node-id")
  printf '[{"id": "%s", "address": "%s:4647", "non_voter": false}]\n' "${SERVER_ID}" "${BIND_IP}" > "${RAFT_DIR}/peers.json"
  echo "server address ${OLD_RPC} -> ${BIND_IP}: wrote ${RAFT_DIR}/peers.json"
fi

install -m 0640 "${SCRIPT_DIR}/nomad.hcl" /etc/nomad.d/nomad.hcl
sed -i "s|__BIND_IP__|${BIND_IP}|g" /etc/nomad.d/nomad.hcl
sed -i "s|__HTTP_ADDRS__|${HTTP_ADDRS}|g" /etc/nomad.d/nomad.hcl
sed -i "s|__PUBLIC_IFACE__|${PUBLIC_IFACE}|g" /etc/nomad.d/nomad.hcl
sed -i "s|__INTERNAL_IFACE__|${INTERNAL_IFACE}|g" /etc/nomad.d/nomad.hcl

# Nomad runs it before every image pull, so it is in place before the agent
# that will call it is restarted with the config naming it.
install -m 0755 "${SCRIPT_DIR}/docker-credential-orca" /usr/local/bin/docker-credential-orca

echo "=== 6. Writing systemd unit ==="
# Nomad runs as root: it drives the Docker socket and manages cgroups.
# OOMScoreAdjust=-1000 keeps the scheduler itself from being the thing the
# kernel kills when a workload goes berserk.
cat > /etc/systemd/system/nomad.service <<'EOF'
[Unit]
Description=Nomad
Documentation=https://www.nomadproject.io/
Wants=network-online.target
After=network-online.target docker.service

[Service]
User=root
Group=root
ExecReload=/bin/kill -HUP $MAINPID
ExecStart=/usr/local/bin/nomad agent -config /etc/nomad.d
KillMode=process
KillSignal=SIGINT
LimitNOFILE=65536
LimitNPROC=infinity
Restart=on-failure
RestartSec=2
TasksMax=infinity
OOMScoreAdjust=-1000
Delegate=cpu cpuset io memory pids

[Install]
WantedBy=multi-user.target
EOF

echo "=== 7. Starting Nomad ==="
systemctl daemon-reload
systemctl enable nomad
systemctl restart nomad

echo "=== Nomad install complete ==="
