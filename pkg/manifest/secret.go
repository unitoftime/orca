package manifest

import (
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// SecretDir is where a service's secret files appear inside its container.
//
// Not a choice orca makes: Nomad's docker driver mounts the allocation's
// secrets directory at this path, on a private tmpfs that no other allocation
// can see. Declaring it here is only so the value has a name and a reason
// beside it, rather than being a bare string in the job builder.
const SecretDir = "/secrets"

// EnvFile is the file under SecretDir that orca renders a task's secret
// environment into, for Nomad to read as KEY=value pairs. It is orca's, so no
// declared secret may be delivered there: the two templates would share one
// destination and one would silently overwrite the other.
const EnvFile = "orca.env"

// Secret is one secret a service needs at run time.
//
// Written as a bare name, which is the whole thing most of the time:
//
//	secrets:
//	  - apiToken
//
// That delivers the value as the file /secrets/apiToken. Files are the
// default because they are what a process can read without its value ever
// being in the environment, where it is inherited by every child process,
// printed by an unlucky crash handler, and visible in /proc to anything else
// on the box.
//
// The mapping form overrides where it lands:
//
//	secrets:
//	  - name: session_key
//	    env: SESSION_KEY      # an environment variable instead of a file
//	  - name: gh_key
//	    path: github.pem      # a file under a different name
//
// `env` and `path` are mutually exclusive: each says where the value goes, and
// a secret goes to exactly one place.
type Secret struct {
	// Name is the secret's name in the cluster's store: what
	// `orca secret set <group>/<name>` sets, and what `orca secret list`
	// reports.
	Name string `yaml:"name"`

	// Env delivers the value as this environment variable rather than a file.
	Env string `yaml:"env,omitempty"`

	// Path is the file name under /secrets, when it differs from Name.
	// Relative to the secrets directory, so "github.pem" is
	// /secrets/github.pem. Subdirectories are allowed; escaping upward is not.
	Path string `yaml:"path,omitempty"`
}

// File is the name of the file this secret is delivered as, relative to
// SecretDir. Meaningless for an env-delivered secret; callers check IsFile.
func (s Secret) File() string {
	if s.Path != "" {
		return s.Path
	}
	return s.Name
}

// IsFile reports whether this secret is delivered as a file, which is every
// secret that did not ask for an environment variable.
func (s Secret) IsFile() bool { return s.Env == "" }

// MountPath is where the file appears inside the container.
func (s Secret) MountPath() string { return SecretDir + "/" + s.File() }

func (s *Secret) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var name string
		if err := node.Decode(&name); err != nil {
			return fmt.Errorf("secret must be a name like apiToken, or a mapping with name and env or path")
		}
		s.Name = name
		return nil
	}

	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("secret must be a name like apiToken, or a mapping with name and env or path")
	}

	// node.Decode does not inherit the parent decoder's KnownFields setting,
	// so strictness is enforced by hand here, for the same reason volume does
	// it.
	if err := checkKnownFields(node, "secret", "name", "env", "path"); err != nil {
		return err
	}

	type plain Secret // avoid recursing into this method
	var out plain
	if err := node.Decode(&out); err != nil {
		return err
	}
	*s = Secret(out)
	return nil
}

func (s Secret) MarshalYAML() (any, error) {
	// The bare name round-trips as a bare name. orca serializes resolved
	// manifests for plan diffs and desired-state hashes, so a secret that
	// marshalled into a mapping it was not written as would make every plan
	// show a change that is not one.
	if s.Env == "" && s.Path == "" {
		return s.Name, nil
	}
	out := map[string]string{"name": s.Name}
	if s.Env != "" {
		out["env"] = s.Env
	}
	if s.Path != "" {
		out["path"] = s.Path
	}
	return out, nil
}

// validSecretFile reports whether a path is a plain relative location under the
// secrets directory. Checked rather than assumed: the value is joined onto a
// destination path on the machine, so "../../etc/cron.d/x" would write a file
// that runs as root.
func validSecretFile(p string) error {
	if p == "" || p == "." {
		return fmt.Errorf("path must name a file")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("path %q must be relative to %s, not absolute", p, SecretDir)
	}
	if path.Clean(p) != p {
		return fmt.Errorf("path %q must be a plain relative path (no . or .. segments, no trailing or doubled slashes)", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("path %q must stay inside %s", p, SecretDir)
		}
	}
	return nil
}
