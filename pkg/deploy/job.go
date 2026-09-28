// Package deploy turns a validated manifest into Nomad jobs and works out what
// actually needs to change. It builds specs and diffs them; it does not talk to
// a machine, so all of it is testable offline.
package deploy

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/pkg/manifest"
)

// Nomad accounts CPU in MHz. orca's manifests are in fractional vCPU, and this
// is the single point that maps between them.
//
// The value is a scheduling weight, not a cap: it decides how the box is shared
// when everything wants CPU at once, and how densely Nomad will pack. 500 means
// one declared vCPU reserves 500MHz, so a real 3GHz core accepts about six
// declared vCPUs. That deliberate oversubscription is right for a machine whose
// workloads are mostly idle background services.
const MHzPerVCPU = 500

// DrainDelay is how long a copy of a replicated service keeps running after it
// has left the catalog, so that whatever still routes to it has stopped
// before it does.
//
// It is the slower of the two paths that learn an address has gone. Ingress
// watches the catalog and drops a route within a second or two. The resolver
// takes longer: the hosts file is re-rendered 5s after the catalog settles,
// CoreDNS re-reads it every 5s, and its answers carry a 5s TTL — 15s at worst.
// Lengthening any of those without this, or this without them, reopens the
// gap it exists to close.
const DrainDelay = 15 * time.Second

// HealthyDeadline is how long a new allocation has to pass its checks before
// Nomad calls it unhealthy, and ProgressDeadline how long a whole rollout may
// go without an allocation becoming healthy before Nomad fails it and rolls
// back. Apply waits for Nomad's verdict, so these bound how long it waits.
const (
	HealthyDeadline  = 4 * time.Minute
	ProgressDeadline = 5 * time.Minute
)

// StopTimeout is how long a service holding data is given to shut down
// before it is killed. Nomad's default is five seconds, which a database
// flushing to disk overruns and is then killed mid-write, to spend its next
// start recovering. Thirty is the most a Nomad client allows without raising
// its own max_kill_timeout.
const StopTimeout = 30 * time.Second

// CanaryTag is what a replicated service's new copies register with while they
// start, in place of the tags that route to them. Neither ingress nor the
// resolver looks for it, so a copy is sent nothing until it has passed its
// checks and Nomad promotes it to the real tags.
const CanaryTag = "orca-canary"

// Meta keys orca stamps on every job it owns. ManagedKey is what makes "which
// jobs are mine" answerable, so orca never touches a job someone else created.
const (
	MetaManaged = "orca.managed"
	MetaApp     = "orca.app"
	MetaService = "orca.service"
	MetaImage   = "orca.image"

	// MetaImageRef is the image as the manifest names it, before pinning, so
	// a registry that cannot be reached can fall back to what that same name
	// resolved to last time rather than stopping every apply.
	MetaImageRef = "orca.imageref"
	MetaAuth     = "orca.authhash"

	// MetaNetwork marks a platform job placed for a cluster of more than one
	// machine, where "internal" is the private NIC rather than the container
	// bridge. See platformJob.
	MetaNetwork = "orca.network"
)

// Options are the cluster facts a manifest cannot know, since it lives in its
// own repo.
type Options struct {
	// Datacenter is the Nomad datacenter, from cluster.yaml.
	Datacenter string

	// Ingress reports whether the front door runs at all. A port routed
	// through it on a cluster with ingress off would deploy, pass its health
	// check and be reachable by nobody, so apply refuses it instead.
	Ingress bool

	// TLS reports whether ingress has certificates, which decides the
	// entrypoint a route attaches to. A route that asks for TLS on a front
	// door with no certificate resolver is one Traefik never serves.
	TLS bool

	// DataDir is where host paths for volumes are rooted.
	DataDir string

	// DNS enables the cluster resolver: tasks are pointed at it and given
	// their group's search domains, which is what makes a bare `db` mean this
	// group's db. Off means services have no way to find each other by name.
	DNS bool

	// InternalNetwork is the host network an internal service port is
	// published on. Empty publishes no host port at all: the service registers
	// its own allocation address, which every container on the machine can
	// reach directly over the bridge and nothing outside it can reach at all.
	//
	// That is right whenever every service is on one machine. Allocation
	// addresses come from a per-node bridge using the same CIDR everywhere, so
	// once there is a second machine they stop being unambiguous and a host
	// port on the private network is needed instead.
	InternalNetwork string

	// Node is the machine this service is pinned to, resolved by apply. Empty
	// leaves placement to Nomad.
	Node string
}

