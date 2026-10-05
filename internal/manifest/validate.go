package manifest

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var (
	// dnsLabel is the shape of a group or service name: it becomes a DNS label
	// in a generated hostname and a Nomad job/group name, so the strictest
	// consumer sets the rule.
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

	envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

	// secretName is what a secret may be called. It is also a file name under
	// /secrets by default, and it is interpolated into a command that runs on
	// the machine, so it matches cmd/orca's own pattern exactly. A name that
	// validates here but not there would be one that can be declared and never
	// set.
	secretName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// IsDNSLabel reports whether name can be one label of a DNS name, which is
// the strictest of the places orca's names end up.
func IsDNSLabel(name string) bool {
	return len(name) <= 63 && dnsLabel.MatchString(name)
}

// errList accumulates problems so one run reports every *semantic* error in a
// manifest, rather than making the author fix one per invocation.
//
// Syntax errors do not accumulate: a value the decoder cannot read at all (a
// size with no unit, a mistyped protocol, an unknown field) aborts decoding
// before validation runs, so exactly one is reported. Fixing that would mean
// making every custom unmarshaler total and re-parsing during validation, which
// costs more clarity than the second error is worth. See
// TestSyntaxErrorsDoNotAccumulate.
type errList struct {
	prefix string
	errs   []error
}

func (e *errList) addf(format string, args ...any) {
	e.errs = append(e.errs, fmt.Errorf(e.prefix+format, args...))
}

func (e *errList) add(err error) {
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s%w", e.prefix, err))
	}
}

// Validate checks a normalized manifest and reports every problem it finds at
// once. It changes nothing: Discover and ParseGroup normalize first.
func (m *Manifest) Validate() error {
	e := &errList{prefix: m.Path + ": "}

	// The group name comes from the directory, so a name that cannot be used
	// is a directory that needs renaming.
	if !IsDNSLabel(m.Group) {
		e.addf("group name %q must be lowercase letters, digits and dashes (a DNS label); rename the directory", m.Group)
	}
	// Reserved rather than merged: orca files its own jobs under this name, so
	// a directory of it would put your services in the same namespace, and a
	// service called traefik or dns would collide with the real one outright.
	//
	// The prefix too: jobs are named <group>-<service>, so a group orca-node
	// with a service exporter would be orca's own node exporter.
	if m.Group == ReservedGroup || strings.HasPrefix(m.Group, ReservedGroup+"-") {
		e.addf("group name %q is reserved: %s and every name starting %s- belong to the jobs orca runs for you (%s/traefik, %s/dns, ...); rename the directory",
			m.Group, ReservedGroup, ReservedGroup, ReservedGroup, ReservedGroup)
	}

	if len(m.Services) == 0 {
		e.addf("no services defined")
	}

	seen := map[string]bool{}
	// claimed maps "proto/port" to the service that took it, so a collision
	// names both sides instead of just saying a port is busy.
	claimed := map[string]string{}

	for i, s := range m.Services {
		if s == nil {
			e.addf("services[%d] is empty", i)
			continue
		}

		what := fmt.Sprintf("service %q", s.Name)
		if s.Name == "" {
			what = fmt.Sprintf("document %d", i+1)
		}
		// Point at the file the service was declared in, not the group
		// directory: a group is merged from several files and the directory
		// alone does not say which one to open.
		where := s.srcFile
		if where == "" {
			where = m.Path
		}
		se := &errList{prefix: fmt.Sprintf("%s: %s: ", where, what)}

		validateName(se, m.Group, s, seen)
		validateSource(se, s)
		validateBackup(se, s)

		// A target runs no container, so the validators below have nothing to
		// check on it, and validateSource has already refused every field
		// they would have looked at.
		if !s.IsTarget() {
			validateSizing(se, s)
			validateVolume(se, s)
			validateEnv(se, s)
			validateCmd(se, s)
			validateSecrets(se, s)
			validatePorts(se, s, claimed)
			validateNode(se, s)
			validateTLS(se, s)
		}

		e.errs = append(e.errs, se.errs...)
	}

	return errors.Join(e.errs...)
}

// validateCmd refuses a secret reference in cmd. Only env values have them
// filled in, on the machine, so in cmd it would reach the shell as text; and a
// command line is readable by anything on the machine, which is what keeping
// secrets out of it is for.
func validateCmd(e *errList, s *Service) {
	if _, err := ShellValue(s.Cmd); err != nil {
		e.addf("cmd: %v", err)
	}
}

