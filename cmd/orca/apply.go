package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/internal/deploy"
	"github.com/unitoftime/orca/internal/images"
	"github.com/unitoftime/orca/internal/manifest"
	"github.com/unitoftime/orca/internal/registry"
)

// cmdApply converges the cluster to the manifests. With no argument it applies
// every group beside cluster.yaml; named groups narrow the scope, and anything
// outside that scope is left completely alone.
func cmdApply(ctx context.Context, cfg Config, in invocation, planOnly bool) error {
	yes, args := in.Has(flagYes), in.args

	// Every group, kept alongside the scoped set: a backup names its target
	// by <group>/<service>, and that target is usually in a group this apply
	// was not asked to touch.
	all, err := loadGroups(cfg)
	if err != nil {
		return err
	}

	// The target machine. Multi-machine placement is per-service; this is the
	// one orca talks to, which is the first server in the config.
	cluster, err := clusterFor(ctx, cfg)
	if err != nil {
		return err
	}

	// Held from before the cluster is read until the last health check, so
	// nothing this apply plans against can change underneath it. plan only
	// reads, and takes nothing.
	if !planOnly {
		var release func()
		ctx, release, err = cluster.LockApply(ctx)
		if err != nil {
			return err
		}
		defer release()
	}

	current, err := cluster.Jobs(ctx)
	if err != nil {
		return err
	}

	groups, err := scopeGroups(all, args, current)
	if err != nil {
		return err
	}

	// scope is nil for a whole-cluster apply: everything orca owns is in play,
	// including groups whose directory is gone, which is exactly what has to be
	// stopped. Named groups narrow it, and nothing outside is touched.
	var scope map[string]bool
	if len(args) > 0 {
		scope = map[string]bool{}
		for _, a := range args {
			scope[a] = true
		}
	}

	// orca's own jobs are a group you did not write. Including them here means
	// plan, apply, health and status treat them exactly like anything else,
	// with no separate verb and no special cases, and a capability switched
	// off in cluster.yaml is simply a service orca no longer declares, so the
	// ordinary "stop what is no longer declared" rule removes it.
	platformInScope := len(args) == 0
	for _, a := range args {
		if a == manifest.ReservedGroup {
			platformInScope = true
		}
	}

	// A database with a `backup:` gets a periodic job, and its target is
	// usually in a group this apply was not asked to touch, so it is looked
	// up in every group, not only the ones in scope.
	backups, err := collectBackups(all, groups)
	if err != nil {
		return err
	}

	pins := newImageResolver(ctx, registry.Remote{}, current)

	desired, err := buildJobs(cfg, pins, groups, backups)
	if err != nil {
		return err
	}
	volumeDirs, err := declaredVolumeDirs(cfg, groups)
	if err != nil {
		return err
	}

	// The status page runs a build of orca on the machine, and its job names
	// that build by its hash, so the build is found, and if need be made,
	// before the job can be rendered. Plan does this too; it only ships in
	// apply.
	// The certificate job is the same build, and runs wherever there is
	// HTTPS, since every certificate ingress presents comes from it.
	var statusBin *statusBinary
	if platformInScope && (cfg.Monitoring.Status.Enabled || cfg.Ingress.TLS()) {
		b, err := resolveStatusBinary(ctx)
		if err != nil {
			return err
		}
		statusBin = &b
	}

	if platformInScope {
		// The existing auth hash is read from the cluster first, so an
		// unchanged password produces an unchanged job spec.
		password, err := adminPassword(ctx, cluster, planOnly)
		if err != nil {
			return err
		}
		authHash, err := resolveAuthHash(current, password)
		if err != nil {
			return err
		}
		desired = append(desired, buildPlatformJobs(cfg, authHash, statusBin)...)
	}

	// Every image is submitted as the digest its tag points at right now:
	// orca's own and the ones templates and backups add, not only the ones a
	// manifest names.
	if err := deploy.PinImages(desired, pins.Pin); err != nil {
		return err
	}

	// The machine pulls with its own credentials, not yours, so an image that
	// resolved only because you are logged in would otherwise fail at the
	// pull, after the health timeout, as "unauthorized".
	if err := preflightRegistries(ctx, cluster, pins.private); err != nil {
		return err
	}

	// Every secret the apply depends on is checked before anything is
	// submitted. Nomad blocks a task whose secret is missing, so without this
	// the failure arrives as a two-minute health timeout naming a raft path
	// instead of a secret.
	toGenerate, err := preflightSecrets(ctx, cluster, groups, backups)
	if err != nil {
		return err
	}

	// What the volumes already hold decides whether the jobs can start on
	// them at all, so it is checked before anything is submitted.
	facts, err := inspectVolumes(ctx, cfg, cluster.node, volumeDirs, false)
	if err != nil {
		return err
	}
	if err := checkVolumeData(groups, facts, toGenerate); err != nil {
		return err
	}
	if err := checkVolumePlacement(ctx, cfg, volumeDirs); err != nil {
		return err
	}

	// A certificate a service serves itself is in hand before the service is
	// submitted, for the reason a secret is checked: Nomad blocks a task
	// whose certificate is missing, having already stopped the copy before it.
	// The ones ingress presents are asked for in the same breath.
	certRecords, err := cluster.Certificates(ctx)
	if err != nil {
		return err
	}
	certs := planCerts(wantedCerts(cfg, groups, platformInScope), certRecords, func(group string) bool {
		// Every group is a full apply's to decide for, declared or not: one
		// whose directory is gone has no use for its certificates either.
		// A narrowed apply decides for the groups it names, likewise.
		return len(args) == 0 || slices.Contains(args, group)
	})
	if err := checkCertsPossible(cfg, certs, current, platformInScope); err != nil {
		return err
	}

	plans, err := cluster.PlanJobs(ctx, desired)
	if err != nil {
		return err
	}
	plan := deploy.BuildPlan(desired, current, plans, scope)

	// plan changes nothing: not a job, not a firewall rule, not a secret.
	// What apply would generate is said instead of done.
	if planOnly {
		for _, g := range toGenerate {
			fmt.Printf("  generate secret %s\n", g)
		}
		fmt.Print(certs.String())
		fmt.Println(plan.String())
		return nil
	}

	// Every service the manifests declare, whether or not this apply touched
	// it. Health is checked against this set so a crash loop is reported even
	// when the spec has not moved.
	var inScope []string
	for _, j := range desired {
		inScope = append(inScope, *j.ID)
	}

	fmt.Print(certs.String())
	fmt.Println(plan.String())

	if bad := plan.Unplaceable(); len(bad) > 0 {
		var names []string
		for _, c := range bad {
			names = append(names, c.Group+"/"+c.Service)
		}
		return fmt.Errorf("nothing changed: Nomad cannot place %s", strings.Join(names, ", "))
	}

	// Asked before anything changes, so answering no leaves the machine
	// exactly as it was, including the firewall, which would otherwise
	// already have closed the ports of the services you just declined to
	// stop.
	if plan.HasWork() {
		fmt.Println()
		if err := confirmStops(plan, yes); err != nil {
			return err
		}
	}

	// Before anything is deployed, so a port is open by the time something is
	// listening on it rather than a moment after. Also on a no-op apply,
	// which is what makes rules flushed by hand come back.
	if err := applyFirewall(ctx, cfg, all); err != nil {
		return err
	}

	// Before the jobs they are for, so a build of orca whose own jobs need
	// something new is allowed it by the apply that deploys them.
	if platformInScope {
		if err := cluster.WritePolicies(ctx); err != nil {
			return err
		}
	}

	// A template's secret is orca's to create: the author never writes it and
	// never sees the value.
	if err := createGeneratedSecrets(ctx, cluster, toGenerate); err != nil {
		return err
	}

	// Before the job that mounts it is submitted, and on a no-op apply too,
	// so a build removed from the machine is back before the page restarts.
	if statusBin != nil {
		if err := shipStatusBinary(ctx, *statusBin, binaryHosts(cfg)); err != nil {
			return err
		}
	}

	if !plan.HasWork() && len(certs.Request) == 0 && len(certs.Remove) == 0 {
		return waitForHealth(ctx, cluster, inScope, nil, false)
	}

	if err := ensureVolumeDirs(ctx, cfg, cluster.node, volumeDirs); err != nil {
		return err
	}

	// orca's own jobs first when a certificate is waited for, since the job
	// that issues it may be among them. Everything else is held back until
	// the certificates exist, so a name that cannot be proven changes no
	// service.
	submitted := map[string]bool{}
	if len(certs.Request) > 0 {
		own, rest := splitPlatform(plan)
		if submitted, err = execute(ctx, cluster, own); err != nil {
			return err
		}
		if err := requestCerts(ctx, cluster, certs.Request); err != nil {
			return err
		}
		plan = rest
	}

	more, err := execute(ctx, cluster, plan)
	if err != nil {
		return err
	}
	maps.Copy(submitted, more)

	// After the services that held them were updated or stopped.
	removeCerts(ctx, cluster, certs.Remove)

	return waitForHealth(ctx, cluster, inScope, submitted, true)
}

