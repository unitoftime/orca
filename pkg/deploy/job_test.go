package deploy

import (
	"strings"
	"testing"
	"time"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/pkg/manifest"
)

func parse(t *testing.T, body string) *manifest.Manifest {
	return parseIn(t, "a", body)
}

func parseIn(t *testing.T, group, body string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.ParseGroup(group, []byte(body), group+"/svc.yaml")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	return m
}

func buildOne(t *testing.T, body, service string, opts Options) *nomad.Job {
	return buildOneIn(t, "a", body, service, opts)
}

func buildOneIn(t *testing.T, group, body, service string, opts Options) *nomad.Job {
	t.Helper()
	m := parseIn(t, group, body)
	s, ok := m.Service(service)
	if !ok {
		t.Fatalf("no service %q", service)
	}
	job, err := Build(m, s, "img@sha256:"+strings.Repeat("a", 64), opts)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return job
}

const defaultOptsDC = "home"

func defaultOpts() Options {
	return Options{Datacenter: defaultOptsDC, Ingress: true, TLS: true, DataDir: "/var/orca"}
}

func TestBuildBasics(t *testing.T) {
	job := buildOneIn(t, "blog", `
name: api
image: ghcr.io/x/blog:1.4
cpu: 2
memory: 4G
ports:
  7777: [tcp, udp]
`, "api", defaultOpts())

	if *job.ID != "blog-api" {
		t.Errorf("job ID = %q, want blog-api", *job.ID)
	}
	if job.Meta[MetaManaged] != "true" || job.Meta[MetaApp] != "blog" || job.Meta[MetaService] != "api" {
		t.Errorf("meta = %v", job.Meta)
	}

	task := job.TaskGroups[0].Tasks[0]
	// 2 vCPU at 500MHz each.
	if *task.Resources.CPU != 1000 {
		t.Errorf("cpu = %d MHz, want 1000", *task.Resources.CPU)
	}
	if *task.Resources.MemoryMB != 4096 {
		t.Errorf("memory = %d MB, want 4096", *task.Resources.MemoryMB)
	}
}

// A raw host port must be reserved at exactly the number asked for. A dynamic
// port here would hand a UDP service a different port on every deploy.
func TestRawPortIsReserved(t *testing.T) {
	job := buildOneIn(t, "blog", `
name: api
image: i:1
ports:
  7777: [tcp, udp]
`, "api", defaultOpts())

	net := job.TaskGroups[0].Networks[0]
	if net.Mode != "bridge" {
		t.Errorf("network mode = %q", net.Mode)
	}
	if len(net.ReservedPorts) != 1 {
		t.Fatalf("reserved ports = %+v, want 1", net.ReservedPorts)
	}
	p := net.ReservedPorts[0]
	if p.Value != 7777 || p.To != 7777 {
		t.Errorf("reserved port = %+v, want host and container 7777", p)
	}
	if len(net.DynamicPorts) != 0 {
		t.Errorf("want no dynamic ports, got %+v", net.DynamicPorts)
	}
}

func TestHostPortMayDifferFromContainerPort(t *testing.T) {
	job := buildOneIn(t, "blog", `
name: dev
image: i:1
ports:
  7777: tcp:7778
`, "dev", defaultOpts())

	p := job.TaskGroups[0].Networks[0].ReservedPorts[0]
	if p.Value != 7778 || p.To != 7777 {
		t.Errorf("port = %+v, want host 7778 -> container 7777", p)
	}
}

// An internal service publishes no host port at all on a single machine. It
// registers its allocation's own address instead, which every container can
// reach over the bridge and nothing outside the machine can reach.
//
// Publishing a host port here would silently put internal-only services on the
// public internet at a random high port.
func TestInternalServicePublishesNothing(t *testing.T) {
	job := buildOneIn(t, "blog", `
name: db
template: postgres:17
volume: 20G
`, "db", defaultOpts())

	net := job.TaskGroups[0].Networks[0]
	if len(net.ReservedPorts) != 0 || len(net.DynamicPorts) != 0 {
		t.Fatalf("an internal service should publish no host port, got reserved=%+v dynamic=%+v",
			net.ReservedPorts, net.DynamicPorts)
	}

	svc := job.TaskGroups[0].Services[0]
	if svc.Name != "blog-db" {
		t.Errorf("catalog name = %q, want blog-db", svc.Name)
	}
	if svc.AddressMode != "alloc" {
		t.Errorf("address mode = %q, want alloc so the catalog holds the container's own address", svc.AddressMode)
	}
	// With no host port there is no label to name, so the port is the
	// container port itself.
	if svc.PortLabel != "5432" {
		t.Errorf("port label = %q, want the container port 5432", svc.PortLabel)
	}
	if len(svc.Checks) != 1 || svc.Checks[0].AddressMode != "alloc" {
		t.Errorf("the check must use the same address as the registration, got %+v", svc.Checks)
	}
}

