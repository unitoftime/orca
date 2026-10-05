package deploy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/internal/manifest"
)

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
		Name:      CatalogName(manifest.ReservedGroup, "victoriametrics"),
		Tags:      []string{DNSTag(manifest.ReservedGroup, "victoriametrics")},
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
			CatalogName(manifest.ReservedGroup, "victorialogs"))
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

var nodeExporterName = CatalogName(manifest.ReservedGroup, "node-exporter")

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
		Name:      CatalogName(manifest.ReservedGroup, "status"),
		Tags:      []string{DNSTag(manifest.ReservedGroup, "status")},
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
			CatalogName(manifest.ReservedGroup, "victoriametrics"), StatusEnvMetrics)
	}
	if opts.Logs != nil {
		fmt.Fprintf(&b, "{{ range nomadService %q }}%s=http://{{ .Address }}:{{ .Port }}\n{{ end }}",
			CatalogName(manifest.ReservedGroup, "victorialogs"), StatusEnvLogs)
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
		Name:      CatalogName(manifest.ReservedGroup, "victorialogs"),
		Tags:      []string{DNSTag(manifest.ReservedGroup, "victorialogs")},
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
		Name:      CatalogName(manifest.ReservedGroup, "vector"),
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
`, LogsPort, CatalogName(manifest.ReservedGroup, "victorialogs"))
}
