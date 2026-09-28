#!/usr/bin/env bash
#
# orca docker install: pinned static binaries, our own systemd units.
#
# Log rotation is capped here, since unbounded container logs are the single
# most common cause of a self-hosted box filling its disk.

set -e
set -o pipefail

if [ "$EUID" -ne 0 ]; then
  echo "Error: run this script as root." >&2
  exit 1
fi

DOCKER_VERSION="{{DOCKER_VERSION}}"
DATA_DIR="{{DATA_DIR}}"

# CHANGED tracks whether anything that requires a daemon restart moved.
#
# Restarting dockerd kills every running container (live-restore does not
# survive a unit restart), so an unconditional restart means re-running
# bootstrap takes down every workload on the machine. It has also been seen to
# leave stale CNI address reservations behind, so allocations come back and then
# fail with a duplicate-address error. Bootstrap must be safe to re-run on a
# live machine, which makes this conditional load-bearing rather than a nicety.
CHANGED=0

# write_if_changed installs stdin at $1 only when the content differs, and sets
# CHANGED when it does. It always succeeds, so it is safe under `set -e`.
write_if_changed() {
  local dest="$1" tmp
  tmp=$(mktemp)
  cat > "$tmp"
  if [ -f "$dest" ] && cmp -s "$tmp" "$dest"; then
    rm -f "$tmp"
    return 0
  fi
  install -m 0644 "$tmp" "$dest"
  rm -f "$tmp"
  CHANGED=1
  echo "updated ${dest}"
  return 0
}

echo "=== 1. Installing Docker ${DOCKER_VERSION} ==="
# From the official static tarball rather than the apt repo, so a blanket
# `apt-get upgrade` can never bump the engine out from under running workloads.
# The version is controlled entirely by versions.go.
if docker --version 2>/dev/null | grep -qF "Docker version ${DOCKER_VERSION}"; then
  echo "docker ${DOCKER_VERSION} already installed, skipping download."
else
  DOCKER_TMP=$(mktemp -d)
  curl -fsSL "https://download.docker.com/linux/static/stable/x86_64/docker-${DOCKER_VERSION}.tgz" -o "${DOCKER_TMP}/docker.tgz"
  # The file is checked against the sum pinned beside its version, so what is
  # installed as root is what was reviewed, not whatever the URL serves today.
  echo "{{DOCKER_SHA256}}  ${DOCKER_TMP}/docker.tgz" | sha256sum -c --quiet
  tar -xzf "${DOCKER_TMP}/docker.tgz" -C "${DOCKER_TMP}"
  install -m 0755 "${DOCKER_TMP}"/docker/* /usr/local/bin/
  rm -rf "${DOCKER_TMP}"
  CHANGED=1
fi

groupadd -f docker

echo "=== 2. Writing systemd units ==="
write_if_changed /etc/systemd/system/containerd.service <<'EOF'
[Unit]
Description=containerd container runtime
Documentation=https://containerd.io
After=network.target local-fs.target

[Service]
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/containerd
Type=notify
Delegate=yes
KillMode=process
Restart=always
RestartSec=5
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=1048576
TasksMax=infinity
OOMScoreAdjust=-999

[Install]
WantedBy=multi-user.target
EOF

write_if_changed /etc/systemd/system/docker.socket <<'EOF'
[Unit]
Description=Docker Socket for the API

[Socket]
ListenStream=/run/docker.sock
SocketMode=0660
SocketUser=root
SocketGroup=docker

[Install]
WantedBy=sockets.target
EOF

write_if_changed /etc/systemd/system/docker.service <<'EOF'
[Unit]
Description=Docker Application Container Engine
Documentation=https://docs.docker.com
After=network-online.target docker.socket containerd.service
Wants=network-online.target
Requires=docker.socket containerd.service

[Service]
Type=notify
ExecStart=/usr/local/bin/dockerd -H fd:// --containerd=/run/containerd/containerd.sock
ExecReload=/bin/kill -s HUP $MAINPID
TimeoutStartSec=0
RestartSec=2
Restart=always
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
Delegate=yes
KillMode=process
OOMScoreAdjust=-500

[Install]
WantedBy=multi-user.target
EOF

echo "=== 3. Configuring the daemon ==="
# log-opts is the important line. Docker's default json-file driver grows
# without bound; one chatty container can fill the disk and take down every
# other workload plus the database next to it. 10m x 3 per container caps the
# blast radius. Logs you actually keep are shipped to VictoriaLogs, which has
# its own disk ceiling; these files are only the local ring buffer.
#
# live-restore keeps containers running across a dockerd reload, so a config
# change that does not need a restart does not bounce every workload.
mkdir -p /etc/docker
write_if_changed /etc/docker/daemon.json <<EOF
{
  "data-root": "${DATA_DIR}/docker",
  "storage-driver": "overlay2",
  "live-restore": true,
  "log-driver": "json-file",
  "log-opts": {
    "max-size": "10m",
    "max-file": "3"
  }
}
EOF

# Fail safe if the data directory's mount is missing rather than silently
# recreating the data-root on the root filesystem.
mkdir -p /etc/systemd/system/docker.service.d
write_if_changed /etc/systemd/system/docker.service.d/10-orca-data-root.conf <<EOF
[Unit]
RequiresMountsFor=${DATA_DIR}
EOF

echo "=== 4. Starting Docker ==="
systemctl daemon-reload
systemctl enable containerd docker.socket docker >/dev/null 2>&1

if ! systemctl is-active --quiet docker; then
  echo "docker is not running, starting it."
  systemctl restart containerd
  systemctl restart docker
elif [ "${CHANGED}" = "1" ]; then
  echo "docker configuration changed, restarting."
  systemctl restart containerd
  systemctl restart docker
else
  echo "docker configuration unchanged, leaving running containers alone."
fi

docker --version
echo "=== Docker install complete ==="