// splitPlatform divides a plan into the changes to orca's own jobs and the
// rest.
func splitPlatform(plan deploy.Plan) (own, rest deploy.Plan) {
	for _, c := range plan.Changes {
		if c.Group == manifest.ReservedGroup {
			own.Changes = append(own.Changes, c)
		} else {
			rest.Changes = append(rest.Changes, c)
		}
	}
	return own, rest
}

// binaryHosts is every machine a job made of orca's own binary runs on: the
// status page's, and the certificate job's beside ingress.
func binaryHosts(cfg Config) []NodeConfig {
	var hosts []NodeConfig
	add := func(n NodeConfig, err error) {
		if err == nil && !slices.ContainsFunc(hosts, func(h NodeConfig) bool { return h.Host == n.Host }) {
			hosts = append(hosts, n)
		}
	}
	if cfg.Monitoring.Status.Enabled {
		add(cfg.MonitoringNode())
	}
	if cfg.Ingress.TLS() {
		add(cfg.IngressNode())
	}
	return hosts
}

// preflightSecrets checks that every secret the apply needs is set or about
// to be generated, and returns the generated ones that do not exist yet. It
// only reads: plan runs it too.
func preflightSecrets(ctx context.Context, cluster *Cluster, groups []*manifest.Manifest, backups []backedUp) ([]generatedSecret, error) {
	needed := neededSecrets(groups, backups)
	generated := generatedSecrets(groups)
	if len(needed) == 0 && len(generated) == 0 {
		return nil, nil
	}

	set, err := cluster.SecretPaths(ctx)
	if err != nil {
		return nil, err
	}
	return missingGenerated(needed, generated, set)
}