func validateName(e *errList, group string, s *Service, seen map[string]bool) {
	switch {
	case s.Name == "":
		e.addf("name is required")
	case !IsDNSLabel(s.Name):
		e.addf("name %q must be lowercase letters, digits and dashes (a DNS label)", s.Name)
	// The service is registered, and a storage template's bucket created, as
	// <group>-<service>, and both are refused past one DNS label's length.
	case len(group)+1+len(s.Name) > 63:
		e.addf("%s-%s is longer than 63 characters, which its name in the cluster cannot be; shorten the service or the group", group, s.Name)
	case seen[s.Name]:
		e.addf("duplicate service name %q", s.Name)
	default:
		seen[s.Name] = true
	}
}

// validateSource resolves image-or-template and rejects any field a template
// owns. Silently overriding an author's value would be worse: they would have
// written something that looks effective and is not.
func validateSource(e *errList, s *Service) {
	declared := 0
	for _, v := range []string{s.Image, s.Template, s.Target} {
		if v != "" {
			declared++
		}
	}
	switch {
	case declared == 0:
		e.addf("needs an image, a template or a target")
		return
	case declared > 1:
		e.addf("declares more than one of image, template and target; use one")
		return
	case s.Target != "":
		validateTarget(e, s)
		return
	case s.Template == "":
		return
	}

	t, err := ParseTemplate(s.Template)
	if err != nil {
		e.add(err)
		return
	}
	spec := t.Spec()

	// A template owns these. Agreeing with it is harmless; contradicting it
	// is the error.
	for _, cport := range s.PortNumbers() {
		if cport != spec.Port {
			e.addf("template %s listens on %d, but this declares port %d", t, spec.Port, cport)
		}
	}
	if s.Cmd != "" {
		e.addf("template %s provides its own command; remove cmd", t)
	}

	if spec.VolumeRequired && s.Volume == nil {
		e.addf("template %s needs a volume, e.g. volume: 20G", t)
	}
	// Postgres does not work from an arbitrary directory, so a mount that
	// looks honoured and is not would be a genuinely confusing failure.
	if s.Volume != nil && s.Volume.Mount != spec.VolumeMount {
		e.addf("template %s fixes the volume mount at %s, but this sets %s", t, spec.VolumeMount, s.Volume.Mount)
	}
}

func validateSizing(e *errList, s *Service) {
	if s.Memory < MinMemory {
		e.addf("memory must be at least %s, got %s", MinMemory, s.Memory)
	}
	if s.CPU <= 0 {
		e.addf("cpu must be greater than 0")
	}
	if s.Replicas < 1 {
		e.addf("replicas must be at least 1, got %d", s.Replicas)
	}
}

func validateVolume(e *errList, s *Service) {
	if s.Volume == nil {
		return
	}
	if s.Volume.Size <= 0 {
		e.addf("volume size must be greater than 0")
	}
	switch {
	case s.Volume.Mount == "":
		e.addf("volume needs a mount path, e.g. volume: {size: 20G, mount: /data}")
	case !strings.HasPrefix(s.Volume.Mount, "/"):
		e.addf("volume mount %q must be an absolute path", s.Volume.Mount)
	}
	// A host volume is one directory on one machine. Several replicas writing
	// to it concurrently corrupts anything that was not built for it, and the
	// two things most likely to have a volume (a database and a file store)
	// emphatically were not.
	if s.Replicas > 1 {
		e.addf("has a volume and %d replicas; a volume is one directory on one machine, so it cannot be shared", s.Replicas)
	}
}

func validateEnv(e *errList, s *Service) {
	for k, v := range s.Env {
		if !envKey.MatchString(k) {
			e.addf("env key %q must start with a letter or underscore and contain only letters, digits and underscores", k)
		}
		// Any ${...} must be a secret reference. Without this, a typo like
		// ${secrets.token} ships to the container as that literal string and
		// fails somewhere much less obvious.
		if _, err := ParseEnvValue(v); err != nil {
			e.addf("env %s: %v", k, err)
		}
	}
}

