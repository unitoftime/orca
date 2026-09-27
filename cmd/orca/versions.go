package main

// Stack pins the exact version of every third-party component orca installs on
// a node. It is the single source of truth for "what does this cluster run":
// bump a field, rebuild orca, re-run `orca bootstrap`, and the node converges.
//
// Versions live in code rather than cluster.yaml on purpose — they travel with the
// binary and an upgrade is a reviewable commit, not a per-box config edit.
//
// Each comment is the browse URL for finding newer releases. Mind the version
// string format: projects disagree about the leading "v" and the download URLs
// depend on it.
type Stack struct {
	Nomad string // https://releases.hashicorp.com/nomad/                   (no "v")

	Docker string // https://download.docker.com/linux/static/stable/x86_64/ (no "v")

	// CNIPlugins are the upstream reference plugins (bridge, firewall,
	// portmap, host-local, loopback). Nomad's bridge network mode is built on
	// them: without them every allocation fails placement on the constraint
	// ${attr.plugins.cni.version.bridge}, and nothing can ever be scheduled.
	//
	// These are the plain upstream plugins, not a replacement CNI like Cilium:
	// orca needs no network identity beyond Nomad's own bridge.
	CNIPlugins string // https://github.com/containernetworking/plugins/releases (with "v")

	// Platform images. These are containers orca runs for you, pinned for the
	// same reason as everything else: an upgrade should be a reviewable commit,
	// never something that happens because a tag moved.
	// Rclone is the S3 client backups use. Chosen over MinIO's mc because it
	// speaks every S3 dialect, configures entirely from environment variables
	// with no config file, and is still published where it can be pulled —
	// mc's images have disappeared from both Docker Hub and quay.io.
	//
	// Alpine is the image the status page's orca binary is mounted into, until
	// orca publishes an image of its own.
	Rclone          string // https://hub.docker.com/r/rclone/rclone/tags  (no "v")
	Alpine          string // https://hub.docker.com/_/alpine/tags                         (no "v")
	CoreDNS         string // https://hub.docker.com/r/coredns/coredns/tags                (no "v")
	NodeExporter    string // https://hub.docker.com/r/prom/node-exporter/tags             (with "v")
	Traefik         string // https://hub.docker.com/_/traefik                            (with "v")
	Vector          string // https://hub.docker.com/r/timberio/vector/tags               (no "v")
	VictoriaLogs    string // https://hub.docker.com/r/victoriametrics/victoria-logs/tags (with "v")
	VictoriaMetrics string // https://hub.docker.com/r/victoriametrics/victoria-metrics/tags (with "v")
}

// Versions is the pinned stack this build of orca installs.
var Versions = Stack{
	Nomad:      "2.0.3",  // https://releases.hashicorp.com/nomad/
	Docker:     "29.6.1", // https://download.docker.com/linux/static/stable/x86_64/
	CNIPlugins: "v1.9.1", // https://github.com/containernetworking/plugins/releases

	Rclone:          "1.71.0",
	Alpine:          "3.24.2",
	CoreDNS:         "1.12.4",
	NodeExporter:    "v1.12.1",
	Traefik:         "v3.6.25",
	Vector:          "0.51.0",
	VictoriaLogs:    "v1.52.0",
	VictoriaMetrics: "v1.152.0",
}
