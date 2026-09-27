package deploy

import (
	"regexp"
	"strings"
	"testing"
	"text/template"

	nomad "github.com/hashicorp/nomad/api"
	"gopkg.in/yaml.v3"
)

func platformOpts() PlatformOptions {
	return PlatformOptions{
		Datacenter:     "home",
		DataDir:        "/var/orca",
		Domain:         "example.com",
		MonitoringNode: "box0",
		IngressNode:    "box0",
		Images: PlatformImages{
			Traefik: "traefik:v3.6", Vector: "timberio/vector:0.51.0-alpine",
			VictoriaLogs: "vl:v1", VictoriaMetrics: "vm:v1",
			NodeExporter: "prom/node-exporter:v1",
		},
		StoreNetwork: "internal",
		Ingress:      &IngressSpec{TLS: true, ACMEEmail: "me@example.com", AuthHash: "$2a$10$hash"},
		Logs:         &LogsSpec{Retention: "14d", DiskBytes: 10 << 30},
		Metrics:      &MetricsSpec{Retention: "30d", MinFreeBytes: 2 << 30},
		Status:       &StatusSpec{Image: "alpine:3", Binary: "/var/orca/bin/orca-0123456789abcdef"},
	}
}

func jobNames(opts PlatformOptions) map[string]bool {
	out := map[string]bool{}
	for _, j := range BuildPlatform(opts) {
		out[j.Meta[MetaService]] = true
	}
	return out
}

func TestBuildPlatformAll(t *testing.T) {
	got := jobNames(platformOpts())
	for _, want := range []string{"traefik", "vector", "victorialogs", "victoriametrics", "node-exporter", "status"} {
		if !got[want] {
			t.Errorf("missing job %q", want)
		}
	}
}

// A capability switched off must produce no job at all, so the ordinary "stop
// what is no longer declared" rule removes it from a running cluster.
func TestBuildPlatformOmitsDisabled(t *testing.T) {
	opts := platformOpts()
	opts.Ingress = nil
	opts.Metrics = nil

	got := jobNames(opts)
	for _, off := range []string{"traefik", "victoriametrics", "node-exporter"} {
		if got[off] {
			t.Errorf("%q is disabled but a job was built", off)
		}
	}
	// Logs is a pair: the store and the shipper are one capability.
	for _, on := range []string{"victorialogs", "vector"} {
		if !got[on] {
			t.Errorf("%q should still be built", on)
		}
	}
}

// Vector must run on every machine, now and on any machine added later.
func TestVectorIsASystemJob(t *testing.T) {
	for _, j := range BuildPlatform(platformOpts()) {
		if j.Meta[MetaService] != "vector" {
			continue
		}
		if *j.Type != "system" {
			t.Errorf("vector type = %q, want system", *j.Type)
		}
		if len(j.TaskGroups[0].Constraints) != 0 {
			t.Error("a system job must not be pinned to one machine")
		}
		return
	}
	t.Fatal("no vector job")
}

// Every machine reports on itself, now and on any machine added later, and
// its series say which machine they are.
func TestNodeExporter(t *testing.T) {
	var job *nomad.Job
	for _, j := range BuildPlatform(platformOpts()) {
		if j.Meta[MetaService] == "node-exporter" {
			job = j
		}
	}
	if job == nil {
		t.Fatal("no node-exporter job")
	}
	group := job.TaskGroups[0]
	if *job.Type != "system" || len(group.Constraints) != 0 {
		t.Errorf("want an unpinned system job, got type %q constraints %v", *job.Type, group.Constraints)
	}

	// Dynamic: a fixed port on every machine is a number no service could
	// use anywhere once there is more than one.
	net := group.Networks[0]
	if len(net.ReservedPorts) != 0 || len(net.DynamicPorts) != 1 {
		t.Fatalf("want one dynamic port, got reserved %v dynamic %v", net.ReservedPorts, net.DynamicPorts)
	}
	if net.DynamicPorts[0].HostNetwork != "internal" {
		t.Errorf("host network = %q, want internal", net.DynamicPorts[0].HostNetwork)
	}

	task := group.Tasks[0]
	if task.Config["pid_mode"] != "host" || task.Config["network_mode"] != "host" {
		t.Errorf("the numbers must be the host's: %v", task.Config)
	}
	args := strings.Join(task.Config["args"].([]string), " ")
	for _, want := range []string{"--path.rootfs=/host", "--web.listen-address=${NOMAD_ADDR_http}"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}

	svc := group.Services[0]
	if svc.Name != "orca-node-exporter" || len(svc.Tags) != 1 || svc.Tags[0] != "node=${node.unique.name}" {
		t.Errorf("registration = %s %v", svc.Name, svc.Tags)
	}
}

