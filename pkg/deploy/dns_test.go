package deploy

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"text/template"
)

func TestDNSNames(t *testing.T) {
	if got := DNSName("blog", "db"); got != "db.blog.orca" {
		t.Errorf("DNSName = %q, want db.blog.orca", got)
	}
	// The search domains are what make a bare `db` mean this group's db, and
	// `db.blog` mean another group's.
	got := SearchDomains("blog")
	if len(got) != 2 || got[0] != "blog.orca" || got[1] != "orca" {
		t.Errorf("SearchDomains = %v, want [blog.orca orca]", got)
	}
}

// The hosts file comes from the catalog, by tag, so any service that
// registers resolves, whichever apply deployed it. A list baked into the
// resolver's job would leave a service added by `orca apply shop` unresolved
// until an apply that included orca's own group.
func TestDNSHostsComeFromTaggedRegistrations(t *testing.T) {
	type svc struct {
		Name string
		Tags []string
	}
	type addr struct{ Address string }
	catalog := []svc{
		{"shop-db", []string{DNSTag("shop", "db")}},
		{"blog-prod-web", []string{DNSTag("blog-prod", "web"), "traefik.enable=true"}},
		{"someone-elses", []string{"x"}},
	}
	addrs := map[string][]addr{
		"shop-db":       {{"172.26.64.5"}},
		"blog-prod-web": {{"172.26.64.6"}, {"172.26.64.7"}},
		"someone-elses": {{"10.9.9.9"}},
	}

	funcs := template.FuncMap{
		"nomadServices": func() []svc { return catalog },
		"nomadService":  func(n string) []addr { return addrs[n] },
		"regexMatch":    func(re, s string) (bool, error) { return regexp.MatchString(re, s) },
		"replaceAll":    func(from, to, s string) string { return strings.ReplaceAll(s, from, to) },
	}
	tmpl, err := template.New("hosts").Funcs(funcs).Parse(dnsHosts())
	if err != nil {
		t.Fatalf("hosts template does not parse: %v", err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, nil); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	for _, want := range []string{
		"\n172.26.64.5 db.shop.orca db.shop\n",
		"\n172.26.64.6 web.blog-prod.orca web.blog-prod\n",
		"\n172.26.64.7 web.blog-prod.orca web.blog-prod",
	} {
		if !strings.Contains(got+"\n", want) {
			t.Errorf("hosts file missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "10.9.9.9") {
		t.Errorf("an untagged registration is not orca's to name:\n%s", got)
	}
}

// Every registration orca makes carries its name, so it can be resolved.
func TestRegistrationsCarryTheirDNSName(t *testing.T) {
	job := buildOneIn(t, "shop", "{name: db, template: postgres:17, volume: 1G}", "db", defaultOpts())
	tags := job.TaskGroups[0].Services[0].Tags
	if len(tags) == 0 || tags[0] != "orca-dns=db.shop" {
		t.Errorf("tags = %v, want orca-dns=db.shop", tags)
	}

	all := platformOpts()
	all.DNS = &DNSSpec{}
	for _, j := range BuildPlatform(all) {
		for _, s := range j.TaskGroups[0].Services {
			if s.PortLabel == "" && j.Meta[MetaService] == "vector" {
				continue
			}
			// One registration per machine, there to be scraped: its tags are
			// its series' labels, and a name that answered with every machine
			// at once would find nothing in particular.
			if j.Meta[MetaService] == "node-exporter" {
				continue
			}
			if want := DNSTag(OrcaApp, j.Meta[MetaService]); len(s.Tags) == 0 || s.Tags[0] != want {
				t.Errorf("%s: tags = %v, want %s", j.Meta[MetaService], s.Tags, want)
			}
		}
	}
}

// Scoping the hosts file to the orca zone would mean "db.blog", which is not
// under .orca, never reaching it and being forwarded upstream to fail.
func TestCorefileServesHostsForEveryName(t *testing.T) {
	cf := corefile()
	if !strings.Contains(cf, ".:53") {
		t.Errorf("hosts must be served from the root zone:\n%s", cf)
	}
	if strings.Contains(cf, Zone+":53") {
		t.Errorf("a zone-scoped block would miss two-label names:\n%s", cf)
	}
	// Anything not an orca name still has to resolve.
	if !strings.Contains(cf, "forward . /etc/resolv.conf") {
		t.Errorf("external names must still resolve:\n%s", cf)
	}
	if !strings.Contains(cf, "bind "+DNSAddress) {
		t.Errorf("the resolver must bind the bridge gateway, not every interface:\n%s", cf)
	}
}

// Every machine needs a resolver it can reach locally.
func TestDNSIsASystemJob(t *testing.T) {
	opts := platformOpts()
	opts.DNS = &DNSSpec{}

	for _, j := range BuildPlatform(opts) {
		if j.Meta[MetaService] != "dns" {
			continue
		}
		if *j.Type != "system" {
			t.Errorf("dns type = %q, want system", *j.Type)
		}
		if len(j.TaskGroups[0].Constraints) != 0 {
			t.Error("a system job must not be pinned to one machine")
		}
		// Restarting the resolver whenever any service moves would take name
		// resolution away from the whole machine for a file it re-reads.
		for _, tmpl := range j.TaskGroups[0].Tasks[0].Templates {
			if strings.Contains(*tmpl.DestPath, "hosts") && *tmpl.ChangeMode != "noop" {
				t.Errorf("hosts file change mode = %q, want noop", *tmpl.ChangeMode)
			}
		}
		return
	}
	t.Fatal("no dns job was built")
}

// In bridge mode every task joins the allocation's network namespace, and
// Docker refuses to configure DNS on a container that did not create the
// namespace it uses. The settings belong to the network, not the task.
func TestResolverIsConfiguredOnTheNetwork(t *testing.T) {
	opts := defaultOpts()
	opts.DNS = true

	job := buildOneIn(t, "blog", "{name: api, image: i:1}", "api", opts)

	net := job.TaskGroups[0].Networks[0]
	if net.DNS == nil {
		t.Fatal("resolver settings must be on the network block")
	}
	if len(net.DNS.Servers) != 1 || net.DNS.Servers[0] != DNSAddress {
		t.Errorf("servers = %v, want the bridge gateway", net.DNS.Servers)
	}
	if len(net.DNS.Searches) != 2 || net.DNS.Searches[0] != "blog.orca" {
		t.Errorf("searches = %v, want the group's domains first", net.DNS.Searches)
	}

	cfg := job.TaskGroups[0].Tasks[0].Config
	if _, set := cfg["dns_servers"]; set {
		t.Error("dns on the task is rejected by Docker in bridge mode")
	}
}

func TestNoResolverConfigWhenDNSIsOff(t *testing.T) {
	job := buildOneIn(t, "blog", "{name: api, image: i:1}", "api", defaultOpts())
	if job.TaskGroups[0].Networks[0].DNS != nil {
		t.Error("with dns disabled, tasks should keep the machine's resolver")
	}
}
