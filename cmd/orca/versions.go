package main

// Stack pins the exact version of every third-party binary orca installs on
// a node: bump a field, rebuild orca, re-run `orca bootstrap`, and the node
// converges. The container images orca runs are pinned the same way, in
// pkg/images.
//
// Versions live in code rather than cluster.yaml on purpose: they travel with the
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
}

// Versions is the pinned stack this build of orca installs.
var Versions = Stack{
	Nomad:      "2.0.3",  // https://releases.hashicorp.com/nomad/
	Docker:     "29.6.1", // https://download.docker.com/linux/static/stable/x86_64/
	CNIPlugins: "v1.9.1", // https://github.com/containernetworking/plugins/releases
}