// Above one machine an allocation address is ambiguous, because every node's
// bridge uses the same CIDR. The port is published on the private network,
// never the public interface, and at the container's own number, because the
// resolver answers with an address and no port: `db.shop:5432` has to mean
// 5432 on whichever machine db is on.
func TestInternalServiceOnManyMachinesUsesPrivate(t *testing.T) {
	opts := defaultOpts()
	opts.InternalNetwork = "internal"

	job := buildOneIn(t, "blog", `
name: db
template: postgres:17
volume: 20G
`, "db", opts)

	net := job.TaskGroups[0].Networks[0]
	if len(net.ReservedPorts) != 1 || len(net.DynamicPorts) != 0 {
		t.Fatalf("want one reserved port, got %+v / %+v", net.ReservedPorts, net.DynamicPorts)
	}
	p := net.ReservedPorts[0]
	if p.To != 5432 || p.Value != 5432 {
		t.Errorf("port = %d -> %d, want 5432 -> 5432", p.Value, p.To)
	}
	if p.HostNetwork != "internal" {
		t.Errorf("host network = %q, want internal", p.HostNetwork)
	}

	svc := job.TaskGroups[0].Services[0]
	if svc.AddressMode == "alloc" {
		t.Error("a published port should register the host address, not the allocation's")
	}
}

// `expose:` is the only way anything becomes publicly reachable, and the job
// spec should say so rather than relying on Nomad's default interface.
func TestExposedPortsAreExplicitlyPublic(t *testing.T) {
	job := buildOneIn(t, "blog", `
name: api
image: i:1
ports:
  7777: tcp:7777
`, "api", defaultOpts())

	p := job.TaskGroups[0].Networks[0].ReservedPorts[0]
	if p.HostNetwork != "public" {
		t.Errorf("host network = %q, want an explicit public", p.HostNetwork)
	}
}

// A service that listens on nothing registers nothing: there is no address to
// publish and no check to gate a rollout on.
func TestServiceWithNoPortRegistersNothing(t *testing.T) {
	job := buildOne(t, "{name: worker, image: i:1}", "worker", defaultOpts())
	if len(job.TaskGroups[0].Services) != 0 {
		t.Errorf("want no service registration, got %+v", job.TaskGroups[0].Services)
	}
}

func TestHTTPExposeGetsTraefikTags(t *testing.T) {
	job := buildOneIn(t, "shop", `
name: web
image: i:1
ports:
  8080: web.shop.example.com
`, "web", defaultOpts())

	tags := strings.Join(job.TaskGroups[0].Services[0].Tags, "\n")
	for _, want := range []string{
		"traefik.enable=true",
		"Host(`web.shop.example.com`)",
		".tls=true",
	} {
		if !strings.Contains(tags, want) {
			t.Errorf("tags should contain %q, got:\n%s", want, tags)
		}
	}
}

// Without certificates there is no https entrypoint, so a route must attach
// to http, and every entrypoint a route names must be one ingress defines.
// Traefik drops a route naming one it does not, such as "websecure".
func TestRoutesNameEntrypointsIngressDefines(t *testing.T) {
	for _, tls := range []bool{true, false} {
		opts := defaultOpts()
		opts.TLS = tls
		job := buildOneIn(t, "shop", "{name: web, image: i:1, ports: {8080: web.shop.example.com}}", "web", opts)

		static := traefikConfig(PlatformOptions{Ingress: &IngressSpec{TLS: tls}})

		for _, tag := range job.TaskGroups[0].Services[0].Tags {
			if _, ep, ok := strings.Cut(tag, ".entrypoints="); ok {
				if !strings.Contains(static, "\n  "+ep+":\n") {
					t.Errorf("tls=%v: route names entrypoint %q, which ingress does not define:\n%s", tls, ep, static)
				}
			}
			if _, r, ok := strings.Cut(tag, ".tls.certresolver="); ok {
				t.Errorf("tls=%v: route names resolver %q, and ingress defines none: its certificates come from the certificate job", tls, r)
			}
		}
	}
}

