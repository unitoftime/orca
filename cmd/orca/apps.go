package main

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/unitoftime/orca/internal/deploy"
	"github.com/unitoftime/orca/internal/manifest"
)

// loadGroups discovers and validates every group under the cluster root, then
// checks the constraints that span groups.
//
// There is no index to maintain: the directories beside cluster.yaml are the
// index. A group added by creating a directory is deployed by the next apply,
// and one removed by deleting a directory is stopped by it.
func loadGroups(cfg Config) ([]*manifest.Manifest, error) {
	groups, err := discoverGroups(cfg)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("no groups found in %s; a group is a directory of service files beside %s",
			cfg.Root, manifest.ClusterFile)
	}
	return groups, nil
}

// discoverGroups is loadGroups without requiring that any group exists. An
// empty tree is a real answer (every group has been deleted), and the
// questions purge asks ("is this still declared?") need it rather than an
// error.
func discoverGroups(cfg Config) ([]*manifest.Manifest, error) {
	groups, err := manifest.Discover(cfg.Root)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(checkJobIDs(groups), checkHostPorts(groups, machinePorts(cfg)),
		checkHostnames(groups, platformHostnames(cfg))); err != nil {
		return nil, err
	}
	return groups, nil
}

// checkJobIDs catches two services resolving to the same Nomad job name. Job
// IDs are <group>-<service>, so group "blog-db" with service "x" collides with
// group "blog" with service "db-x". Vanishingly rare, and silently catastrophic
// if it happened: the two would deploy over each other, each apply undoing the
// last.
func checkJobIDs(groups []*manifest.Manifest) error {
	type owner struct{ group, service, path string }
	taken := map[string]owner{}
	var errs []error

	for _, m := range groups {
		for _, s := range m.Services {
			// A templated service that can back itself up reserves its backup
			// job's name too, so a group with a `db` template and an ordinary
			// service called `db-backup` is refused before either is declared
			// with a backup.
			ids := deploy.ServiceJobIDs(m.App, s)
			for _, id := range ids {
				if prev, exists := taken[id]; exists {
					errs = append(errs, fmt.Errorf(
						"job name %q is produced by both %s/%s (%s) and %s/%s (%s); rename one",
						id, prev.group, prev.service, prev.path, m.App, s.Name, s.SourceFile()))
					continue
				}
				taken[id] = owner{group: m.App, service: s.Name, path: s.SourceFile()}
			}
		}
	}
	return errors.Join(errs...)
}

// platformHostnames are the names orca's own dashboards are served at, which
// no service may claim, whether or not the dashboards are published now.
func platformHostnames(cfg Config) map[string]string {
	out := map[string]string{}
	if d := cfg.Monitoring.Domain; d != "" {
		for _, name := range []string{"status", "logs", "metrics"} {
			out[name+"."+d] = "orca's dashboards"
		}
	}
	return out
}

// checkHostnames refuses two services claiming one hostname. Ingress would
// take both routes, and which one a request reaches would be up to its rule
// priorities rather than anything written here; a service claiming a
// dashboard's name could sit in front of its password.
func checkHostnames(groups []*manifest.Manifest, reserved map[string]string) error {
	taken := map[string]string{}
	for host, owner := range reserved {
		taken[strings.ToLower(host)] = owner
	}
	var errs []error
	for _, m := range groups {
		for _, s := range m.Services {
			for _, cport := range s.PortNumbers() {
				p := s.Ports[cport]
				if p.Kind != manifest.PortDomain {
					continue
				}
				host := strings.ToLower(p.Domain)
				who := fmt.Sprintf("%s/%s (%s)", m.App, s.Name, s.SourceFile())
				if prev, ok := taken[host]; ok {
					errs = append(errs, fmt.Errorf("hostname %s is claimed by %s and %s", host, prev, who))
					continue
				}
				taken[host] = who
			}
			// A name a service serves TLS for by itself is claimed like any
			// other: a certificate has one owner, which is what decides when
			// it is no longer needed.
			if s.TLS != "" {
				who := fmt.Sprintf("%s/%s (%s)", m.App, s.Name, s.SourceFile())
				if prev, ok := taken[s.TLS]; ok {
					errs = append(errs, fmt.Errorf("hostname %s is claimed by %s and %s", s.TLS, prev, who))
					continue
				}
				taken[s.TLS] = who
			}
		}
	}
	return errors.Join(errs...)
}

// machinePort is a host port the machine itself holds before any manifest
// asks for one, and what holds it.
type machinePort struct {
	proto, owner string
	port         int
}

// machinePorts are the public ports no service may claim. sshd is listening
// on 22 whatever orca does, and ingress takes 80 and 443 when it runs. Nomad
// knows about neither (sshd is not its job, and Traefik binds through host
// networking), so a service asking for one of these places without complaint
// and then fails on the box with "address already in use".
func machinePorts(cfg Config) []machinePort {
	out := []machinePort{{proto: "tcp", port: 22, owner: "ssh"}}
	if cfg.Ingress.Enabled {
		out = append(out,
			machinePort{proto: "tcp", port: 80, owner: "ingress (orca/traefik)"},
			machinePort{proto: "tcp", port: 443, owner: "ingress (orca/traefik)"})
	}
	return out
}

