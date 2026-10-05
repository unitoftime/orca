package deploy

import (
	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/internal/manifest"
)

// OrcaServices are those services that register an address.
//
// One list serves the resolver's name list, the log command's target list
// and the jobs themselves. With three separate lists, two would be updated
// when a component was added and the third would silently miss it.
// vector is absent deliberately: it registers no port, so there is no address
// to resolve or to filter logs by.
var OrcaServices = []string{"traefik", "victorialogs", "victoriametrics", "node-exporter", "status", "certs", "dns"}

// Well-known ports for the platform's data stores. They are fixed rather than
// dynamic so a store keeps its address across restarts, and they bind the
// internal host network (the container bridge, or the private address above
// one machine), so a fixed port is not a fixed hole in the machine. Everything
// that talks to them finds the address through the catalog.
const (
	LogsPort    = 9428
	MetricsPort = 8428
)

// PlatformOptions is the resolved platform configuration. A nil capability is
// one that is turned off and will not be deployed.
type PlatformOptions struct {
	Datacenter string
	DataDir    string

	// Domain is where the dashboards are published: status.<domain> and the
	// rest. Empty publishes none.
	Domain string

	// MultiNode is whether the cluster spans more than one machine. Each
	// machine's Nomad agent reports its own allocations, and above one
	// machine those agents are reached on their private addresses rather
	// than loopback.
	MultiNode bool

	// MonitoringNode and IngressNode are the machines the platform's pinned
	// jobs run on: the log and metric stores and the status page on one,
	// ingress on the other (the same machine unless cluster.yaml separates
	// them). Pinned because each owns data on one disk: without it a
	// reschedule would start a store against an empty directory and report
	// it healthy while serving nothing. The jobs on every machine (the
	// resolver, the log shipper, the node exporter) are pinned to neither.
	MonitoringNode string
	IngressNode    string

	// StoreNetwork is the host network the platform's own stores bind. Always
	// "internal": the container bridge on one machine, the private NIC once
	// there is a cluster. Never "public".
	StoreNetwork string

	Images PlatformImages

	Ingress *IngressSpec
	Logs    *LogsSpec
	Metrics *MetricsSpec
	Status  *StatusSpec
	Certs   *CertsSpec

	// DNS is the resolver. Enabled means services find each other by name;
	// disabled means they have no way to, so it is on unless deliberately
	// turned off.
	DNS *DNSSpec
}

// DNSSpec is the resolver. It has no settings: the names it answers come from
// the catalog, by the tag every registration carries.
type DNSSpec struct{}

// PlatformImages pins what the platform runs.
type PlatformImages struct {
	CoreDNS         string
	NodeExporter    string
	Traefik         string
	Vector          string
	VictoriaLogs    string
	VictoriaMetrics string
}

type IngressSpec struct {
	// TLS is whether ingress gets certificates from Let's Encrypt and serves
	// HTTPS, redirecting plain HTTP to it.
	TLS bool

	// ACMEEmail is the optional contact on the Let's Encrypt account. It
	// does not decide whether there are certificates: Let's Encrypt issues
	// them to an account with no contact at all.
	ACMEEmail string

	// AuthHash is a bcrypt hash of the admin password. Empty means the
	// platform dashboards are not published at all. This fails closed, because
	// the alternative is putting your logs on the internet behind nothing.
	AuthHash string
}

// StatusSpec is the status page: every machine and service at a glance, and
// the API `orca top` reads.
//
// It is orca itself, run as `orca serve-status`. Image is what the task runs
// in. Binary, when set, is the path on the host of an orca binary apply
// shipped there, mounted in as /orca. That is how orca gets onto the machine
// until it publishes an image of its own. An image of its own would carry orca
// at /orca, and Binary would simply be empty: the job is otherwise the same.
type StatusSpec struct {
	Image  string
	Binary string
}

// CertsSpec is the certificate job: what gets the certificates services ask
// for with `tls:` and keeps them renewed. Like the status page it is orca
// itself, run as `orca serve-certs`; Image and Binary mean what they do there.
type CertsSpec struct {
	Image  string
	Binary string

	// Directory is the certificate authority's ACME directory. Empty is
	// Let's Encrypt.
	Directory string
}

// StatusBinaryDir is where apply ships orca's own binary for the status page.
func StatusBinaryDir(dataDir string) string { return dataDir + "/bin" }

// StatusBinaryPath is where one build of orca lives on the host, named for its
// contents. A new build is a new path, so the job spec changes with it and
// nothing that is running ever has its binary replaced underneath it; and the
// previous path is still there for Nomad to revert to if the new one fails.
func StatusBinaryPath(dataDir, sha256hex string) string {
	return StatusBinaryDir(dataDir) + "/orca-" + sha256hex[:16]
}

type LogsSpec struct {
	Retention string

	// DiskBytes is a plain byte count. The size is resolved before it gets
	// here because these flags reject "10G" (they want a bare number or a
	// decimal suffix like "10GB"), and a rejected flag is a container that
	// exits 2 with its usage text on stdout and nothing on stderr, which is a
	// genuinely hard failure to read.
	DiskBytes int64
}