// missingGenerated is preflightSecrets' judgement, without the cluster.
//
// A generated secret not yet created counts as set: apply creates it before
// anything that reads it is submitted, and plan has to be able to say an apply
// would work without creating anything itself.
func missingGenerated(needed []secretRef, generated []generatedSecret, set map[string]bool) ([]generatedSecret, error) {
	var toGenerate []generatedSecret
	willBeSet := map[string]bool{}
	for p := range set {
		willBeSet[p] = true
	}
	for _, g := range generated {
		if !set[g.Path()] {
			toGenerate = append(toGenerate, g)
			willBeSet[g.Path()] = true
		}
	}
	if err := checkMissing(needed, willBeSet); err != nil {
		return nil, err
	}
	return toGenerate, nil
}

// execute applies the plan, running stops before creates and updates.
//
// Creating first would let "a service that replaced another" start before
// the old one goes, but that protects nothing: a stop only ever targets a
// service the manifests no longer declare, and a service that merely changed
// gets an update to the same job ID, never a stop and a create. What creating
// first does cost is exactly the case where the two contend for the same
// resource.
//
// That case is a job ID changing, which is what renaming a group is. Creating
// first would put the new ingress on the machine while the old one still
// holds :80, and the new log shipper against a buffer the old one has locked:
// the replacements would be unplaceable or crash-looping until the thing they
// replace goes away.
//
// The cost is that a failure partway through leaves fewer services running
// rather than more. That is the right trade: what was stopped is what you
// declared you no longer wanted.
//
// It returns the jobs it submitted, which are the ones whose rollout health
// is then waited for.
func execute(ctx context.Context, cluster *Cluster, plan deploy.Plan) (map[string]bool, error) {
	var errs []error
	submitted := map[string]bool{}

	for _, c := range plan.Work() {
		if c.Kind != deploy.ChangeStop {
			continue
		}
		// Removed from Nomad, not merely stopped. A stopped job record is a
		// tombstone that lingers in `status` forever, and it buys nothing:
		// the manifests are the source of truth, so bringing a service back
		// means declaring it again, which redeploys it either way.
		//
		// This deletes no data. The volume stays on disk and `orca status`
		// reports it as orphaned, so "kept" never means "invisible".
		fmt.Printf("  stop    %s/%s ... ", c.Group, c.Service)
		if err := cluster.Stop(ctx, c.JobID); err != nil {
			fmt.Println("FAILED")
			errs = append(errs, err)
			continue
		}
		fmt.Println("ok")
	}

	for _, c := range plan.Work() {
		if c.Kind == deploy.ChangeStop {
			continue
		}
		fmt.Printf("  %-7s %s/%s ... ", c.Kind, c.Group, c.Service)
		if err := cluster.Submit(ctx, c.Job, c.Plan.JobModifyIndex); err != nil {
			fmt.Println("FAILED")
			errs = append(errs, err)
			continue
		}
		submitted[c.JobID] = true
		fmt.Println("ok")
	}

	return submitted, errors.Join(errs...)
}

