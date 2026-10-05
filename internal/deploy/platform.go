package deploy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/internal/manifest"
)

// OrcaApp is the group the jobs orca runs for you are filed under: ingress,
// the resolver, the log and metric stores. They are ordinary Nomad jobs
// carrying ordinary orca metadata, so plan, apply, status and the health wait
// all work on them with no special cases. They are simply an app you did not
// write, and they are named so you can see that.
const OrcaApp = manifest.ReservedGroup

// OrcaServices are those services that register an address.
//
// One list serves the resolver's name list, the log command's target list
// and the jobs themselves. With three separate lists, two would be updated
// when a component was added and the third would silently miss it.
// vector is absent deliberately: it registers no port, so there is no address
// to resolve or to filter logs by.
var OrcaServices = []string{"traefik", "victorialogs", "victoriametrics", "node-exporter", "status", "certs", "dns"}

// Ingress's entrypoints and certificate resolver, named once. The routes an
// app carries in its catalog tags must name exactly what Traefik's own
// configuration defines, or Traefik drops the route.
const (
	EntryPointHTTP  = "http"
	EntryPointHTTPS = "https"
)

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
	id := JobID(OrcaApp, name)

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
		// against an app. Losing the log store to a busy service would take away
		// the thing you need to find out why.
		Priority:   ptr(60),
		TaskGroups: []*nomad.TaskGroup{group},
		Meta: map[string]string{
			MetaManaged: "true",
			MetaApp:     OrcaApp,
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

func metricsJob(opts PlatformOptions) *nomad.Job {
	job, group, task := platformJob(opts, "victoriametrics", opts.Images.VictoriaMetrics, 200, 512)
	pinTo(group, opts.MonitoringNode)

	group.Networks = []*nomad.NetworkResource{storePort(opts, "http", MetricsPort)}
	task.Config["network_mode"] = "host"
	task.Config["volumes"] = []string{PlatformVolumePath(opts.DataDir, "victoriametrics") + ":/storage"}
	task.Config["args"] = []string{
		"-storageDataPath=/storage",
		// The address comes from the port Nomad bound, so it follows the host
		// network in the job spec instead of being written twice and able to
		// disagree with it.
		"-httpListenAddr=${NOMAD_ADDR_http}",
		"-retentionPeriod=" + opts.Metrics.Retention,
		// VictoriaMetrics has no byte ceiling the way the log store does, so
		// this is the backstop: it stops accepting data rather than consuming
		// the last of the disk and taking the database down with it.
		fmt.Sprintf("-storage.minFreeDiskSpaceBytes=%d", opts.Metrics.MinFreeBytes),
		"-promscrape.config=/local/scrape.yml",
	}

	task.Templates = append(task.Templates, &nomad.Template{
		EmbeddedTmpl: ptr(scrapeConfig(opts)),
		DestPath:     ptr("local/scrape.yml"),
		// VictoriaMetrics re-reads its scrape config on SIGHUP, so the log
		// store moving does not restart the metric store.
		ChangeMode:   ptr("signal"),
		ChangeSignal: ptr("SIGHUP"),
	})

	group.Services = []*nomad.Service{{
		Name:      CatalogName(OrcaApp, "victoriametrics"),
		Tags:      []string{DNSTag(OrcaApp, "victoriametrics")},
		PortLabel: "http",
		Provider:  "nomad",
		Checks: []nomad.ServiceCheck{{
			Type: "http", Path: "/health", Interval: dur("15s"), Timeout: dur("3s"),
		}},
	}}
	return job
}

// scrapeConfig is what VictoriaMetrics scrapes. Nomad's own telemetry already
// reports per-allocation CPU, memory and disk, so one target per machine covers
// every workload on it without a per-service exporter.
//
// The stores are scraped where they actually listen, not at 127.0.0.1: they
// bind the internal network (the container bridge or the private address,
// never loopback), so a loopback target would be permanently down.
// The metric store's own address is the one Nomad bound for it; the log
// store's comes from the catalog, since it may be on another machine.
func scrapeConfig(opts PlatformOptions) string {
	logs := opts.Logs != nil

	var b strings.Builder
	if logs {
		fmt.Fprintf(&b, `{{ $logs := "" }}{{ range nomadService %q }}{{ $logs = printf "%%s:%%d" .Address .Port }}{{ end }}`+"\n",
			CatalogName(OrcaApp, "victorialogs"))
	}
	b.WriteString(`global:
  scrape_interval: 30s
scrape_configs:
`)
	b.WriteString(nomadScrape(opts))
	b.WriteString(`  - job_name: victoriametrics
    static_configs:
      - targets: ['{{ env "NOMAD_ADDR_http" }}']
`)
	if logs {
		b.WriteString(`{{ if $logs }}  - job_name: victorialogs
    static_configs:
      - targets: ['{{ $logs }}']
{{ end }}`)
	}
	b.WriteString(catalogScrape("nodes", nodeExporterName, "", "{{ .Port }}"))
	b.WriteString(catalogScrape("services", MetricsCatalogName, "", "{{ .Port }}"))
	return b.String()
}

var nodeExporterName = CatalogName(OrcaApp, "node-exporter")

// nomadMetrics is where Nomad serves its telemetry, and the parameter that
// makes it Prometheus-formatted rather than JSON.
const nomadMetrics = `    metrics_path: /v1/metrics
    params:
      format: ['prometheus']
`

// nomadScrape scrapes every machine's Nomad agent. Each agent reports only
// the allocations running on its own machine, so scraping any one of them
// alone would silently leave out every service on the others.
//
// On one machine the agent answers on loopback only. Above one, it also
// answers on the machine's private address, which is the address that
// machine's node exporter registers at: the exporter binds the internal
// network, and above one machine that is the private interface. So the
// exporters' registrations are the list of machines, and a machine added later
// is scraped with no change here. Either way each agent's series carry the
// machine's node label, so an allocation's usage says where it ran.
func nomadScrape(opts PlatformOptions) string {
	if opts.MultiNode {
		return catalogScrape("nomad", nodeExporterName, nomadMetrics, strconv.Itoa(NomadHTTPPort))
	}
	return fmt.Sprintf(`  - job_name: nomad
%s    static_configs:
      - targets: ['127.0.0.1:%d']
        labels:
          node: %q
`, nomadMetrics, NomadHTTPPort, opts.MonitoringNode)
}

// catalogScrape scrapes every registration of one catalog name: the ports
// manifests declared as `metrics`, and the node exporter on each machine.
//
// Rendered from the catalog, like the log store's address above, so the target
// list follows services and machines as they come, go and move, and a change
// reaches the store as a SIGHUP rather than a restart. Each target's tags
// become its labels: a service's group and service, and a machine's node name.
// An app's prod and test servers export the same metric names, and group and
// service are what keep them apart.
//
// Values are quoted: they are DNS labels, and a group named "123" or "yes"
// would otherwise reach VictoriaMetrics as a number or a boolean.
//
// head is any settings the job needs beyond its targets, and port is the
// port to scrape at each registered address: normally the registered port,
// but Nomad is scraped at its own.
func catalogScrape(job, catalogName, head, port string) string {
	return fmt.Sprintf(`{{ with nomadService %q }}  - job_name: %s
%s    static_configs:
{{ range . }}      - targets: ['{{ .Address }}:%s']
        labels:
{{ range .Tags }}{{ $kv := . | split "=" }}          {{ index $kv 0 }}: "{{ index $kv 1 }}"
{{ end }}{{ end }}{{ end }}`, catalogName, job, head, port)
}

// NodeTag is the tag each machine's exporter registers with, and so the label
// its series carry: the node's name, as cluster.yaml gives it. Nomad fills it
// in per machine when it registers the service.
const NodeTag = "node=${node.unique.name}"

// nodeExporterJob reports each machine's own CPU, memory, disks and network.
// Nomad's telemetry covers what each allocation uses; this is the machine
// underneath them, which is what actually fills up.
//
// A system job, like vector, so every machine reports, including one added
// later, with no change here. It reads the host's /proc, /sys and mounts, which
// is why it is part of the platform rather than something a manifest could
// declare.
func nodeExporterJob(opts PlatformOptions) *nomad.Job {
	job, group, task := platformJob(opts, "node-exporter", opts.Images.NodeExporter, 100, smallJobMemoryMB)

	job.Type = ptr("system")

	// A dynamic port rather than a well-known one. Above one machine a
	// service's internal port takes its number on every machine, and this
	// runs on every machine, so a fixed port would be one number no service
	// could use anywhere. The metric store finds it through the catalog and
	// never needs to know the number.
	group.Networks = []*nomad.NetworkResource{{
		Mode:         "host",
		DynamicPorts: []nomad.Port{{Label: "http", HostNetwork: storeNetwork(opts)}},
	}}

	// The host's network and process namespaces, and its root read-only: the
	// numbers are the machine's, not the container's.
	task.Config["network_mode"] = "host"
	task.Config["pid_mode"] = "host"
	task.Config["volumes"] = []string{"/:/host:ro,rslave"}
	task.Config["args"] = []string{
		"--path.rootfs=/host",
		// Not covered by rootfs: without it the disk collector logs an error
		// on every start and names disks without their udev properties.
		"--path.udev.data=/host/run/udev/data",
		"--web.listen-address=${NOMAD_ADDR_http}",
		// Nomad mounts a secrets tmpfs and bind mounts into every allocation
		// directory. Each would be a filesystem series of its own, named after
		// an allocation id, repeating the disk it lives on.
		"--collector.filesystem.mount-points-exclude=" + nodeMountExclude(opts.DataDir),
		// Every container has a veth pair, renamed on every deploy. Kept, they
		// would be a new set of series per deploy for the whole retention
		// period, none of which says anything the machine's own interface
		// does not.
		"--collector.netdev.device-exclude=^(veth.*|lo)$",
		"--collector.netclass.ignored-devices=^veth.*$",
		// What bootstrap's package hook writes: whether an installed update
		// is waiting for a reboot.
		"--collector.textfile.directory=/host/run/orca/textfile",
	}

	// No DNS name: this registers once per machine, so a name would answer
	// with all of them. Its tags become its series' labels instead.
	group.Services = []*nomad.Service{{
		Name:      nodeExporterName,
		Tags:      []string{NodeTag},
		PortLabel: "http",
		Provider:  "nomad",
		Checks: []nomad.ServiceCheck{{
			Type: "http", Path: "/", Interval: dur("15s"), Timeout: dur("3s"),
		}},
	}}
	return job
}

// nodeMountExclude is node exporter's default mount exclusion, plus Nomad's
// allocation directories.
func nodeMountExclude(dataDir string) string {
	nomadDir := regexp.QuoteMeta(strings.TrimPrefix(dataDir, "/") + "/nomad")
	return `^/(dev|proc|run/credentials/.+|sys|var/lib/docker/.+|var/lib/containers/storage/.+|` + nomadDir + `/.+)($|/)`
}

// statusJob runs the status page.
//
// Host networking, because it reads Nomad's API, which answers on loopback
// and which the firewall deliberately keeps every container on the bridge
// away from. It only ever reads, and its identity is allowed nothing else. A
// dynamic port on the internal network, found through the catalog by ingress
// and by `orca top`, so it takes no number a service might want.
func statusJob(opts PlatformOptions) *nomad.Job {
	job, group, task := platformJob(opts, "status", opts.Status.Image, 100, smallJobMemoryMB)
	exposeIdentity(task)
	// With the stores, which it reads, and on the machine apply ships orca's
	// binary to (see statusbin.go).
	pinTo(group, opts.MonitoringNode)

	group.Networks = []*nomad.NetworkResource{{
		Mode:         "host",
		DynamicPorts: []nomad.Port{{Label: "http", HostNetwork: storeNetwork(opts)}},
	}}

	task.Config["network_mode"] = "host"
	task.Config["command"] = "/orca"
	args := []string{
		"serve-status",
		"--listen=${NOMAD_ADDR_http}",
		fmt.Sprintf("--nomad=http://127.0.0.1:%d", NomadHTTPPort),
	}
	// The other UIs, for the page to link to. Only those published: with a
	// store switched off, or no domain, there is nothing to link.
	for _, name := range otherDashboards(opts) {
		args = append(args, "--link="+name+"="+dashboardURL(opts, name))
	}
	task.Config["args"] = args
	if opts.Status.Binary != "" {
		task.Config["volumes"] = []string{opts.Status.Binary + ":/orca:ro"}
	}

	// The stores' addresses, from the catalog. A store that moves restarts
	// the page, which holds nothing worth keeping. One that is switched off
	// is simply absent, and the page shows that section as unavailable.
	if env := statusEnv(opts); env != "" {
		task.Templates = append(task.Templates, &nomad.Template{
			EmbeddedTmpl: ptr(env),
			DestPath:     ptr("local/stores.env"),
			Envvars:      ptr(true),
			ChangeMode:   ptr("restart"),
		})
	}

	group.Services = []*nomad.Service{{
		Name:      CatalogName(OrcaApp, "status"),
		Tags:      []string{DNSTag(OrcaApp, "status")},
		PortLabel: "http",
		Provider:  "nomad",
		Checks: []nomad.ServiceCheck{{
			Type: "http", Path: "/healthz", Interval: dur("15s"), Timeout: dur("3s"),
		}},
	}}
	return job
}

// certsJob runs the certificate job.
//
// Host networking for the status page's reason: it reads and writes Nomad's
// variable store, which answers on loopback. On the ingress machine, since
// ingress is what forwards the authority's checks to it and what it checks
// its own route through. It keeps nothing on disk.
func certsJob(opts PlatformOptions) *nomad.Job {
	job, group, task := platformJob(opts, "certs", opts.Certs.Image, 100, smallJobMemoryMB)
	exposeIdentity(task)
	pinTo(group, opts.IngressNode)

	group.Networks = []*nomad.NetworkResource{{
		Mode:         "host",
		DynamicPorts: []nomad.Port{{Label: "http", HostNetwork: storeNetwork(opts)}},
	}}

	task.Config["network_mode"] = "host"
	task.Config["command"] = "/orca"
	args := []string{
		"serve-certs",
		"--listen=${NOMAD_ADDR_http}",
		fmt.Sprintf("--nomad=http://127.0.0.1:%d", NomadHTTPPort),
	}
	if opts.Certs.Directory != "" {
		args = append(args, "--directory="+opts.Certs.Directory)
	}
	if opts.Ingress.ACMEEmail != "" {
		args = append(args, "--email="+opts.Ingress.ACMEEmail)
	}
	task.Config["args"] = args
	if opts.Certs.Binary != "" {
		task.Config["volumes"] = []string{opts.Certs.Binary + ":/orca:ro"}
	}

	group.Services = []*nomad.Service{{
		Name:      CatalogName(OrcaApp, "certs"),
		Tags:      []string{DNSTag(OrcaApp, "certs")},
		PortLabel: "http",
		Provider:  "nomad",
		Checks: []nomad.ServiceCheck{{
			Type: "http", Path: "/healthz", Interval: dur("15s"), Timeout: dur("3s"),
		}},
	}}
	return job
}

// Environment variables the status page finds the stores by.
const (
	StatusEnvMetrics = "ORCA_METRICS_URL"
	StatusEnvLogs    = "ORCA_LOGS_URL"
)

func statusEnv(opts PlatformOptions) string {
	var b strings.Builder
	if opts.Metrics != nil {
		fmt.Fprintf(&b, "{{ range nomadService %q }}%s=http://{{ .Address }}:{{ .Port }}\n{{ end }}",
			CatalogName(OrcaApp, "victoriametrics"), StatusEnvMetrics)
	}
	if opts.Logs != nil {
		fmt.Fprintf(&b, "{{ range nomadService %q }}%s=http://{{ .Address }}:{{ .Port }}\n{{ end }}",
			CatalogName(OrcaApp, "victorialogs"), StatusEnvLogs)
	}
	return b.String()
}

func logsJob(opts PlatformOptions) *nomad.Job {
	job, group, task := platformJob(opts, "victorialogs", opts.Images.VictoriaLogs, 200, 512)
	pinTo(group, opts.MonitoringNode)

	group.Networks = []*nomad.NetworkResource{storePort(opts, "http", LogsPort)}
	task.Config["network_mode"] = "host"
	task.Config["volumes"] = []string{PlatformVolumePath(opts.DataDir, "victorialogs") + ":/storage"}
	task.Config["args"] = []string{
		"-storageDataPath=/storage",
		"-httpListenAddr=${NOMAD_ADDR_http}",
		"-retentionPeriod=" + opts.Logs.Retention,
		// The hard ceiling. When it is reached the oldest days are dropped, so
		// logs can never grow into the space the database needs. This single
		// setting is most of orca's answer to "the disk filled up again".
		fmt.Sprintf("-retention.maxDiskSpaceUsageBytes=%d", opts.Logs.DiskBytes),
	}

	group.Services = []*nomad.Service{{
		Name:      CatalogName(OrcaApp, "victorialogs"),
		Tags:      []string{DNSTag(OrcaApp, "victorialogs")},
		PortLabel: "http",
		Provider:  "nomad",
		Checks: []nomad.ServiceCheck{{
			Type: "http", Path: "/health", Interval: dur("15s"), Timeout: dur("3s"),
		}},
	}}
	return job
}

// vectorJob ships every container's logs to the store. It is a system job, so
// Nomad runs exactly one on every machine, now and on any machine added later,
// with no change to this file.
func vectorJob(opts PlatformOptions) *nomad.Job {
	job, group, task := platformJob(opts, "vector", opts.Images.Vector, 200, 256)

	job.Type = ptr("system")

	task.Config["network_mode"] = "host"
	// The Docker socket is how Vector discovers containers and reads their
	// logs, which also gets it the container labels Nomad sets, so a log line
	// arrives already knowing which job and task produced it.
	task.Config["volumes"] = []string{
		"/var/run/docker.sock:/var/run/docker.sock:ro",
		PlatformVolumePath(opts.DataDir, "vector") + ":/vector-data",
	}
	task.Config["args"] = []string{"--config", "/local/vector.yaml"}

	task.Templates = append(task.Templates, &nomad.Template{
		EmbeddedTmpl: ptr(vectorConfig()),
		DestPath:     ptr("local/vector.yaml"),
		ChangeMode:   ptr("restart"),
	})

	group.Services = []*nomad.Service{{
		Name:      CatalogName(OrcaApp, "vector"),
		Provider:  "nomad",
		PortLabel: "",
	}}
	// No port, so no check to gate a rollout on.
	group.Services[0].Checks = nil
	job.Update.HealthCheck = ptr("task_states")

	return job
}

// vectorConfig reads container logs from Docker and writes them to the log
// store.
//
// The disk buffer is the part that matters: if the log store is down or being
// restarted, Vector holds lines on disk and sends them when it returns, rather
// than losing them or blocking. The buffer has its own ceiling, because an
// unbounded buffer is just a slower way to fill the disk.
func vectorConfig() string {
	// The log store's address is resolved from the service catalog, not
	// written as loopback. Vector is a system job running on every machine
	// while the store runs on one, so a hardcoded 127.0.0.1 would silently
	// drop the logs of every machine except that one.
	//
	// The default keeps the config valid before the store has registered;
	// otherwise Vector would crash-loop on unparseable YAML until it did.
	//
	// Each line carries the machine as `node`, the name cluster.yaml gives it
	// and the label every metric series has, so a log line and a graph say
	// the same thing about where. Vector's own `host` is the OS hostname
	// (whatever the provider called the box) and is dropped rather than left
	// to disagree with it. The node is part of the stream: a job on every
	// machine is one source per machine.
	return fmt.Sprintf(`{{ $addr := "127.0.0.1:%d" }}{{ range nomadService "%s" }}{{ $addr = printf "%%s:%%d" .Address .Port }}{{ end }}
data_dir: /vector-data

sources:
  containers:
    type: docker_logs
    exclude_containers:
      - vector

transforms:
  tagged:
    type: remap
    inputs: [containers]
    source: |
      .job = .label."com.hashicorp.nomad.job_name" || "unknown"
      .task = .label."com.hashicorp.nomad.task_name" || "unknown"
      .alloc = .label."com.hashicorp.nomad.alloc_id" || ""
      .node = {{ env "node.unique.name" | toJSON }}
      del(.label)
      del(.host)

sinks:
  victorialogs:
    type: elasticsearch
    inputs: [tagged]
    endpoints: ["http://{{ $addr }}/insert/elasticsearch"]
    mode: bulk
    api_version: v8
    healthcheck:
      enabled: false
    query:
      _msg_field: message
      _time_field: timestamp
      _stream_fields: job,task,node
    buffer:
      type: disk
      max_size: 268435488
      when_full: drop_newest
`, LogsPort, CatalogName(OrcaApp, "victorialogs"))
}

func ingressJob(opts PlatformOptions) *nomad.Job {
	job, group, task := platformJob(opts, "traefik", opts.Images.Traefik, 200, 256)
	// Pinned because it is where DNS points. It keeps nothing on disk: its
	// certificates come from the variable store.
	pinTo(group, opts.IngressNode)

	// Host networking, for two reasons: 80 and 443 have to be the real ports on
	// the real interface, and Traefik reads the Nomad catalog over loopback.
	// The one platform component that binds a public interface, because being
	// the public front door is its entire job.
	group.Networks = []*nomad.NetworkResource{{
		Mode: "host",
		ReservedPorts: []nomad.Port{
			{Label: "http", Value: 80, To: 80, HostNetwork: "public"},
			{Label: "https", Value: 443, To: 443, HostNetwork: "public"},
		},
	}}
	task.Config["network_mode"] = "host"
	task.Config["args"] = []string{"--configFile=/secrets/traefik.yml"}

	// In the task's private tmpfs: it holds the token ingress reads the
	// catalog with.
	exposeIdentity(task)
	task.Templates = append(task.Templates, &nomad.Template{
		EmbeddedTmpl: ptr(traefikConfig(opts)),
		DestPath:     ptr("secrets/traefik.yml"),
		ChangeMode:   ptr("restart"),
	})

	if dyn := traefikDynamicConfig(opts); dyn != "" {
		// Routers and middleware are dynamic configuration, which Traefik will
		// not take from its static file. A second file, served through the file
		// provider, is how the dashboards get published.
		//
		// noop, not restart: the file provider watches it and reloads on its
		// own. It changes whenever a dashboard's backend moves (the status
		// page gets a new port every time orca is upgraded), and restarting
		// ingress for that would drop every HTTP service to re-route one.
		task.Templates = append(task.Templates, &nomad.Template{
			EmbeddedTmpl: ptr(dyn),
			// In the task's private tmpfs: it holds the certificates' keys.
			DestPath:   ptr("secrets/dynamic.yml"),
			ChangeMode: ptr("noop"),
		})
	}

	if opts.Ingress.AuthHash != "" {
		job.Meta[MetaAuth] = opts.Ingress.AuthHash
	}

	group.Services = []*nomad.Service{{
		Name:      CatalogName(OrcaApp, "traefik"),
		Tags:      []string{DNSTag(OrcaApp, "traefik")},
		PortLabel: "http",
		Provider:  "nomad",
		Checks: []nomad.ServiceCheck{{
			Type: "http", Path: "/ping", Interval: dur("15s"), Timeout: dur("3s"),
		}},
	}}
	return job
}

func traefikConfig(opts PlatformOptions) string {
	var b strings.Builder

	// Without an explicit entryPoint, Traefik serves /ping on its internal
	// "traefik" entrypoint, which is not defined here, so the health check
	// would fail forever on a Traefik that is working perfectly.
	fmt.Fprintf(&b, `ping:
  entryPoint: %[1]s

entryPoints:
  %[1]s:
    address: ":80"
`, EntryPointHTTP)

	// With HTTPS off, ingress still routes — it just serves plain HTTP.
	if opts.Ingress.TLS {
		// allowACMEByPass leaves the path a certificate authority checks out
		// of the redirect, so that check reaches the certificate job over
		// plain HTTP, which is how the authority makes it.
		//
		// Ingress asks for no certificates itself. Every route on the HTTPS
		// entrypoint serves whichever of the certificate job's matches the
		// name; see traefikDynamicConfig.
		fmt.Fprintf(&b, `    allowACMEByPass: true
    http:
      redirections:
        entryPoint:
          to: %[1]s
          scheme: https
  %[1]s:
    address: ":443"
    http:
      tls: {}
`, EntryPointHTTPS)
	}

	// watch: routes follow Nomad's event stream instead of a poll every 15s,
	// so a copy leaving the catalog stops getting requests within DrainDelay.
	// The token is the task's own identity, which Nomad renders in here.
	fmt.Fprintf(&b, `
providers:
  nomad:
    exposedByDefault: false
    watch: true
    endpoint:
      address: http://127.0.0.1:%d
      token: {{ env "NOMAD_TOKEN" | toJSON }}
`, NomadHTTPPort)

	if traefikDynamicConfig(opts) != "" {
		// Watched, so a changed route reaches Traefik without a restart.
		b.WriteString(`  file:
    filename: /secrets/dynamic.yml
    watch: true
`)
	}

	return b.String()
}

// dashboardsEnabled reports whether the platform's own web UIs are published.
// A domain to serve them on, a password to put in front of them and HTTPS to
// carry it are all required: over plain HTTP the password crosses the internet
// readable by anyone on the way. Without any of them they stay reachable only
// over SSH, which is how `orca top` and `orca logs` reach them anyway.
func dashboardsEnabled(opts PlatformOptions) bool {
	return opts.Domain != "" && opts.Ingress != nil && opts.Ingress.TLS && opts.Ingress.AuthHash != ""
}

// dashboard is one of the platform's built-in web UIs.
type dashboard struct {
	name string

	// service is the catalog name its backend is resolved from.
	service string

	// fallback is used until the service has registered.
	fallback string

	// internal is a backend with no page of its own to publish.
	internal bool
}

// traefikDynamicConfig is the routing ingress cannot learn from the catalog:
// the platform's web UIs on subdomains, behind basic auth, and the ownership
// checks the certificate job answers.
//
// vmui, the query UI built into both Victoria binaries, is what makes logs
// and metrics explorable in a browser without running Grafana. The status
// page shows what Nomad has placed where.
//
// Nomad's own UI is deliberately not among them. Its UI is its API, and the
// token that opens it can run any container, privileged, on every machine:
// not something to put on the internet behind a login page. It stays
// reachable over an SSH tunnel.
func traefikDynamicConfig(opts PlatformOptions) string {
	boards := dashboards(opts)
	certs := opts.Certs != nil && opts.Ingress != nil && opts.Ingress.TLS
	if len(boards) == 0 && !certs {
		return ""
	}
	if certs {
		// A backend like any dashboard's. Its port is dynamic, so there is
		// no address to fall back to; see the status page's.
		boards = append(boards, dashboard{name: "certs", service: CatalogName(OrcaApp, "certs"), fallback: "http://127.0.0.1:1", internal: true})
	}

	var b strings.Builder

	// Resolve every backend up front. The fallback keeps the file valid before
	// a store has registered, which matters because an unparseable dynamic
	// config would take down the routes that do work alongside it.
	for _, d := range boards {
		fmt.Fprintf(&b, `{{ $%s := %q }}{{ range nomadService %q }}{{ $%s = printf "http://%%s:%%d" .Address .Port }}{{ end }}`+"\n",
			d.name, d.fallback, d.service, d.name)
	}

	b.WriteString("http:\n")
	if dashboardsEnabled(opts) {
		b.WriteString("  middlewares:\n    dashboard-auth:\n      basicAuth:\n        users:\n")
		fmt.Fprintf(&b, "          - %q\n", "admin:"+opts.Ingress.AuthHash)
	}

	b.WriteString("  routers:\n")
	for _, d := range boards {
		if d.internal {
			continue
		}
		fmt.Fprintf(&b, `    %s:
      rule: "Host(`+"`"+`%s.%s`+"`"+`)"
      service: %s
      middlewares: [dashboard-auth]
`, d.name, d.name, opts.Domain, d.name)
		if opts.Ingress.TLS {
			b.WriteString("      tls: {}\n")
		}
	}
	if certs {
		// Every ownership check, for any name, goes to the certificate job:
		// it is the only thing here that asks an authority for anything.
		fmt.Fprintf(&b, `    acme:
      rule: "PathPrefix(`+"`"+`%s`+"`"+`)"
      entryPoints: [%s]
      service: certs
`, ACMEChallengePath, EntryPointHTTP)
	}

	b.WriteString("  services:\n")
	for _, d := range boards {
		fmt.Fprintf(&b, `    %s:
      loadBalancer:
        servers:
          - url: %q
`, d.name, fmt.Sprintf("{{ $%s }}", d.name))
	}

	if certs {
		// Every certificate the cluster holds, rendered from the store, so
		// one that is issued or renewed reaches ingress without a restart and
		// without the certificate job being up: Nomad renders this, and
		// ingress watches the file. A record still waiting for its
		// certificate is skipped. toJSON writes each PEM as one quoted line,
		// which YAML reads back as it was.
		//
		// The section is written only once there is a certificate to put in
		// it. Traefik refuses a file whose tls section is empty, the whole
		// file and not just the section, and the file is also what routes
		// the ownership checks that the first certificate depends on.
		fmt.Fprintf(&b, `{{ $none := true }}{{ range nomadVarList %[1]q }}{{ with nomadVar .Path }}{{ if .%[2]s.Value }}{{ if $none }}{{ $none = false }}tls:
  certificates:
{{ end }}    - certFile: {{ .%[2]s | toJSON }}
      keyFile: {{ .%[3]s | toJSON }}
{{ end }}{{ end }}{{ end }}`, CertPrefix, CertChainKey, CertKeyKey)
	}

	return b.String()
}

// dashboards is the platform's published web UIs, or none when they are not
// published.
func dashboards(opts PlatformOptions) []dashboard {
	if !dashboardsEnabled(opts) {
		return nil
	}

	// Backends are resolved from the catalog rather than assumed to be on
	// loopback. Once the stores bind the private address instead, nothing is
	// listening on 127.0.0.1 for ingress to reach: the catalog is the only
	// thing that knows where they actually are.
	var boards []dashboard
	if opts.Status != nil {
		boards = append(boards, dashboard{
			name:    "status",
			service: CatalogName(OrcaApp, "status"),
			// Its port is dynamic, so there is no address to fall back to.
			// Nothing listens on port 1: a 502 until it registers, which is
			// the truth.
			fallback: "http://127.0.0.1:1",
		})
	}
	if opts.Logs != nil {
		boards = append(boards, dashboard{
			name:     "logs",
			service:  CatalogName(OrcaApp, "victorialogs"),
			fallback: fmt.Sprintf("http://127.0.0.1:%d", LogsPort),
		})
	}
	if opts.Metrics != nil {
		boards = append(boards, dashboard{
			name:     "metrics",
			service:  CatalogName(OrcaApp, "victoriametrics"),
			fallback: fmt.Sprintf("http://127.0.0.1:%d", MetricsPort),
		})
	}
	return boards
}

// DashboardURLs lists where the platform UIs are published, for reporting.
func DashboardURLs(opts PlatformOptions) []string {
	var out []string
	if dashboardsEnabled(opts) && opts.Status != nil {
		out = append(out, dashboardURL(opts, "status"))
	}
	for _, name := range otherDashboards(opts) {
		out = append(out, dashboardURL(opts, name))
	}
	return out
}

// otherDashboards names the published UIs besides the status page, which
// links to each of them.
func otherDashboards(opts PlatformOptions) []string {
	if !dashboardsEnabled(opts) {
		return nil
	}
	var out []string
	if opts.Logs != nil {
		out = append(out, "logs")
	}
	if opts.Metrics != nil {
		out = append(out, "metrics")
	}
	return out
}

func dashboardURL(opts PlatformOptions, name string) string {
	scheme := "http"
	if opts.Ingress.TLS {
		scheme = "https"
	}
	return scheme + "://" + name + "." + opts.Domain
}
