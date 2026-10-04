package deploy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/pkg/manifest"
)

// envFile builds a Nomad template that renders a task's environment for
// everything that cannot be a literal in the jobspec: values read from Nomad's
// variable store when the task starts, so a secret never passes through orca,
// a manifest, or a jobspec anyone can read back.
type envFile struct {
	group string
	b     strings.Builder
	n     int
}

// set writes KEY=value, where the value is literal text and secrets spliced
// together in order.
//
// Every value is written as a single JSON string. Nomad reads this file with
// go-envparse, which decodes JSON escapes inside double quotes, so a secret
// holding a quote, a backslash or a newline arrives intact. Written raw, such
// a value would break the line, and nothing before the machine can escape it,
// since the value does not exist until the template renders there. Literal
// text is placed in the template as a quoted string rather than raw text, so a
// {{ in a manifest's env value is text and not a template action.
func (f *envFile) set(key string, parts []manifest.EnvPart) {
	args := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.Secret == "" {
			args = append(args, strconv.Quote(p.Literal))
			continue
		}
		f.n++
		v := fmt.Sprintf("$s%d", f.n)
		fmt.Fprintf(&f.b, `{{ %s := "" }}{{ with nomadVar %q }}{{ %s = printf "%%s" .%s }}{{ end }}`,
			v, SecretPath(f.group, p.Secret), v, SecretItemKey)
		args = append(args, v)
	}
	if len(args) == 0 {
		args = append(args, `""`)
	}
	// print joins strings without separators, and printf "%s" above turns the
	// variable's item into a plain string whatever type the template engine
	// gives it, so toJSON always sees a string.
	fmt.Fprintf(&f.b, "%s={{ print %s | toJSON }}\n", key, strings.Join(args, " "))
}

// setSecret writes KEY=<the secret's value>.
func (f *envFile) setSecret(key, name string) {
	f.set(key, []manifest.EnvPart{{Secret: name}})
}

// setLiteral writes KEY=<text>.
func (f *envFile) setLiteral(key, text string) {
	f.set(key, []manifest.EnvPart{{Literal: text}})
}

func (f *envFile) String() string { return f.b.String() }

// renderTemplate builds the template for a service's secret-bearing
// environment. Returns "" when a service has none, so simple services get a
// jobspec with no template block at all.
//
// Siblings' addresses are deliberately not in it. As ORCA_<SVC>_ADDR read from
// the catalog with change_mode restart, they would restart every service in a
// group whenever any other moved. Services find each other by name through the
// resolver instead.
func renderTemplate(m *manifest.Manifest, s *manifest.Service) string {
	f := envFile{group: m.App}

	// Secrets the template owns: the author never writes these, and never
	// sees the value. One read from a config file instead is skipped, so it
	// never passes through the environment at all.
	if tmpl := s.Tmpl(); tmpl != nil {
		for _, sec := range tmpl.Spec().Secrets {
			if sec.Env != "" {
				f.setSecret(sec.Env, manifest.GeneratedSecret(s.Name, sec.Suffix))
			}
		}
	}

	// Declared secrets that asked for an environment variable. The ones that
	// did not are written as files instead, by secretFiles.
	//
	// Sorted by the variable name, which is unique among them, so the rendered
	// template and therefore the job hash is stable across runs.
	var envSecrets []manifest.Secret
	for _, sec := range s.Secrets {
		if !sec.IsFile() {
			envSecrets = append(envSecrets, sec)
		}
	}
	sort.Slice(envSecrets, func(i, j int) bool { return envSecrets[i].Env < envSecrets[j].Env })
	for _, sec := range envSecrets {
		f.setSecret(sec.Env, sec.Name)
	}

	// Env values that reference secrets, in sorted order for the same reason.
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts, err := manifest.ParseEnvValue(s.Env[k])
		if err != nil || !manifest.HasSecret(parts) {
			// Plain values go in the jobspec; a value that does not parse
			// was already refused by validation.
			continue
		}
		f.set(k, parts)
	}

	return f.String()
}

// secretFiles builds one Nomad template per file-delivered secret, each
// rendering a single secret's value into its own file under the allocation's
// secrets directory, which the docker driver mounts at manifest.SecretDir.
//
// One file per secret rather than one file of many: an application reading a
// secret from a file expects the file to *be* the secret, and the format it
// would otherwise have to parse is a format orca would have to invent.
//
// Sorted by destination so the job spec, and therefore the deploy hash, does
// not change when the manifest's ordering does.
func secretFiles(m *manifest.Manifest, s *manifest.Service) []*nomad.Template {
	var out []*nomad.Template
	for _, sec := range s.Secrets {
		if !sec.IsFile() {
			continue
		}
		out = append(out, &nomad.Template{
			EmbeddedTmpl: ptr(fmt.Sprintf("{{ with nomadVar %q }}{{ .%s }}{{ end }}",
				SecretPath(m.App, sec.Name), SecretItemKey)),
			DestPath: ptr("secrets/" + sec.File()),
			// Read-only, and readable by whoever the image runs as. A stricter
			// mode would have to guess that user (distroless images run as
			// nonroot, the postgres image as postgres), and guessing wrong
			// fails at startup with a permission error that says nothing about
			// secrets. It costs no isolation: the secrets directory is a
			// private tmpfs that only this allocation can see.
			Perms:      ptr("0444"),
			ChangeMode: ptr("restart"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return *out[i].DestPath < *out[j].DestPath })
	return out
}

// certFiles renders a hostname's certificate and key into the task, from its
// record in the variable store.
//
// noop, where a secret restarts: a certificate renews every couple of months
// whether or not anything was deployed, and restarting whatever serves it on
// that schedule is an outage nobody asked for. Nomad rewrites the files in
// place and the service reloads them.
func certFiles(host string) []*nomad.Template {
	file := func(item, dest string) *nomad.Template {
		return &nomad.Template{
			EmbeddedTmpl: ptr(fmt.Sprintf("{{ with nomadVar %q }}{{ .%s }}{{ end }}", CertPath(host), item)),
			DestPath:     ptr("secrets/" + dest),
			Perms:        ptr("0444"), // as secretFiles
			ChangeMode:   ptr("noop"),
		}
	}
	return []*nomad.Template{file(CertChainKey, TLSCertFile), file(CertKeyKey, TLSKeyFile)}
}