func platformJobNamed(t *testing.T, opts PlatformOptions, name string) *nomad.Job {
	t.Helper()
	for _, j := range BuildPlatform(opts) {
		if j.Meta[MetaService] == name {
			return j
		}
	}
	return nil
}

// The status page is orca, run from the build apply shipped, reading Nomad on
// loopback and the stores at whatever address the catalog has for them.
func TestStatusJob(t *testing.T) {
	job := platformJobNamed(t, platformOpts(), "status")
	if job == nil {
		t.Fatal("no status job")
	}
	group := job.TaskGroups[0]
	task := group.Tasks[0]

	if task.Config["image"] != "alpine:3" || task.Config["command"] != "/orca" {
		t.Errorf("image %v command %v", task.Config["image"], task.Config["command"])
	}
	vols, _ := task.Config["volumes"].([]string)
	if len(vols) != 1 || vols[0] != "/var/orca/bin/orca-0123456789abcdef:/orca:ro" {
		t.Errorf("volumes = %v", vols)
	}
	args := strings.Join(task.Config["args"].([]string), " ")
	for _, want := range []string{"serve-status", "--listen=${NOMAD_ADDR_http}", "--nomad=http://127.0.0.1:4646",
		"--link=nomad=https://nomad.example.com", "--link=logs=https://logs.example.com", "--link=metrics=https://metrics.example.com"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
	// Loopback is where Nomad answers, and the firewall keeps the bridge off
	// it, so the page has to share the host's network.
	if task.Config["network_mode"] != "host" {
		t.Error("the status page must run on the host's network to reach Nomad")
	}
	net := group.Networks[0]
	if len(net.ReservedPorts) != 0 || len(net.DynamicPorts) != 1 || net.DynamicPorts[0].HostNetwork != "internal" {
		t.Errorf("want one dynamic internal port, got %+v", net)
	}

	env := *task.Templates[0].EmbeddedTmpl
	for _, want := range []string{`nomadService "orca-victoriametrics"`, "ORCA_METRICS_URL=", `nomadService "orca-victorialogs"`, "ORCA_LOGS_URL="} {
		if !strings.Contains(env, want) {
			t.Errorf("env template missing %q:\n%s", want, env)
		}
	}
	if !*task.Templates[0].Envvars {
		t.Error("the stores' addresses should reach the page as environment variables")
	}
	if c := group.Services[0].Checks[0]; c.Path != "/healthz" {
		t.Errorf("check path = %q", c.Path)
	}

	// A published image carries orca itself: no mount, the job otherwise the same.
	opts := platformOpts()
	opts.Status = &StatusSpec{Image: "ghcr.io/unitoftime/orca:v1"}
	if vols := platformJobNamed(t, opts, "status").TaskGroups[0].Tasks[0].Config["volumes"]; vols != nil {
		t.Errorf("an image with orca in it needs no mount, got %v", vols)
	}

	// A store that is off is simply not in the page's environment.
	opts = platformOpts()
	opts.Metrics = nil
	env = *platformJobNamed(t, opts, "status").TaskGroups[0].Tasks[0].Templates[0].EmbeddedTmpl
	if strings.Contains(env, "ORCA_METRICS_URL") {
		t.Errorf("metrics are off but still in the env:\n%s", env)
	}
}

func TestStatusIsPublishedWithTheDashboards(t *testing.T) {
	cfg := traefikDynamicConfig(platformOpts())
	for _, want := range []string{"status.example.com", `nomadService "orca-status"`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("dynamic config missing %q:\n%s", want, cfg)
		}
	}
	if urls := DashboardURLs(platformOpts()); len(urls) == 0 || urls[0] != "https://status.example.com" {
		t.Errorf("status should be the first url, got %v", urls)
	}

	opts := platformOpts()
	opts.Status = nil
	if cfg := traefikDynamicConfig(opts); strings.Contains(cfg, "status.example.com") {
		t.Errorf("status is off but still routed:\n%s", cfg)
	}
}