// Ingress routes to the registered port, so the routed port must be the one
// registered even with a lower internal port beside it, or web traffic goes
// to the lower one.
func TestTheRoutedPortIsTheRegisteredOne(t *testing.T) {
	job := buildOneIn(t, "shop", `
name: web
image: i:1
ports:
  2112: internal
  3000: web.shop.example.com
`, "web", defaultOpts())

	svc := job.TaskGroups[0].Services[0]
	if svc.PortLabel != "3000" {
		t.Errorf("registered port = %q, want the routed 3000", svc.PortLabel)
	}
}

// An HTTP port is reached by ingress, not by the internet, so it is addressed
// like an internal one and publishes nothing on the public interface. It must
// not be a dynamic port there, unreachable only because the firewall says so.
func TestHTTPPortPublishesNothingPublic(t *testing.T) {
	job := buildOneIn(t, "shop", "{name: web, image: i:1, ports: {8080: web.example.com}}", "web", defaultOpts())
	net := job.TaskGroups[0].Networks[0]
	if len(net.ReservedPorts) != 0 || len(net.DynamicPorts) != 0 {
		t.Errorf("a single machine needs no host port for ingress, got %+v / %+v", net.ReservedPorts, net.DynamicPorts)
	}
	if svc := job.TaskGroups[0].Services[0]; svc.AddressMode != "alloc" || svc.PortLabel != "8080" {
		t.Errorf("want the allocation's own address and port, got %q %q", svc.AddressMode, svc.PortLabel)
	}

	opts := defaultOpts()
	opts.InternalNetwork = "internal"
	job = buildOneIn(t, "shop", "{name: web, image: i:1, ports: {8080: web.example.com}}", "web", opts)
	for _, p := range job.TaskGroups[0].Networks[0].ReservedPorts {
		if p.HostNetwork != "internal" {
			t.Errorf("above one machine an http port binds the internal network, got %q", p.HostNetwork)
		}
	}
}

func TestIngressOffRefusesRoutedPorts(t *testing.T) {
	opts := defaultOpts()
	opts.Ingress = false
	m := parse(t, "{name: web, image: i:1, ports: {8080: errors.example.org}}")
	_, err := BuildApp(m, map[string]string{"web": "i@sha256:1"}, opts, func(*manifest.Service) (string, error) { return "", nil })
	if err == nil || !strings.Contains(err.Error(), "switched off") {
		t.Errorf("want a refusal naming ingress, got %v", err)
	}
}

func TestCustomHostnameUsesItVerbatim(t *testing.T) {
	job := buildOneIn(t, "shop", `
name: web
image: i:1
ports:
  8080: errors.example.org
`, "web", defaultOpts())

	tags := strings.Join(job.TaskGroups[0].Services[0].Tags, "\n")
	if !strings.Contains(tags, "Host(`errors.example.org`)") {
		t.Errorf("want the custom hostname, got:\n%s", tags)
	}
}

// A volume means the data is on one disk, so the job must be pinned and the
// bind mount must point into the data directory.
func TestVolumePinsAndMounts(t *testing.T) {
	opts := defaultOpts()
	opts.Node = "box0"

	job := buildOneIn(t, "blog", `
name: db
template: postgres:17
volume: 20G
`, "db", opts)

	group := job.TaskGroups[0]
	if len(group.Constraints) != 1 {
		t.Fatalf("want a placement constraint, got %+v", group.Constraints)
	}
	c := group.Constraints[0]
	if c.LTarget != "${node.unique.name}" || c.RTarget != "box0" {
		t.Errorf("constraint = %+v, want pinning to box0", c)
	}

	vols, _ := group.Tasks[0].Config["volumes"].([]string)
	want := "/var/orca/volumes/services/blog/db:/var/lib/postgresql/data"
	if len(vols) != 1 || vols[0] != want {
		t.Errorf("volumes = %v, want %q", vols, want)
	}
}

func TestNoVolumeMeansNoConstraint(t *testing.T) {
	job := buildOne(t, "{name: bot, image: i:1}", "bot", defaultOpts())
	if len(job.TaskGroups[0].Constraints) != 0 {
		t.Errorf("a stateless service should not be pinned, got %+v", job.TaskGroups[0].Constraints)
	}
}

