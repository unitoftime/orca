package deploy

import (
	"fmt"
	"strings"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/internal/manifest"
)

// Ingress's entrypoints and certificate resolver, named once. The routes a
// service carries in its catalog tags must name exactly what Traefik's own
// configuration defines, or Traefik drops the route.
const (
	EntryPointHTTP  = "http"
	EntryPointHTTPS = "https"
)

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
		Name:      CatalogName(manifest.ReservedGroup, "certs"),
		Tags:      []string{DNSTag(manifest.ReservedGroup, "certs")},
		PortLabel: "http",
		Provider:  "nomad",
		Checks: []nomad.ServiceCheck{{
			Type: "http", Path: "/healthz", Interval: dur("15s"), Timeout: dur("3s"),
		}},
	}}
	return job
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
		Name:      CatalogName(manifest.ReservedGroup, "traefik"),
		Tags:      []string{DNSTag(manifest.ReservedGroup, "traefik")},
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

// How many requests a second one address may make of the dashboards, and how
// many at once. Enough for a page that loads a few dozen files and then
// polls, and a ceiling on how fast a password can be guessed from one place.
const (
	dashboardRequestsPerSecond = 10
	dashboardRequestBurst      = 100
)

// notFromAnotherSite is the part of a dashboard's route that refuses a
// request another site's page told the browser to make.
//
// A browser that is logged in to a dashboard sends its password with every
// request there, including one a page elsewhere asks for: a form that posts
// to the metric store, say. The browser also says where each request came
// from, in Sec-Fetch-Site, so those are simply not routed. What is let
// through from elsewhere is following a link to a page, which is how the
// status page's own links to the other two arrive, and which only reads.
// Anything that sends no such header is not a browser being steered.
const notFromAnotherSite = "!(HeaderRegexp(`Sec-Fetch-Site`, `^(cross-site|same-site)$`)" +
	" && !(Method(`GET`) && Header(`Sec-Fetch-Mode`, `navigate`) && Header(`Sec-Fetch-Dest`, `document`)))"

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
		boards = append(boards, dashboard{name: "certs", service: CatalogName(manifest.ReservedGroup, "certs"), fallback: "http://127.0.0.1:1", internal: true})
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
		// The limit comes first, so a wrong password costs a guess against
		// it as well: one shared password stands between the internet and
		// every log line, and checking one is slow on purpose.
		b.WriteString("  middlewares:\n    dashboard-auth:\n      basicAuth:\n        users:\n")
		fmt.Fprintf(&b, "          - %q\n", "admin:"+opts.Ingress.AuthHash)
		fmt.Fprintf(&b, "    dashboard-limit:\n      rateLimit:\n        average: %d\n        burst: %d\n",
			dashboardRequestsPerSecond, dashboardRequestBurst)
	}

	b.WriteString("  routers:\n")
	for _, d := range boards {
		if d.internal {
			continue
		}
		fmt.Fprintf(&b, `    %s:
      rule: "Host(`+"`"+`%s.%s`+"`"+`) && %s"
      service: %s
      middlewares: [dashboard-limit, dashboard-auth]
`, d.name, d.name, opts.Domain, notFromAnotherSite, d.name)
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
			service: CatalogName(manifest.ReservedGroup, "status"),
			// Its port is dynamic, so there is no address to fall back to.
			// Nothing listens on port 1: a 502 until it registers, which is
			// the truth.
			fallback: "http://127.0.0.1:1",
		})
	}
	if opts.Logs != nil {
		boards = append(boards, dashboard{
			name:     "logs",
			service:  CatalogName(manifest.ReservedGroup, "victorialogs"),
			fallback: fmt.Sprintf("http://127.0.0.1:%d", LogsPort),
		})
	}
	if opts.Metrics != nil {
		boards = append(boards, dashboard{
			name:     "metrics",
			service:  CatalogName(manifest.ReservedGroup, "victoriametrics"),
			fallback: fmt.Sprintf("http://127.0.0.1:%d", MetricsPort),
		})
	}
	return boards
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

// dashboardURL is where a published UI is reached. Over HTTPS, always: they
// are not published without it.
func dashboardURL(opts PlatformOptions, name string) string {
	return "https://" + name + "." + opts.Domain
}