// A dashboard's backend moving (the status page gets a new port on every
// upgrade) must re-route it without restarting ingress, which would drop
// every HTTP service for the sake of one route.
func TestDashboardRoutesReloadWithoutRestart(t *testing.T) {
	job := platformJobNamed(t, platformOpts(), "traefik")
	var found bool
	for _, tmpl := range job.TaskGroups[0].Tasks[0].Templates {
		if *tmpl.DestPath == "local/dynamic.yml" {
			found = true
			if *tmpl.ChangeMode != "noop" {
				t.Errorf("dynamic.yml change mode = %q, want noop", *tmpl.ChangeMode)
			}
		}
	}
	if !found {
		t.Fatal("no dynamic.yml template")
	}
	if !strings.Contains(traefikConfig(platformOpts()), "watch: true") {
		t.Error("the file provider must watch the file for noop to reach Traefik")
	}
}

func TestStatusBinaryPathIsContentAddressed(t *testing.T) {
	sum := "0123456789abcdef" + strings.Repeat("0", 48)
	if got := StatusBinaryPath("/var/orca", sum); got != "/var/orca/bin/orca-0123456789abcdef" {
		t.Errorf("path = %q", got)
	}
}

// Allocation directories are not disks; the disks they sit on are.
func TestNodeMountExclude(t *testing.T) {
	re := regexp.MustCompile(nodeMountExclude("/var/orca"))
	for _, m := range []string{"/var/orca/nomad/alloc/0b1c/web/secrets", "/proc", "/var/lib/docker/overlay2/x/merged"} {
		if !re.MatchString(m) {
			t.Errorf("%s should be excluded", m)
		}
	}
	for _, m := range []string{"/", "/boot", "/var/orca", "/var/orca/volumes", "/var/orca/nomadic"} {
		if re.MatchString(m) {
			t.Errorf("%s should be reported", m)
		}
	}
}

// Nothing the platform runs for itself may be reachable from outside the
// machines: on one machine that means loopback, and once there is a cluster it
// means the private network. Never the public interface.
func TestStoresNeverBindPublic(t *testing.T) {
	for _, network := range []string{"internal"} {
		opts := platformOpts()
		opts.StoreNetwork = network

		for _, j := range BuildPlatform(opts) {
			svc := j.Meta[MetaService]
			if svc != "victorialogs" && svc != "victoriametrics" {
				continue
			}
			ports := j.TaskGroups[0].Networks[0].ReservedPorts
			if len(ports) != 1 {
				t.Fatalf("%s: want one reserved port, got %+v", svc, ports)
			}
			if ports[0].HostNetwork != network {
				t.Errorf("%s: host network = %q, want %q", svc, ports[0].HostNetwork, network)
			}
			// The listen address follows the bound port rather than being
			// written separately, so the two cannot disagree.
			args, _ := j.TaskGroups[0].Tasks[0].Config["args"].([]string)
			if !strings.Contains(strings.Join(args, " "), "httpListenAddr=${NOMAD_ADDR_http}") {
				t.Errorf("%s: listen address should follow the bound port, args: %v", svc, args)
			}
		}
	}
}

// Ingress is the one platform component that binds a public interface, because
// being the public front door is its entire job.
func TestOnlyIngressBindsPublic(t *testing.T) {
	for _, j := range BuildPlatform(platformOpts()) {
		svc := j.Meta[MetaService]
		for _, n := range j.TaskGroups[0].Networks {
			for _, p := range append(n.ReservedPorts, n.DynamicPorts...) {
				if p.HostNetwork == "public" && svc != "traefik" {
					t.Errorf("%s binds the public interface on port %d", svc, p.Value)
				}
				if svc == "traefik" && p.HostNetwork != "public" {
					t.Errorf("ingress port %d should be explicitly public, got %q", p.Value, p.HostNetwork)
				}
			}
		}
	}
}

// Vector runs on every machine while the store runs on one, so a hardcoded
// loopback sink would silently drop the logs of every machine but that one.
func TestVectorResolvesTheStoreFromTheCatalog(t *testing.T) {
	cfg := vectorConfig()
	if !strings.Contains(cfg, `nomadService "orca-victorialogs"`) {
		t.Errorf("vector should resolve the store from the catalog:\n%s", cfg)
	}
	// And still parse before the store has registered, or it crash-loops on
	// unparseable YAML until it does.
	if !strings.Contains(cfg, `$addr := "127.0.0.1:9428"`) {
		t.Errorf("vector config needs a fallback address:\n%s", cfg)
	}
}