// Build turns one service into a Nomad job. image must already be
// digest-pinned; resolving it is apply's job, not this function's, so the
// jobspec is a pure function of its inputs and can be tested without a network.
func Build(m *manifest.Manifest, s *manifest.Service, image string, opts Options) (*nomad.Job, error) {
	id := JobID(m.App, s.Name)

	ports, portLabel, allocAddressed := buildPorts(s, opts)

	network := &nomad.NetworkResource{Mode: "bridge"}

	if opts.DNS {
		// Set on the network rather than the task. In bridge mode every task
		// joins the allocation's network namespace, and Docker refuses to
		// configure DNS on a container that did not create the namespace it
		// is using ("conflicting options: dns and the network mode"). The
		// resolver settings belong to the namespace, which is what this block
		// configures.
		network.DNS = &nomad.DNSConfig{
			Servers:  []string{DNSAddress},
			Searches: SearchDomains(m.App),
		}
	}
	for _, p := range ports {
		network.ReservedPorts = append(network.ReservedPorts, nomad.Port{
			Label: p.label, Value: p.host, To: p.container, HostNetwork: p.network,
		})
	}

	task := &nomad.Task{
		Name:   s.Name,
		Driver: "docker",
		Config: map[string]any{
			"image": image,
			// Nomad maps these labels into the container's netns; without the
			// list the reserved host ports are allocated but never published.
			"ports": portLabels(ports),
		},
		Resources: &nomad.Resources{
			CPU:      ptr(int(s.CPU) * MHzPerVCPU / 1000),
			MemoryMB: ptr(s.Memory.Megabytes()),
		},
		Env: plainEnv(s),
	}

	var extraTasks []*nomad.Task

	if tmpl := s.Tmpl(); tmpl != nil {
		spec := tmpl.Spec()

		// The template's own arguments, then whatever it derives from the
		// size asked for, appended to the image's entrypoint, which is how
		// the postgres image takes server settings.
		args := append([]string(nil), spec.Args...)
		if spec.Tune != nil {
			args = append(args, spec.Tune(s.Memory)...)
		}
		if len(args) > 0 {
			task.Config["args"] = args
		}

		// The secret path prefix every templated file interpolates, so a
		// template's body names its own secrets without knowing the group.
		prefix := SecretPath(m.App, s.Name)

		if len(spec.Entrypoint) > 0 {
			task.Config["entrypoint"] = spec.Entrypoint
		}

		if spec.SharedMemory != nil {
			task.Config["shm_size"] = int64(spec.SharedMemory(s.Memory))
		}

		if spec.Config != nil {
			task.Templates = append(task.Templates, &nomad.Template{
				EmbeddedTmpl: ptr(fmt.Sprintf(spec.Config.Body, prefix)),
				DestPath:     ptr(spec.Config.Path),
				ChangeMode:   ptr("restart"),
			})
		}

		if spec.Init != nil {
			extraTasks = append(extraTasks, initTask(m, s, image, spec, prefix))
		}
	}

	if s.Cmd != "" {
		// The entrypoint is overridden, not just the command. Docker's command
		// and args replace an image's CMD but leave its ENTRYPOINT in front, so
		// setting only those on an image that has one runs
		// `<entrypoint> /bin/sh -c <cmd>`: the flags land on the wrong binary
		// and the container dies with something unhelpful like "Too many
		// arguments!". Overriding the entrypoint makes `cmd` mean the same
		// thing on every image.
		//
		// A shell, so `cmd` can be a small script rather than forcing exec-form
		// argv on someone writing three lines of setup. The cost is that an
		// image with no shell (scratch, distroless) cannot use `cmd`; such an
		// image should be run with its own entrypoint instead.
		task.Config["entrypoint"] = []string{"/bin/sh", "-c"}
		cmd, _ := manifest.ShellValue(s.Cmd) // validated
		task.Config["args"] = []string{cmd}
	}

	if tmpl := renderTemplate(m, s); tmpl != "" {
		// env=true makes Nomad read the rendered file as KEY=value pairs into
		// the task's environment. Secrets therefore reach the container from
		// Nomad's own variable store and never pass through orca, a manifest,
		// or a jobspec that anyone can read back.
		task.Templates = append(task.Templates, &nomad.Template{
			EmbeddedTmpl: ptr(tmpl),
			DestPath:     ptr("secrets/" + manifest.EnvFile),
			Envvars:      ptr(true),
			ChangeMode:   ptr("restart"),
		})
	}

	task.Templates = append(task.Templates, secretFiles(m, s)...)

	group := &nomad.TaskGroup{
		Name:     ptr(s.Name),
		Count:    ptr(s.Replicas),
		Networks: []*nomad.NetworkResource{network},
		Tasks:    append([]*nomad.Task{task}, extraTasks...),
		RestartPolicy: &nomad.RestartPolicy{
			Attempts: ptr(3),
			Interval: durPtr("5m"),
			Delay:    durPtr("15s"),
			Mode:     ptr("delay"),
		},
	}

	if s.Volume != nil {
		hostPath := VolumePath(opts.DataDir, m.App, s.Name)
		// A bind mount rather than a Nomad host volume, because a host volume
		// has to be declared in the client config, which would mean
		// re-bootstrapping the machine every time an app grows a volume.
		// Nomad's dynamic host volumes are the eventual upgrade; this keeps the
		// same node-pinning property in the meantime.
		task.Config["volumes"] = []string{hostPath + ":" + s.Volume.Mount}
		task.KillTimeout = ptr(StopTimeout)
	}

	if opts.Node != "" {
		// Pinning is what makes data survive. Nomad knows nothing about a bind
		// mount, so without this constraint a reschedule onto another machine
		// would start the service against an empty directory and look healthy
		// while serving nothing.
		group.Constraints = append(group.Constraints, &nomad.Constraint{
			LTarget: "${node.unique.name}",
			RTarget: opts.Node,
			Operand: "=",
		})
	}

	if svc := buildService(m, s, portLabel, allocAddressed, opts); svc != nil {
		group.Services = []*nomad.Service{svc}
		// With another copy to take the traffic, one being replaced leaves
		// the catalog first and is stopped only once nothing is still sending
		// it anything. Not with one copy: that one leaving the catalog is the
		// outage, and waiting would only make it longer before the new one
		// can start.
		if s.Replicas > 1 {
			group.ShutdownDelay = ptr(DrainDelay)
		}
	}
	if svc := metricsService(m, s, opts); svc != nil {
		group.Services = append(group.Services, svc)
	}

	job := &nomad.Job{
		ID:          ptr(id),
		Name:        ptr(id),
		Type:        ptr("service"),
		Datacenters: []string{opts.Datacenter},
		TaskGroups:  []*nomad.TaskGroup{group},
		Meta: map[string]string{
			MetaManaged:  "true",
			MetaApp:      m.App,
			MetaService:  s.Name,
			MetaImage:    image,
			MetaImageRef: s.ResolvedImage(),
		},
		Update: &nomad.UpdateStrategy{
			MaxParallel:      ptr(1),
			MinHealthyTime:   durPtr("10s"),
			HealthyDeadline:  ptr(HealthyDeadline),
			ProgressDeadline: ptr(ProgressDeadline),
			AutoRevert:       ptr(true),
		},
	}

	if blueGreen(s, ports, group) {
		// A new copy of every replica starts alongside the old ones and is
		// sent traffic only once all of them are healthy; the old copies then
		// leave the catalog, drain, and stop. A rolling update replaces one at
		// a time instead, and routes to each new copy the moment it is
		// registered, which is before its image has even been pulled.
		job.Update.Canary = ptr(s.Replicas)
		job.Update.AutoPromote = ptr(true)
		group.Services[0].CanaryTags = []string{CanaryTag}
	}

	// A service with no health check has nothing to gate a rollout on, so
	// asking Nomad to wait for checks would hang the deploy forever.
	if group.Services != nil && len(group.Services[0].Checks) > 0 {
		job.Update.HealthCheck = ptr("checks")
	} else {
		job.Update.HealthCheck = ptr("task_states")
	}

	return job, nil
}

