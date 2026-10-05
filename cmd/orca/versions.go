package main

// Stack pins the exact version of every third-party binary orca installs on
// a node: bump a field, rebuild orca, re-run `orca bootstrap`, and the node
// converges. The container images orca runs are pinned the same way, in
// internal/images.
//
// Versions live in code rather than cluster.yaml on purpose: they travel with the
// binary and an upgrade is a reviewable commit, not a per-box config edit.
//
// Each comment is the browse URL for finding newer releases. Mind the version
// string format: projects disagree about the leading "v" and the download URLs
// depend on it.
//
// Each download is pinned by its SHA256 too, checked on the machine before
// anything is installed. Bumping a version means bumping its sum: Nomad and
// the CNI plugins publish theirs beside the release; Docker does not, so
// take `curl -fsSL <url> | sha256sum`.
type Stack struct {
	Nomad       string // https://releases.hashicorp.com/nomad/                   (no "v")
	NomadSHA256 string // of nomad_<version>_linux_amd64.zip

	Docker       string // https://download.docker.com/linux/static/stable/x86_64/ (no "v")
	DockerSHA256 string // of docker-<version>.tgz

	// CNIPlugins are the upstream reference plugins (bridge, firewall,
	// portmap, host-local, loopback). Nomad's bridge network mode is built on
	// them: without them every allocation fails placement on the constraint
	// ${attr.plugins.cni.version.bridge}, and nothing can ever be scheduled.
	//
	// These are the plain upstream plugins, not a replacement CNI like Cilium:
	// orca needs no network identity beyond Nomad's own bridge.
	CNIPlugins       string // https://github.com/containernetworking/plugins/releases (with "v")
	CNIPluginsSHA256 string // of cni-plugins-linux-amd64-<version>.tgz
}

// versions is the pinned stack this build of orca installs.
var versions = Stack{
	Nomad:       "2.0.3",
	NomadSHA256: "8455d5691de4cb451e9443282f1c0171570b480737fc6386992638c52a4795e4",

	Docker:       "29.6.1",
	DockerSHA256: "b0df4a43a98d7ecb708acbdb5a34a3416e13b6e39bcbbdf296f51f0f3442b29f",

	CNIPlugins:       "v1.9.1",
	CNIPluginsSHA256: "b98f74a0f8522f0a83867178729c1aa70f2158f90c45a2ca8fa791db1c76b303",
}