// A log line names its machine the way a metric series does: by the node
// name from cluster.yaml, not the OS hostname, which is whatever the provider
// called the box.
func TestVectorLabelsLinesWithTheNode(t *testing.T) {
	cfg := vectorConfig()
	if !strings.Contains(cfg, `.node = {{ env "node.unique.name" | toJSON }}`) {
		t.Errorf("vector should label each line with the node name:\n%s", cfg)
	}
	if !strings.Contains(cfg, "del(.host)") {
		t.Errorf("vector should drop the OS hostname, which disagrees with node:\n%s", cfg)
	}
	if !strings.Contains(cfg, "_stream_fields: job,task,node") {
		t.Errorf("each machine's lines should be a stream of their own:\n%s", cfg)
	}
}

// Once the stores bind the private address, nothing listens on loopback for
// ingress to reach.
func TestDashboardBackendsResolveFromTheCatalog(t *testing.T) {
	cfg := traefikDynamicConfig(platformOpts())
	for _, want := range []string{
		`nomadService "orca-victorialogs"`,
		`nomadService "orca-victoriametrics"`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("dynamic config should resolve %s:\n%s", want, cfg)
		}
	}
	// Nomad is the exception: it binds loopback on every machine by design.
	if !strings.Contains(cfg, "http://127.0.0.1:4646") {
		t.Errorf("the nomad dashboard should use loopback:\n%s", cfg)
	}
}

// The disk flags reject "10G": they want a bare byte count. A rejected flag is
// a container that exits 2 with its usage on stdout and nothing on stderr.
func TestDiskLimitsAreByteCounts(t *testing.T) {
	for _, j := range BuildPlatform(platformOpts()) {
		args, _ := j.TaskGroups[0].Tasks[0].Config["args"].([]string)
		joined := strings.Join(args, " ")
		switch j.Meta[MetaService] {
		case "victorialogs":
			if !strings.Contains(joined, "-retention.maxDiskSpaceUsageBytes=10737418240") {
				t.Errorf("log disk cap not a byte count: %v", args)
			}
		case "victoriametrics":
			if !strings.Contains(joined, "-storage.minFreeDiskSpaceBytes=2147483648") {
				t.Errorf("metrics free-space floor not a byte count: %v", args)
			}
		}
	}
}

// Dashboards fail closed: no password means they are not published at all.
func TestDashboardsRequireAPassword(t *testing.T) {
	opts := platformOpts()
	opts.Ingress.AuthHash = ""

	if dashboardsEnabled(opts) {
		t.Error("dashboards should not be published without a password")
	}
	if got := traefikDynamicConfig(opts); got != "" {
		t.Errorf("want no dynamic config, got:\n%s", got)
	}
	if got := DashboardURLs(opts); got != nil {
		t.Errorf("want no urls, got %v", got)
	}
}

// The page links only to what is published: nothing without a domain, and no
// store that is switched off.
func TestStatusLinksOnlyPublishedUIs(t *testing.T) {
	links := func(opts PlatformOptions) string {
		var out []string
		for _, a := range statusJob(opts).TaskGroups[0].Tasks[0].Config["args"].([]string) {
			if strings.HasPrefix(a, "--link=") {
				out = append(out, a)
			}
		}
		return strings.Join(out, " ")
	}

	opts := platformOpts()
	opts.Logs = nil
	if got := links(opts); strings.Contains(got, "logs") || !strings.Contains(got, "--link=nomad=") {
		t.Errorf("logs off: links = %q", got)
	}

	opts = platformOpts()
	opts.Domain = ""
	if got := links(opts); got != "" {
		t.Errorf("no domain: links = %q", got)
	}
}

func TestDashboardsRequireADomain(t *testing.T) {
	opts := platformOpts()
	opts.Domain = ""
	if dashboardsEnabled(opts) {
		t.Error("dashboards need a domain to be served on")
	}
}

func TestDashboardRoutes(t *testing.T) {
	cfg := traefikDynamicConfig(platformOpts())

	for _, want := range []string{
		"logs.example.com", "metrics.example.com", "nomad.example.com",
		"127.0.0.1:4646",
		"basicAuth", "admin:$2a$10$hash", "certResolver: orca",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("dynamic config missing %q:\n%s", want, cfg)
		}
	}
}

// A dashboard for a capability that is switched off would route to nothing.
func TestDashboardsFollowEnabledCapabilities(t *testing.T) {
	opts := platformOpts()
	opts.Logs = nil

	cfg := traefikDynamicConfig(opts)
	if strings.Contains(cfg, "logs.example.com") {
		t.Errorf("logs is disabled but still routed:\n%s", cfg)
	}
	if !strings.Contains(cfg, "metrics.example.com") {
		t.Error("metrics should still be routed")
	}
}