// blueGreen reports whether a service is deployed by starting a full new set
// of copies before stopping the old one.
//
// Only a service that already runs several copies, since that is its
// author saying two can run at once; and only one that takes traffic, since
// that is what the swap protects. Never one with a volume, where two copies
// would share one data directory, or with a port reserved on the machine,
// where the new copy could not bind it while the old one holds it.
func blueGreen(s *manifest.Service, ports []portSpec, group *nomad.TaskGroup) bool {
	return s.Replicas > 1 && s.Volume == nil && len(ports) == 0 && len(group.Services) > 0
}

// initTask finishes setting up a template that is not usable the moment its
// process starts.
//
// A poststart hook in the same allocation, so it shares the network namespace
// and reaches the service on loopback. That is why the service's admin
// interface never has to be reachable from anywhere else. It is not a sidecar:
// it does its work once and exits, and it runs again on every deploy, which is
// why everything it does has to be idempotent.
func initTask(m *manifest.Manifest, s *manifest.Service, image string, spec manifest.TemplateSpec, prefix string) *nomad.Task {
	bucket := BucketName(m.App, s.Name)

	initImage := spec.Init.Image
	if initImage == "" {
		initImage = image
	}

	return &nomad.Task{
		Name:   s.Name + "-init",
		Driver: "docker",
		Config: map[string]any{
			"image":      initImage,
			"entrypoint": []string{"/bin/sh", "-c"},
			"args":       []string{"sh /" + spec.Init.Path},
		},
		Lifecycle: &nomad.TaskLifecycle{Hook: "poststart", Sidecar: false},
		Resources: &nomad.Resources{CPU: ptr(100), MemoryMB: ptr(128)},
		Templates: []*nomad.Template{{
			EmbeddedTmpl: ptr(fmt.Sprintf(spec.Init.Body, prefix, bucket)),
			DestPath:     ptr(spec.Init.Path),
			ChangeMode:   ptr("noop"),
		}},
	}
}