// validateSecrets checks the declared secrets and the places they land.
//
// Two secrets arriving at the same destination is the error worth catching
// here: one silently wins, and which one depends on map iteration order, so the
// container gets a different value on different deploys of an unchanged
// manifest.
func validateSecrets(e *errList, s *Service) {
	names := map[string]bool{}
	files := map[string]string{}
	envs := map[string]string{}

	for i, sec := range s.Secrets {
		where := fmt.Sprintf("secrets[%d]", i)
		if sec.Name != "" {
			where = fmt.Sprintf("secret %q", sec.Name)
		}

		switch {
		case sec.Name == "":
			e.addf("%s: name is required", where)
			continue
		case !secretName.MatchString(sec.Name):
			e.addf("%s: name may contain only letters, digits, dashes and underscores", where)
			continue
		case names[sec.Name]:
			e.addf("%s: declared twice", where)
			continue
		}
		names[sec.Name] = true

		if sec.Env != "" && sec.Path != "" {
			e.addf("%s: has both env and path; a secret goes to one place, so use one", where)
			continue
		}

		if sec.Env != "" {
			if !envKey.MatchString(sec.Env) {
				e.addf("%s: env %q must start with a letter or underscore and contain only letters, digits and underscores",
					where, sec.Env)
				continue
			}
			// A plain env value and a secret would both write this key, and
			// the secret wins silently — the manifest says two things and
			// only one happens.
			if _, taken := s.Env[sec.Env]; taken {
				e.addf("%s: env %s is already set in env:", where, sec.Env)
				continue
			}
			if owner, taken := envs[sec.Env]; taken {
				e.addf("%s: env %s is already used by secret %q", where, sec.Env, owner)
				continue
			}
			envs[sec.Env] = sec.Name
			continue
		}

		if sec.Path != "" {
			if err := validSecretFile(sec.Path); err != nil {
				e.addf("%s: %v", where, err)
				continue
			}
		}
		if sec.File() == EnvFile {
			e.addf("%s: file %s is where orca writes the service's environment; use another path", where, sec.MountPath())
			continue
		}
		if owner, taken := files[sec.File()]; taken {
			e.addf("%s: file %s is already used by secret %q", where, sec.MountPath(), owner)
			continue
		}
		files[sec.File()] = sec.Name
	}
}

func validatePorts(e *errList, s *Service, claimed map[string]string) {
	hostnames := 0
	metrics := 0

	for _, cport := range s.PortNumbers() {
		p := s.Ports[cport]

		if cport < 1 || cport > 65535 {
			e.addf("ports: container port %d is outside 1-65535", cport)
			continue
		}

		switch p.Kind {
		case PortMetrics:
			metrics++
		case PortDomain:
			hostnames++
			if err := validateHostname(p.Domain); err != nil {
				e.addf("ports %d: %v", cport, err)
			}
		case PortRaw:
			for _, b := range p.Binds {
				host := b.HostPort(cport)
				key := b.Proto + "/" + fmt.Sprint(host)
				if owner, taken := claimed[key]; taken {
					e.addf("ports %d: host port %d/%s is already used by service %q", cport, host, b.Proto, owner)
					continue
				}
				claimed[key] = s.Name
			}
			// Two replicas of one service both want the same host port, and
			// the second never places. Caught here because the symptom
			// otherwise is a service that is permanently half-deployed with
			// nothing saying why.
			if s.Replicas > 1 {
				e.addf("ports %d: binds a host port and has %d replicas; a host port is one number on one machine, so it cannot be shared",
					cport, s.Replicas)
			}
		}
	}

	// Each hostname-routed port needs its own name, and a service may name
	// only one. Two would be ambiguous about which port answers.
	if hostnames > 1 {
		e.addf("declares %d ports over HTTP; a service gets one hostname, so route the rest through it or split the service", hostnames)
	}

	// One scrape target per service: the metrics are labelled by group and
	// service, and two targets under the same labels would be two sets of
	// series nobody could tell apart.
	if metrics > 1 {
		e.addf("declares %d metrics ports; a service is scraped on one, so serve every metric from it", metrics)
	}
}

// validateTLS checks the name a service wants a certificate for.
func validateTLS(e *errList, s *Service) {
	if s.TLS == "" {
		return
	}
	s.TLS = strings.ToLower(s.TLS)
	if err := validateHostname(s.TLS); err != nil {
		e.addf("tls: %v", err)
	}
	if s.IsTemplated() {
		e.addf("tls: a template serves no TLS of its own; put it on the service that does")
	}
	// A name ingress already serves has its certificate there, and a
	// certificate is for a port a client can reach.
	raw := false
	for _, p := range s.Ports {
		if p.Kind == PortDomain && p.Domain == s.TLS {
			e.addf("tls: %s is already served over HTTPS by ingress on this service's hostname port; tls is for a raw port", s.TLS)
		}
		raw = raw || p.Kind == PortRaw
	}
	if !raw {
		e.addf("tls: needs a tcp or udp port to serve it on; a hostname port is already HTTPS through ingress")
	}
}