// registration is a stand-in for what Nomad's nomadService template function
// returns.
type registration struct {
	Address string
	Port    int
	Tags    []string
}

// renderScrape renders the scrape config the way Nomad would, with stand-ins
// for the functions it provides, and parses the result the way
// VictoriaMetrics will read it. A typo in a template action, or labels that
// come out as the wrong YAML type, would otherwise first show up as a metric
// store that will not load its config on the box.
func renderScrape(t *testing.T, opts PlatformOptions, services, nodes []registration) scrapeFile {
	t.Helper()
	funcs := template.FuncMap{
		"env": func(string) string { return "10.0.0.1:8428" },
		"nomadService": func(name string) []registration {
			switch name {
			case MetricsCatalogName:
				return services
			case "orca-node-exporter":
				return nodes
			}
			return nil
		},
		"split": func(sep, s string) []string { return strings.Split(s, sep) },
	}
	tmpl, err := template.New("scrape").Funcs(funcs).Parse(scrapeConfig(opts))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := tmpl.Execute(&out, nil); err != nil {
		t.Fatal(err)
	}
	var f scrapeFile
	if err := yaml.Unmarshal([]byte(out.String()), &f); err != nil {
		t.Fatalf("rendered config is not YAML: %v\n%s", err, out.String())
	}
	return f
}

func TestScrapeConfigScrapesMetricsPorts(t *testing.T) {
	render := func(t *testing.T, services []registration, nodes ...registration) scrapeFile {
		t.Helper()
		opts := platformOpts()
		opts.Logs = nil
		return renderScrape(t, opts, services, nodes)
	}

	t.Run("targets carry their group and service", func(t *testing.T) {
		f := render(t, []registration{
			{Address: "172.26.64.5", Port: 2112, Tags: MetricsTags("prod", "proxy")},
			// A group name YAML would read as a number stays a string.
			{Address: "172.26.64.6", Port: 2113, Tags: MetricsTags("123", "server")},
		})
		job := f.job("services")
		if job == nil {
			t.Fatal("no services job in the scrape config")
		}
		if len(job.StaticConfigs) != 2 {
			t.Fatalf("want 2 targets, got %d", len(job.StaticConfigs))
		}
		first := job.StaticConfigs[0]
		if first.Targets[0] != "172.26.64.5:2112" {
			t.Errorf("target = %q", first.Targets[0])
		}
		if first.Labels["group"] != "prod" || first.Labels["service"] != "proxy" {
			t.Errorf("labels = %v", first.Labels)
		}
		if got := job.StaticConfigs[1].Labels["group"]; got != "123" {
			t.Errorf("numeric group label = %q", got)
		}
	})

	// An empty static_configs is a null VictoriaMetrics may refuse, so with
	// nothing to scrape there is no job at all.
	t.Run("no targets, no job", func(t *testing.T) {
		if f := render(t, nil); f.job("services") != nil {
			t.Error("services job rendered with nothing to scrape")
		}
		if f := render(t, nil); f.job("nomad") == nil {
			t.Error("the platform's own targets should still be scraped")
		}
		if f := render(t, nil); f.job("nodes") != nil {
			t.Error("nodes job rendered with no exporter registered")
		}
	})

	t.Run("each machine is labelled with its name", func(t *testing.T) {
		f := render(t, nil,
			registration{Address: "10.0.0.10", Port: 23456, Tags: []string{"node=box0"}},
			registration{Address: "10.0.0.11", Port: 24567, Tags: []string{"node=box1"}},
		)
		job := f.job("nodes")
		if job == nil {
			t.Fatal("no nodes job in the scrape config")
		}
		if len(job.StaticConfigs) != 2 {
			t.Fatalf("want 2 targets, got %d", len(job.StaticConfigs))
		}
		if got := job.StaticConfigs[1]; got.Targets[0] != "10.0.0.11:24567" || got.Labels["node"] != "box1" {
			t.Errorf("target = %v", got)
		}
	})
}