// VolumeOwner is the uid that must own a service's volume directory, or 0 to
// leave it to root. A bind mount keeps the host's ownership, so an image that
// drops privileges cannot write to a directory orca created as root.
func VolumeOwner(s *manifest.Service) int {
	if t := s.Tmpl(); t != nil {
		return t.Spec().VolumeUID
	}
	return 0
}

type portSpec struct {
	label     string
	container int
	host      int

	// network is the host network the port binds. Always stated: "public"
	// for a raw port, which is the one way anything is published on the
	// public interface, and the internal network otherwise.
	network string
}

func portLabels(ports []portSpec) []string {
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, p.label)
	}
	return out
}

// buildPorts derives the job's published ports from the service's ports map.
//
// It reports the service's port label, and whether that label is an allocation
// port (a container port with no host mapping) rather than a host port.
//
// Two questions, answered separately. *Who can reach a port* is its kind: a
// raw port is public; internal and ingress-routed ports are not. *How it is
// addressed* is the machine count, and is the same for every port that is not
// public: on one machine by the allocation's own address, which every
// container and ingress reach over the bridge and nothing outside can; above
// one, published on the internal network at the container's own port number,
// because an allocation address is ambiguous there and a resolver answers
// with an address and no port.
//
// HTTP ports are not a third case. A dynamic port on the public interface,
// kept unreachable only by the firewall, would break the rule that nothing is
// published publicly unless a manifest names a protocol, and above one
// machine it would be unreachable by ingress on any other node.
func buildPorts(s *manifest.Service, opts Options) (ports []portSpec, portLabel string, allocAddressed bool) {
	byContainer := map[int]string{}

	for _, cport := range s.PortNumbers() {
		p := s.Ports[cport]
		label := fmt.Sprintf("p%d", cport)

		switch p.Kind {
		case manifest.PortRaw:
			// One reservation per distinct host port: two protocols usually
			// share a number, and Nomad reserves the number rather than the
			// protocol. The firewall is what opens tcp, udp or both, from the
			// same declaration.
			seen := map[int]bool{}
			for _, b := range p.Binds {
				host := b.HostPort(cport)
				if seen[host] {
					continue
				}
				seen[host] = true
				l := label
				if len(seen) > 1 {
					l = fmt.Sprintf("%s-%d", label, host)
				}
				ports = append(ports, portSpec{label: l, container: cport, host: host, network: "public"})
				if byContainer[cport] == "" {
					byContainer[cport] = l
				}
			}

		case manifest.PortInternal, manifest.PortMetrics, manifest.PortDomain:
			if opts.InternalNetwork != "" {
				ports = append(ports, portSpec{label: label, container: cport, host: cport, network: opts.InternalNetwork})
				byContainer[cport] = label
			}
		}
	}

	sort.Slice(ports, func(i, j int) bool { return ports[i].label < ports[j].label })

	primary := s.PrimaryPort()
	if primary == 0 {
		// Nothing listens, so there is nothing to register or check.
		return ports, "", false
	}
	if label := byContainer[primary]; label != "" {
		return ports, label, false
	}
	// Not published, on a single machine: addressed by the allocation.
	return ports, strconv.Itoa(primary), true
}