type MetricsSpec struct {
	Retention string

	// MinFreeBytes is a plain byte count, for the same reason as DiskBytes.
	MinFreeBytes int64
}

// BuildPlatform renders the platform jobs that are switched on.
func BuildPlatform(opts PlatformOptions) []*nomad.Job {
	var jobs []*nomad.Job

	if opts.DNS != nil {
		jobs = append(jobs, dnsJob(opts))
	}

	if opts.Metrics != nil {
		// Host metrics go with the store: they are metrics, and have nowhere
		// else to go.
		jobs = append(jobs, metricsJob(opts), nodeExporterJob(opts))
	}
	if opts.Logs != nil {
		// The store before the shipper: Vector holds logs on disk while its
		// sink is unavailable, so the order is not load-bearing, but there is
		// no reason to start producing before there is anywhere to put them.
		jobs = append(jobs, logsJob(opts), vectorJob(opts))
	}
	if opts.Status != nil {
		jobs = append(jobs, statusJob(opts))
	}
	if opts.Ingress != nil {
		// Only with ingress: the authority checks a name on port 80, and
		// ingress is what holds it.
		if opts.Certs != nil {
			jobs = append(jobs, certsJob(opts))
		}
		jobs = append(jobs, ingressJob(opts))
	}
	return jobs
}

// smallJobMemoryMB is what each of orca's small jobs claims: the resolver, the
// host metrics exporter, the status page and the certificate job. Each sits
// between 10 and 20M in use, so this is a few times that. It is kept small
// because a claim is a reservation as well as a cap: on a 4G machine four of
// these at a generous size were memory nothing else could be given.
const smallJobMemoryMB = 48

// platformJob builds the shared skeleton every platform job has.
func platformJob(opts PlatformOptions, name, image string, cpu, memMB int) (*nomad.Job, *nomad.TaskGroup, *nomad.Task) {
	id := JobID(manifest.ReservedGroup, name)

	task := &nomad.Task{
		Name:   name,
		Driver: "docker",
		Config: map[string]any{"image": image},
		Resources: &nomad.Resources{
			CPU:      ptr(cpu),
			MemoryMB: ptr(memMB),
		},
	}

	group := &nomad.TaskGroup{
		Name:  ptr(name),
		Count: ptr(1),
		Tasks: []*nomad.Task{task},
		RestartPolicy: &nomad.RestartPolicy{
			Attempts: ptr(3),
			Interval: durPtr("5m"),
			Delay:    durPtr("15s"),
			Mode:     ptr("delay"),
		},
	}

	job := &nomad.Job{
		ID:          ptr(id),
		Name:        ptr(id),
		Type:        ptr("service"),
		Datacenters: []string{opts.Datacenter},
		// Above the default 50, so a platform job wins a scheduling race
		// against a service of yours. Losing the log store to a busy service would take away
		// the thing you need to find out why.
		Priority:   ptr(60),
		TaskGroups: []*nomad.TaskGroup{group},
		Meta: map[string]string{
			MetaManaged: "true",
			MetaGroup:   manifest.ReservedGroup,
			MetaService: name,
			MetaImage:   image,
		},
		Update: &nomad.UpdateStrategy{
			MaxParallel:     ptr(1),
			MinHealthyTime:  durPtr("10s"),
			HealthyDeadline: durPtr("5m"),
			AutoRevert:      ptr(true),
			HealthCheck:     ptr("checks"),
		},
	}

	// A second machine turns the "internal" host network from the container
	// bridge into the private NIC, but a running allocation keeps the address
	// it was placed with, and a spec that did not change is never resubmitted.
	// So without this, the stores would stay on the first machine's bridge
	// address after it joined a cluster, and every other machine would resolve
	// that address to its own bridge: logs, metrics and ingress quietly
	// reaching nothing.
	// Stamped only above one machine, so a one-machine cluster's specs are
	// exactly what they were.
	if opts.MultiNode {
		job.Meta[MetaNetwork] = "private"
	}

	return job, group, task
}

// pinTo keeps a group on one machine. Empty leaves it to Nomad.
func pinTo(group *nomad.TaskGroup, node string) {
	if node == "" {
		return
	}
	group.Constraints = append(group.Constraints, &nomad.Constraint{
		LTarget: "${node.unique.name}",
		RTarget: node,
		Operand: "=",
	})
}

// storePort binds a well-known port on whichever private network the platform
// stores use. The host network is named rather than an address being written
// in, so the job spec says what it may be reached from and Nomad enforces it.
func storePort(opts PlatformOptions, label string, port int) *nomad.NetworkResource {
	return &nomad.NetworkResource{
		Mode: "host",
		ReservedPorts: []nomad.Port{{
			Label:       label,
			Value:       port,
			To:          port,
			HostNetwork: storeNetwork(opts),
		}},
	}
}

func storeNetwork(opts PlatformOptions) string {
	if opts.StoreNetwork == "" {
		return "internal"
	}
	return opts.StoreNetwork
}