// validateNode checks the shape of a node reference. Whether that machine is
// actually in cluster.yaml is checked by apply, which is the first thing that
// knows what machines exist.
func validateNode(e *errList, s *Service) {
	if s.Node == "" {
		return
	}
	if !IsDNSLabel(s.Node) {
		e.addf("node %q must be lowercase letters, digits and dashes", s.Node)
	}
}

func validateHostname(h string) error {
	if h == "" {
		return fmt.Errorf("hostname is empty")
	}
	if len(h) > 253 {
		return fmt.Errorf("hostname %q is longer than 253 characters", h)
	}
	if !strings.Contains(h, ".") {
		return fmt.Errorf("hostname %q needs a dot; a bare word is read as a protocol or keyword", h)
	}
	for _, label := range strings.Split(h, ".") {
		if !dnsLabel.MatchString(label) || len(label) > 63 {
			return fmt.Errorf("hostname %q has an invalid label %q", h, label)
		}
	}
	return nil
}

// validateTarget checks a target document and the fields only it may set.
//
// A target runs no container, so everything describing a container is refused
// rather than ignored: a cpu or a volume written here would look effective and
// do nothing, which is the failure this whole validator exists to prevent.
func validateTarget(e *errList, s *Service) {
	if s.Target != TargetS3 {
		e.addf("unknown target %q; the only kind is %q", s.Target, TargetS3)
		return
	}
	if s.Endpoint == "" {
		e.addf("target needs an endpoint, e.g. https://<account>.r2.cloudflarestorage.com")
	}
	if s.Bucket == "" {
		e.addf("target needs a bucket")
	}
	// Strict, because it becomes part of a path in a command on the machine.
	for _, part := range strings.Split(s.Path, "/") {
		if s.Path != "" && !IsDNSLabel(part) {
			e.addf("path %q must be folder names of lowercase letters, digits and dashes, separated by /", s.Path)
			break
		}
	}

	var set []string
	if s.CPU != 0 {
		set = append(set, "cpu")
	}
	if s.Memory != 0 {
		set = append(set, "memory")
	}
	if s.Volume != nil {
		set = append(set, "volume")
	}
	if s.Replicas != 0 {
		set = append(set, "replicas")
	}
	if s.Cmd != "" {
		set = append(set, "cmd")
	}
	if s.Node != "" {
		set = append(set, "node")
	}
	if len(s.Env) > 0 {
		set = append(set, "env")
	}
	if len(s.Ports) > 0 {
		set = append(set, "ports")
	}
	if len(s.Secrets) > 0 {
		set = append(set, "secrets")
	}
	if s.Backup != nil {
		set = append(set, "backup")
	}
	if len(set) > 0 {
		e.addf("a target runs no container, so it cannot set %s", strings.Join(set, ", "))
	}
}

// validateBackup checks a service's backup policy.
//
// Whether the target it names actually exists is checked by apply, which is
// the first thing that has every group in hand; a group is validated on its
// own, and the target is usually in another one.
func validateBackup(e *errList, s *Service) {
	// endpoint, bucket and region belong to a target. On anything else they
	// are almost certainly a backup policy written in the wrong shape.
	if !s.IsTarget() {
		var stray []string
		if s.Endpoint != "" {
			stray = append(stray, "endpoint")
		}
		if s.Bucket != "" {
			stray = append(stray, "bucket")
		}
		if s.Path != "" {
			stray = append(stray, "path")
		}
		if s.Region != "" {
			stray = append(stray, "region")
		}
		if len(stray) > 0 {
			e.addf("%s belong to a target; a service backs up by naming one, with backup: {to: <group>/<service>}",
				strings.Join(stray, " and "))
		}
	}

	if s.Backup == nil {
		return
	}
	if _, _, err := TargetRef(s.Backup.To); err != nil {
		e.add(err)
	}
	// orca can back up a database because it knows what a database is. It
	// cannot back up an arbitrary volume, because it does not know what is
	// safe to copy while something is writing to it.
	if t := s.Tmpl(); t == nil || t.Spec().Backup == "" {
		e.addf("backup is only available on a template that knows how to dump itself (%s); this service would need its own",
			strings.Join(backupTemplates(), ", "))
	}
	if s.Backup.Keep < 1 {
		e.addf("backup.keep must be at least 1")
	}
}

// backupTemplates names the templates that can be backed up, for the message
// that tells someone which ones those are.
func backupTemplates() []string {
	var out []string
	for name, spec := range Templates {
		if spec.Backup != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