// Rolling on health checks requires there to be health checks. A service with
// no port has none, and waiting for them would hang the deploy forever.
func TestUpdateHealthCheckMatchesWhetherChecksExist(t *testing.T) {
	withPort := buildOne(t, "{name: web, image: i:1, ports: {8080: web.example.com}}", "web", defaultOpts())
	if *withPort.Update.HealthCheck != "checks" {
		t.Errorf("a service with checks should gate on checks, got %q", *withPort.Update.HealthCheck)
	}

	worker := buildOne(t, "{name: worker, image: i:1}", "worker", defaultOpts())
	if *worker.Update.HealthCheck != "task_states" {
		t.Errorf("a service with no checks must not gate on checks, got %q", *worker.Update.HealthCheck)
	}
}

// A service names its own hostname, on whatever domain it likes; the cluster
// has no domain of its own for it to need.
func TestHostnamesOnAnyDomain(t *testing.T) {
	for _, host := range []string{"shop.example.com", "example.org", "api.other.net"} {
		m := parse(t, "{name: web, image: i:1, ports: {8080: "+host+"}}")
		jobs, err := BuildApp(m, map[string]string{"web": "i@sha256:x"}, defaultOpts(), func(*manifest.Service) (string, error) { return "", nil })
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if tags := strings.Join(jobs[0].TaskGroups[0].Services[0].Tags, "\n"); !strings.Contains(tags, "Host(`"+host+"`)") {
			t.Errorf("%s: not routed:\n%s", host, tags)
		}
	}
}

// Docker's command/args replace an image's CMD but leave its ENTRYPOINT in
// front. Setting only those on an image that has one runs the entrypoint with
// our command as its arguments, which fails in a way that points nowhere near
// the cause. Overriding the entrypoint is what makes `cmd` mean the same thing
// on every image.
func TestCmdOverridesTheEntrypoint(t *testing.T) {
	job := buildOne(t, `
name: worker
image: i:1
cmd: /app -flag=1
`, "worker", defaultOpts())

	cfg := job.TaskGroups[0].Tasks[0].Config
	ep, _ := cfg["entrypoint"].([]string)
	if len(ep) != 2 || ep[0] != "/bin/sh" || ep[1] != "-c" {
		t.Errorf("entrypoint = %v, want /bin/sh -c", cfg["entrypoint"])
	}
	args, _ := cfg["args"].([]string)
	if len(args) != 1 || args[0] != "/app -flag=1" {
		t.Errorf("args = %v, want the command as one shell string", cfg["args"])
	}
	if _, set := cfg["command"]; set {
		t.Error("command must not be set alongside an overridden entrypoint")
	}
}

func TestNoCmdLeavesTheImageAlone(t *testing.T) {
	job := buildOne(t, "{name: w, image: i:1}", "w", defaultOpts())
	cfg := job.TaskGroups[0].Tasks[0].Config
	for _, k := range []string{"entrypoint", "command", "args"} {
		if _, set := cfg[k]; set {
			t.Errorf("a service with no cmd must not set %q", k)
		}
	}
}

// A metrics port is registered as a scrape target alongside the service's own
// registration: under the shared catalog name the metric store ranges over,
// tagged with where it came from, and never with the tag the resolver answers
// for, or `server` would resolve to its metrics endpoint.
func TestMetricsPortIsAScrapeTarget(t *testing.T) {
	body := `
name: server
image: ghcr.io/x/blog-server:1
ports:
  9000: internal
  2113: metrics
`
	job := buildOneIn(t, "prod", body, "server", defaultOpts())

	svcs := job.TaskGroups[0].Services
	if len(svcs) != 2 {
		t.Fatalf("want the service and its scrape target, got %d registrations", len(svcs))
	}
	if svcs[0].Name != "prod-server" || svcs[0].PortLabel != "9000" {
		t.Errorf("the service should register at 9000, not its metrics port; got %s at %s",
			svcs[0].Name, svcs[0].PortLabel)
	}

	m := svcs[1]
	if m.Name != MetricsCatalogName {
		t.Errorf("scrape target name = %q, want %q", m.Name, MetricsCatalogName)
	}
	if m.PortLabel != "2113" || m.AddressMode != "alloc" {
		t.Errorf("on one machine the target is the allocation's own 2113; got %q (%s)", m.PortLabel, m.AddressMode)
	}
	if strings.Join(m.Tags, ",") != "group=prod,service=server" {
		t.Errorf("tags = %v", m.Tags)
	}
	for _, tag := range m.Tags {
		if strings.HasPrefix(tag, DNSTagPrefix) {
			t.Errorf("scrape target carries a DNS tag %q", tag)
		}
	}
	if len(m.Checks) != 0 {
		t.Errorf("a scrape target should not gate rollouts, got checks %+v", m.Checks)
	}

	// Nothing about it is public.
	if len(job.TaskGroups[0].Networks[0].ReservedPorts) != 0 {
		t.Errorf("a metrics port published a host port: %+v", job.TaskGroups[0].Networks[0].ReservedPorts)
	}

	// Above one machine it is published on the internal network like any
	// other internal port, and the target is that host port.
	opts := defaultOpts()
	opts.InternalNetwork = "internal"
	job = buildOneIn(t, "prod", body, "server", opts)
	m = job.TaskGroups[0].Services[1]
	if m.PortLabel != "p2113" || m.AddressMode == "alloc" {
		t.Errorf("above one machine the target should be the published p2113; got %q (%s)", m.PortLabel, m.AddressMode)
	}
	found := false
	for _, p := range job.TaskGroups[0].Networks[0].ReservedPorts {
		if p.Label == "p2113" {
			found = p.HostNetwork == "internal"
		}
	}
	if !found {
		t.Error("metrics port not published on the internal network")
	}
}