// Each Nomad agent reports only its own machine's allocations, so every agent
// must be scraped. Scraping one would leave every service on the other
// machines without usage numbers.
func TestScrapeConfigScrapesEveryNomadAgent(t *testing.T) {
	nodes := []registration{
		{Address: "10.0.0.10", Port: 23456, Tags: []string{"node=box0"}},
		{Address: "10.0.0.11", Port: 24567, Tags: []string{"node=box1"}},
	}

	t.Run("one machine: loopback, labelled", func(t *testing.T) {
		job := renderScrape(t, platformOpts(), nil, nodes[:1]).job("nomad")
		if job == nil {
			t.Fatal("no nomad job")
		}
		if job.MetricsPath != "/v1/metrics" {
			t.Errorf("metrics_path = %q", job.MetricsPath)
		}
		if len(job.StaticConfigs) != 1 {
			t.Fatalf("want one target, got %v", job.StaticConfigs)
		}
		sc := job.StaticConfigs[0]
		if sc.Targets[0] != "127.0.0.1:4646" || sc.Labels["node"] != "box0" {
			t.Errorf("target = %v", sc)
		}
	})

	t.Run("several machines: every agent, at its private address", func(t *testing.T) {
		opts := platformOpts()
		opts.MultiNode = true
		job := renderScrape(t, opts, nil, nodes).job("nomad")
		if job == nil {
			t.Fatal("no nomad job")
		}
		if job.MetricsPath != "/v1/metrics" {
			t.Errorf("metrics_path = %q", job.MetricsPath)
		}
		if len(job.StaticConfigs) != 2 {
			t.Fatalf("want a target per machine, got %v", job.StaticConfigs)
		}
		sc := job.StaticConfigs[1]
		if sc.Targets[0] != "10.0.0.11:4646" || sc.Labels["node"] != "box1" {
			t.Errorf("target = %v", sc)
		}
	})
}

type scrapeFile struct {
	ScrapeConfigs []scrapeJob `yaml:"scrape_configs"`
}

type scrapeJob struct {
	JobName       string         `yaml:"job_name"`
	MetricsPath   string         `yaml:"metrics_path"`
	StaticConfigs []staticConfig `yaml:"static_configs"`
}

type staticConfig struct {
	Targets []string          `yaml:"targets"`
	Labels  map[string]string `yaml:"labels"`
}

func (f scrapeFile) job(name string) *scrapeJob {
	for i := range f.ScrapeConfigs {
		if f.ScrapeConfigs[i].JobName == name {
			return &f.ScrapeConfigs[i]
		}
	}
	return nil
}

// A machine joining changes what "internal" is, and a running allocation
// keeps the address it was placed with. Every platform job's spec has to
// change with it, or apply sees nothing to do and the stores stay on the
// first machine's bridge, an address every other machine has for itself.
func TestPlatformJobsChangeWhenTheClusterGrows(t *testing.T) {
	one := map[string]string{}
	for _, j := range BuildPlatform(platformOpts()) {
		if _, ok := j.Meta[MetaNetwork]; ok {
			t.Errorf("%s: one machine must render exactly as before, got %s=%q", *j.ID, MetaNetwork, j.Meta[MetaNetwork])
		}
		one[*j.ID] = j.Meta[MetaHash]
	}

	opts := platformOpts()
	opts.MultiNode = true
	for _, j := range BuildPlatform(opts) {
		if j.Meta[MetaHash] == one[*j.ID] {
			t.Errorf("%s: same spec on one machine and on several, so apply would never re-place it", *j.ID)
		}
	}
}

// The stores and the status page go where monitoring is, ingress where DNS
// points, and the jobs on every machine nowhere in particular.
func TestPlatformPlacement(t *testing.T) {
	opts := platformOpts()
	opts.MonitoringNode, opts.IngressNode = "box2", "box1"
	opts.DNS = &DNSSpec{}

	want := map[string]string{
		"victorialogs": "box2", "victoriametrics": "box2", "status": "box2",
		"traefik": "box1",
		"dns":     "", "vector": "", "node-exporter": "",
	}
	for _, j := range BuildPlatform(opts) {
		svc := j.Meta[MetaService]
		expected, ok := want[svc]
		if !ok {
			t.Errorf("%s: no expectation for this job", svc)
			continue
		}
		var got string
		for _, c := range j.TaskGroups[0].Constraints {
			if c.LTarget == "${node.unique.name}" {
				got = c.RTarget
			}
		}
		if got != expected {
			t.Errorf("%s pinned to %q, want %q", svc, got, expected)
		}
		delete(want, svc)
	}
	for svc := range want {
		t.Errorf("%s: not rendered", svc)
	}
}