// buildJobs resolves every image to a digest and renders the jobs.
func buildJobs(cfg Config, pins *imageResolver, groups []*manifest.Manifest, backups []backedUp) ([]*nomad.Job, error) {
	opts := deploy.Options{
		Datacenter: datacenter,
		Ingress:    cfg.Ingress.Enabled,
		TLS:        cfg.Ingress.TLS(),
		DataDir:    dataDir,
	}

	// On one machine an internal port needs no host port at all: every
	// container reaches every other directly over the bridge. Once there is a
	// second machine that address is ambiguous, so the port is published on
	// the private network, never the public interface.
	if cfg.MultiNode() {
		opts.InternalNetwork = "internal"
	}
	opts.DNS = cfg.DNS.Enabled

	var jobs []*nomad.Job
	var errs []error

	imagesByGroup := map[string]map[string]string{}
	for _, m := range groups {
		images, err := resolveImages(pins.Pin, m)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		imagesByGroup[m.Group] = images

		built, err := deploy.BuildGroup(m, images, opts, func(s *manifest.Service) (string, error) {
			return nodeFor(cfg, s)
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		jobs = append(jobs, built...)
	}

	// A database with a `backup:` gets a periodic job. Naming a target is
	// what turns backups on; there is no separate switch to forget.
	for _, b := range backups {
		images, ok := imagesByGroup[b.Manifest.Group]
		if !ok {
			// Its group's images failed to resolve, which is already reported.
			continue
		}
		o := opts
		o.Node, _ = nodeFor(cfg, b.Service)
		job, err := deploy.BuildBackup(b.Manifest, b.Service, images[b.Service.Name], b.Spec, o)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		jobs = append(jobs, job)
	}

	return jobs, errors.Join(errs...)
}

// buildPlatformJobs renders the platform jobs this cluster's config asks for.
// statusBin is the build of orca the status page runs, when it is on.
func buildPlatformJobs(cfg Config, authHash string, statusBin *statusBinary) []*nomad.Job {
	return deploy.BuildPlatform(platformOptions(cfg, authHash, statusBin))
}

// platformOptions turns the cluster config into the platform's resolved
// options. A capability that is switched off becomes a nil spec, which
// BuildPlatform renders as no job at all.
func platformOptions(cfg Config, authHash string, statusBin *statusBinary) deploy.PlatformOptions {
	opts := deploy.PlatformOptions{
		Datacenter: datacenter,
		DataDir:    dataDir,
		Domain:     cfg.Monitoring.Domain,
		Images: deploy.PlatformImages{
			CoreDNS:         images.CoreDNS,
			NodeExporter:    images.NodeExporter,
			Traefik:         images.Traefik,
			Vector:          images.Vector,
			VictoriaLogs:    images.VictoriaLogs,
			VictoriaMetrics: images.VictoriaMetrics,
		},
	}

	// Every platform component owns data on one disk and has to be pinned, at
	// any machine count. Leaving it to Nomad on a multi-machine cluster would
	// let a reschedule start a store against an empty directory and report it
	// healthy while serving nothing.
	if n, err := cfg.MonitoringNode(); err == nil {
		opts.MonitoringNode = n.Name
	}
	if n, err := cfg.IngressNode(); err == nil {
		opts.IngressNode = n.Name
	}

	// Nothing the platform runs for itself binds a public interface. The
	// internal network is the container bridge on one machine and the private
	// NIC once there is a cluster, so the stores are reachable by name from
	// any container and from nowhere outside the machines.
	opts.StoreNetwork = "internal"
	opts.MultiNode = cfg.MultiNode()

	if cfg.DNS.Enabled {
		opts.DNS = &deploy.DNSSpec{}
	}
	if cfg.Ingress.Enabled {
		opts.Ingress = &deploy.IngressSpec{TLS: cfg.Ingress.TLS(), ACMEEmail: cfg.Ingress.ACMEEmail, AuthHash: authHash}
	}
	if cfg.Monitoring.Logs.Enabled {
		opts.Logs = &deploy.LogsSpec{Retention: cfg.Monitoring.Logs.Retention, DiskBytes: cfg.Monitoring.LogDiskBytes()}
	}
	if cfg.Monitoring.Metrics.Enabled {
		opts.Metrics = &deploy.MetricsSpec{Retention: cfg.Monitoring.Metrics.Retention, MinFreeBytes: cfg.Monitoring.MetricsMinFreeBytes()}
	}
	if cfg.Monitoring.Status.Enabled {
		opts.Status = &deploy.StatusSpec{Image: images.Alpine}
		if statusBin != nil {
			opts.Status.Binary = statusBin.Remote()
		}
	}
	if cfg.Ingress.TLS() {
		opts.Certs = &deploy.CertsSpec{Image: images.Alpine, Directory: cfg.Ingress.ACMEDirectory}
		if statusBin != nil {
			opts.Certs.Binary = statusBin.Remote()
		}
	}
	return opts
}

// resolveImages pins every tag to the digest it points at right now.
func resolveImages(pin func(ref string) (string, error), m *manifest.Manifest) (map[string]string, error) {
	out := make(map[string]string, len(m.Services))
	var errs []error

	for _, s := range m.Services {
		if s.IsTarget() {
			// A target names somewhere outside the cluster; there is no
			// container and therefore no image to pin.
			continue
		}
		ref := s.ResolvedImage()
		pinned, err := pin(ref)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: service %q: %w", m.Path, s.Name, err))
			continue
		}
		out[s.Name] = pinned
	}
	return out, errors.Join(errs...)
}

// nodeFor decides which machine a service is pinned to.
//
// A service with no volume is left to Nomad. A service with one has to be
// pinned, because its data is on exactly one disk and a reschedule elsewhere
// would start it against an empty directory and look perfectly healthy while
// serving nothing.
//
// Unless it says otherwise with `node:`, that disk is the first server's:
// the machine orca keeps its own stores on. On one machine that is the only
// machine, so it is also where every volume already is when a second one is
// added: the data stays put, and growing the cluster needs no edit to any
// manifest. Refusing instead would make adding a machine break every database
// until each was pinned by hand to the place it already is.
func nodeFor(cfg Config, s *manifest.Service) (string, error) {
	if s.Node != "" {
		if _, ok := cfg.FindNode(s.Node); !ok {
			return "", fmt.Errorf("node %q is not in the cluster config", s.Node)
		}
		return s.Node, nil
	}

	if s.Volume == nil {
		return "", nil
	}

	host, err := cfg.HostNode()
	if err != nil {
		return "", err
	}
	return host.Name, nil
}

// scopeGroups resolves the groups an apply names into the manifests it
// deploys.
//
// A name with no directory is still accepted while orca is running jobs for
// it: that is how you remove a group you have already deleted, and it
// contributes nothing to deploy; its jobs are in scope only to be stopped.
// Decided per name, not for the whole list, so `orca apply shop oldgroup`
// keeps shop's manifests although oldgroup's are missing, rather than
// planning to stop every service shop runs.
func scopeGroups(all []*manifest.Manifest, names []string, current map[string]deploy.JobState) ([]*manifest.Manifest, error) {
	var keep []string
	for _, n := range names {
		if n != manifest.ReservedGroup && !hasGroup(all, n) && runningGroup(current, n) {
			continue
		}
		keep = append(keep, n)
	}
	if len(names) > 0 && len(keep) == 0 {
		return nil, nil
	}
	return selectGroups(all, keep)
}

func hasGroup(groups []*manifest.Manifest, name string) bool {
	for _, m := range groups {
		if m.Group == name {
			return true
		}
	}
	return false
}

// runningGroup reports whether orca is running any job for the group.
func runningGroup(current map[string]deploy.JobState, name string) bool {
	for _, j := range current {
		if j.Group == name && !j.Stopped {
			return true
		}
	}
	return false
}

// confirmStops asks before stopping anything, unless --yes was given.
//
// Creates and updates come from something you wrote. A stop comes from the
// absence of something, and absence is what a wrong `-C`, a half-finished
// checkout or the wrong repository all look like. Because apply sees
// everything it owns, not only the groups whose directories are present,
// pointing orca at a directory missing most of your groups stops the lot.
//
// No data is at stake either way: a stop keeps the volume and only `orca
// purge` deletes. What is at stake is an outage nobody asked for.
func confirmStops(plan deploy.Plan, yes bool) error {
	stops := plan.Stops()
	if len(stops) == 0 || yes {
		return nil
	}

	if gone := plan.VanishedGroups(); len(gone) > 0 {
		fmt.Printf("no manifests here declare: %s\n", strings.Join(gone, ", "))
	}

	// Lighter than purge's "type the name": a stop keeps data and is undone
	// by applying again from the right directory, so the friction should
	// match the cost.
	return confirmAction(fmt.Sprintf("stop %d running service(s)", len(stops)), false)
}