// buildService registers the service in Nomad's catalog so siblings can find it
// and Traefik can route to it. A service with no port registers nothing.
func buildService(m *manifest.Manifest, s *manifest.Service, portLabel string, allocAddressed bool, opts Options) *nomad.Service {
	if portLabel == "" {
		return nil
	}

	check := nomad.ServiceCheck{
		Type:     "tcp",
		Interval: dur("10s"),
		Timeout:  dur("2s"),
	}

	svc := &nomad.Service{
		Name:      CatalogName(m.App, s.Name),
		PortLabel: portLabel,
		Provider:  "nomad",
		Tags:      append([]string{DNSTag(m.App, s.Name)}, traefikTags(m, s, opts)...),
	}

	if allocAddressed {
		// The catalog holds the container's own address and container port, so
		// what is discovered is the service itself rather than a hole punched
		// in the machine to reach it.
		svc.AddressMode = "alloc"
		check.AddressMode = "alloc"
		check.PortLabel = portLabel
	}

	svc.Checks = []nomad.ServiceCheck{check}
	return svc
}

// metricsService registers a service's metrics port as a scrape target.
//
// Every target shares one catalog name, and the metric store's scrape config
// ranges over it, so a service that grows a metrics port is scraped from the
// apply that deployed it, without the metric store being redeployed. Its
// group and service travel as tags because a catalog name cannot be split
// back into them, and they become the series' labels.
//
// No DNS tag, so the resolver never answers for it, and no health check: a
// scrape that fails is already visible as a down target, and gating a rollout
// on the metrics endpoint would hold back a deploy for the wrong reason.
func metricsService(m *manifest.Manifest, s *manifest.Service, opts Options) *nomad.Service {
	cport := s.MetricsPort()
	if cport == 0 {
		return nil
	}

	svc := &nomad.Service{
		Name:     MetricsCatalogName,
		Provider: "nomad",
		Tags:     MetricsTags(m.App, s.Name),
	}

	// Addressed the way buildPorts published it: on one machine by the
	// allocation's own address, which the host-networked metric store
	// reaches over the bridge; above one, at the port bound on the internal
	// network.
	if opts.InternalNetwork != "" {
		svc.PortLabel = fmt.Sprintf("p%d", cport)
	} else {
		svc.PortLabel = strconv.Itoa(cport)
		svc.AddressMode = "alloc"
	}
	return svc
}

// traefikTags produce HTTP routing for the one port (at most) routed by
// hostname: the one its manifest names. Traefik reads these straight off the
// Nomad catalog, and routes to the address and port registered alongside
// them, which is why the routed port is the one registered.
//
// The entrypoint and resolver are the names ingress's own configuration
// defines. They must match exactly, because Traefik drops a route whose
// entrypoint is missing.
func traefikTags(m *manifest.Manifest, s *manifest.Service, opts Options) []string {
	for _, cport := range s.PortNumbers() {
		ex := s.Ports[cport]
		if ex.Kind != manifest.PortDomain {
			continue
		}
		host := ex.Domain

		router := CatalogName(m.App, s.Name)
		tags := []string{
			"traefik.enable=true",
			fmt.Sprintf("traefik.http.routers.%s.rule=Host(`%s`)", router, host),
		}
		if opts.TLS {
			tags = append(tags,
				fmt.Sprintf("traefik.http.routers.%s.entrypoints=%s", router, EntryPointHTTPS),
				fmt.Sprintf("traefik.http.routers.%s.tls.certresolver=%s", router, CertResolver))
		} else {
			tags = append(tags, fmt.Sprintf("traefik.http.routers.%s.entrypoints=%s", router, EntryPointHTTP))
		}
		return tags
	}
	return nil
}

// plainEnv is the env that can go straight into the jobspec: every value
// with no secret in it, with $$ already turned into $.
func plainEnv(s *manifest.Service) map[string]string {
	out := map[string]string{}
	for k, v := range s.Env {
		parts, err := manifest.ParseEnvValue(v)
		if err != nil || manifest.HasSecret(parts) {
			continue
		}
		out[k] = manifest.LiteralValue(parts)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func ptr[T any](v T) *T { return &v }

// dur parses a duration literal that is checked by the tests, so a typo here
// fails the build's test run rather than becoming a zero duration on the box.
func dur(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic("deploy: bad duration literal " + s + ": " + err.Error())
	}
	return d
}

func durPtr(s string) *time.Duration {
	d := dur(s)
	return &d
}