func TestNoMetricsPortNoScrapeTarget(t *testing.T) {
	job := buildOne(t, "{name: web, image: i:1, ports: {8080: internal}}", "web", defaultOpts())
	for _, s := range job.TaskGroups[0].Services {
		if s.Name == MetricsCatalogName {
			t.Error("registered a scrape target for a service with no metrics port")
		}
	}
}

// A replicated service drains before it stops: it leaves the catalog, and
// only once ingress and the resolver have stopped sending it anything is it
// killed. A single copy does not: waiting would only lengthen the outage.
func TestShutdownDelayOnlyWithAnotherCopy(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		service string
		want    *time.Duration
	}{
		{"replicated", "{name: web, image: i:1, replicas: 3, ports: {8080: internal}}", "web", ptr(DrainDelay)},
		{"one copy", "{name: web, image: i:1, ports: {8080: internal}}", "web", nil},
		{"replicated, nothing registered", "{name: worker, image: i:1, replicas: 3}", "worker", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := buildOneIn(t, "shop", tt.yaml, tt.service, defaultOpts())
			got := job.TaskGroups[0].ShutdownDelay
			switch {
			case tt.want == nil && got != nil:
				t.Errorf("shutdown_delay = %v, want none", *got)
			case tt.want != nil && (got == nil || *got != *tt.want):
				t.Errorf("shutdown_delay = %v, want %v", got, *tt.want)
			}
		})
	}
}

// Blue/green runs two full sets at once, so it is only for a service that
// already runs several and has nothing two copies could not share.
func TestBlueGreenOnlyWhereTwoCopiesCanRun(t *testing.T) {
	bg := func(body string, opts Options) bool {
		job := buildOne(t, body, "web", opts)
		return job.Update.Canary != nil && *job.Update.Canary > 0
	}

	job := buildOne(t, "{name: web, image: i:1, replicas: 2, ports: {8080: web.example.com}}", "web", defaultOpts())
	if job.Update.Canary == nil || *job.Update.Canary != 2 || job.Update.AutoPromote == nil || !*job.Update.AutoPromote {
		t.Fatalf("a replicated web service should start a full new set and promote it: %+v", job.Update)
	}
	if tags := job.TaskGroups[0].Services[0].CanaryTags; len(tags) != 1 || tags[0] != CanaryTag {
		t.Errorf("new copies must register without routing tags until promoted, got %v", tags)
	}

	if bg("{name: web, image: i:1, ports: {8080: web.example.com}}", defaultOpts()) {
		t.Error("one copy: must not be deployed blue/green")
	}
	if bg("{name: web, image: i:1, replicas: 2}", defaultOpts()) {
		t.Error("no port, so nothing to swap: must not be deployed blue/green")
	}
	// Above one machine the port is reserved on the machine, which the new
	// copy could not bind beside the old one.
	multi := defaultOpts()
	multi.InternalNetwork = "internal"
	if bg("{name: web, image: i:1, replicas: 2, ports: {8080: web.example.com}}", multi) {
		t.Error("a reserved host port: must not be deployed blue/green")
	}
}