// checkHostPorts enforces the machine's port space across every group. A group
// can only see its own claims, so this is the only place a collision between
// two groups can be caught before it becomes a placement failure on the box.
func checkHostPorts(groups []*manifest.Manifest, reserved []machinePort) error {
	type claim struct{ group, service, path string }
	taken := map[string]claim{}
	held := map[string]string{}
	for _, r := range reserved {
		held[fmt.Sprintf("%s/%d", r.proto, r.port)] = r.owner
	}
	var errs []error

	for _, m := range groups {
		for _, hp := range m.HostPorts() {
			key := fmt.Sprintf("%s/%d", hp.Proto, hp.Port)
			if owner, ok := held[key]; ok {
				errs = append(errs, fmt.Errorf(
					"%s/%s (%s): host port %d/%s is already taken by %s; use another port, or a hostname port to go through ingress",
					hp.App, hp.Service, m.Path, hp.Port, hp.Proto, owner))
				continue
			}
			if prev, exists := taken[key]; exists {
				errs = append(errs, fmt.Errorf(
					"host port %d/%s is claimed by %s/%s (%s) and %s/%s (%s)",
					hp.Port, hp.Proto, prev.group, prev.service, prev.path, hp.App, hp.Service, m.Path))
				continue
			}
			taken[key] = claim{group: hp.App, service: hp.Service, path: m.Path}
		}
	}
	return errors.Join(errs...)
}

// cmdValidate parses every group and reports what it found. It touches no
// machine, so it is the check to run in CI and the one to run while editing.
func cmdValidate(cfg Config, args []string) error {
	groups, err := loadGroups(cfg)
	if err != nil {
		return err
	}

	groups, err = selectGroups(groups, args)
	if err != nil {
		return err
	}

	for _, m := range groups {
		fmt.Printf("%s (%s)\n", m.App, m.Path)
		for _, s := range m.Services {
			// A target runs no container, so the sizing columns would all read
			// zero. What it is and where it points is the whole of it.
			if s.IsTarget() {
				fmt.Printf("  %-14s %-34s -> %s/%s\n",
					s.Name, s.Target+" target", s.Endpoint, path.Join(s.Bucket, s.Path))
				continue
			}

			desc := s.ResolvedImage()
			if s.IsTemplated() {
				desc = fmt.Sprintf("%s [%s]", desc, s.Tmpl())
			}
			fmt.Printf("  %-14s %-34s cpu %-5s mem %-6s", s.Name, desc, s.CPU, s.Memory)
			if s.Replicas > 1 {
				fmt.Printf(" x%d", s.Replicas)
			}
			if s.Volume != nil {
				fmt.Printf(" vol %s -> %s", s.Volume.Size, s.Volume.Mount)
			}
			fmt.Println()
			// Whether a database is backed up, and where to, is the fact most
			// worth seeing without having to open the file.
			if s.Backup != nil {
				fmt.Printf("  %-14s   backup -> %s (%s, keep %d)\n",
					"", s.Backup.To, s.Backup.Schedule, s.Backup.Keep)
			}
			for _, cport := range s.PortNumbers() {
				fmt.Printf("  %-14s   %d -> %s\n", "", cport, s.Ports[cport])
			}
		}
		if secrets := m.Secrets(); len(secrets) > 0 {
			fmt.Printf("  secrets: %s\n", strings.Join(secrets, ", "))
		}
		// What each ${var.NAME} became. The images above already show it
		// filled in; this says where the value came from by name.
		if len(m.Vars) > 0 {
			names := make([]string, 0, len(m.Vars))
			for n := range m.Vars {
				names = append(names, n)
			}
			sort.Strings(names)
			pairs := make([]string, len(names))
			for i, n := range names {
				pairs[i] = n + "=" + m.Vars[n]
			}
			fmt.Printf("  vars: %s\n", strings.Join(pairs, ", "))
		}
	}

	fmt.Printf("\n%d group(s) ok\n", len(groups))
	return nil
}

// selectGroups narrows the discovered groups to the named ones.
func selectGroups(groups []*manifest.Manifest, names []string) ([]*manifest.Manifest, error) {
	if len(names) == 0 {
		return groups, nil
	}

	byName := map[string]*manifest.Manifest{}
	for _, m := range groups {
		byName[m.App] = m
	}

	var out []*manifest.Manifest
	var errs []error
	for _, n := range names {
		if n == deploy.OrcaApp {
			// The platform is selectable but is not a directory on disk.
			continue
		}
		m, ok := byName[n]
		if !ok {
			errs = append(errs, fmt.Errorf("no group named %q; known groups: %s", n, knownGroups(groups)))
			continue
		}
		out = append(out, m)
	}
	return out, errors.Join(errs...)
}

func knownGroups(groups []*manifest.Manifest) string {
	names := make([]string, 0, len(groups))
	for _, m := range groups {
		names = append(names, m.App)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}
