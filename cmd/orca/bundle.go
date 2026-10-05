package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
	"github.com/unitoftime/orca/internal/deploy"
	"github.com/unitoftime/orca/internal/registry"
	"gopkg.in/yaml.v3"
)

// variables is what the cluster's store holds: variable path to item key to
// value. Secrets, registry logins and the dashboard password are all
// variables, so reading, comparing and writing them is one operation on this
// rather than one per kind.
type variables map[string]map[string]string

// Bundle is every secret a cluster holds, as one document a person can read
// and edit. It is the cluster's variables under the names they are known by:
// `orca secret export` writes one, `import` applies one, and `edit` is both
// with an editor in between.
//
// The cluster stays the source of truth. A bundle is a copy: something to
// rebuild a cluster from, or to keep beside the manifests, and never something
// apply reads.
type Bundle struct {
	Secrets           map[string]map[string]string `yaml:"secrets,omitempty"`
	Registries        map[string]RegistryLogin     `yaml:"registries,omitempty"`
	DashboardPassword string                       `yaml:"dashboard_password,omitempty"`

	// Certificates are the ones issued for services with `tls:`, by
	// hostname. Not secrets anyone sets, but a rebuilt cluster that has them
	// asks the certificate authority for nothing, and the authority limits
	// how often it will be asked.
	Certificates map[string]Certificate `yaml:"certificates,omitempty"`
}

// Certificate is one issued certificate and its key, both PEM.
type Certificate struct {
	Owner string `yaml:"owner"`
	Cert  string `yaml:"cert"`
	Key   string `yaml:"key"`
}

// RegistryLogin is the credentials the cluster pulls one registry's images
// with.
type RegistryLogin struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// bundleHeader opens every bundle written out. It carries the shape, because
// the file is as often written by hand as read, and an empty bundle is
// otherwise an empty file.
const bundleHeader = `# orca secrets, in plaintext. The shape:
#
#   secrets:
#     <group>:
#       <name>: <value>
#   registries:
#     <host>:
#       username: <username>
#       password: <token>
#   dashboard_password: <value>
`

// bundleCertsHeader is added for a bundle that holds certificates, which is
// only ever one that was exported: nobody writes those by hand.
const bundleCertsHeader = `#   certificates:
#     <hostname>: {owner: <group>/<service>, cert: <pem>, key: <pem>}
`

// marshalBundle is the bundle as the document a person edits.
func marshalBundle(b Bundle) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString(bundleHeader)
	if len(b.Certificates) > 0 {
		out.WriteString(bundleCertsHeader)
	}
	out.WriteString("\n")
	if len(b.variables()) == 0 {
		return out.Bytes(), nil
	}
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(b); err != nil {
		return nil, fmt.Errorf("write secrets: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("write secrets: %w", err)
	}
	return out.Bytes(), nil
}

// parseBundle reads a bundle and checks it holds nothing the cluster would
// refuse, so a mistake is reported against the file and before anything is
// written.
func parseBundle(data []byte) (Bundle, error) {
	var b Bundle
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	// A file of nothing but comments is an empty bundle, not a broken one.
	if err := dec.Decode(&b); err != nil && !errors.Is(err, io.EOF) {
		return Bundle{}, err
	}

	var errs []error
	for group, names := range b.Secrets {
		for name, value := range names {
			ref := group + "/" + name
			if _, _, err := parseSecretRef(ref); err != nil {
				errs = append(errs, err)
			}
			if value == "" {
				errs = append(errs, fmt.Errorf("secret %s is empty", ref))
			}
		}
	}
	for host, login := range b.Registries {
		if canonical, err := registry.Host(host); err != nil {
			errs = append(errs, err)
		} else if canonical != host {
			errs = append(errs, fmt.Errorf("registry %q should be written %q", host, canonical))
		}
		if login.Username == "" || login.Password == "" {
			errs = append(errs, fmt.Errorf("registry %s needs a username and a password", host))
		}
	}
	for host, c := range b.Certificates {
		if deploy.CertPath(host) == deploy.CertPrefix+"/" || strings.ContainsAny(host, "/ ") {
			errs = append(errs, fmt.Errorf("certificate %q should be a hostname", host))
		}
		if c.Cert == "" || c.Key == "" {
			errs = append(errs, fmt.Errorf("certificate %s needs a cert and a key", host))
		}
	}
	if p := b.DashboardPassword; p != "" && len(p) < minPasswordLength {
		errs = append(errs, fmt.Errorf("dashboard_password must be at least %d characters", minPasswordLength))
	}
	return b, errors.Join(errs...)
}

// variables is the bundle as the store holds it.
func (b Bundle) variables() variables {
	v := variables{}
	for group, names := range b.Secrets {
		for name, value := range names {
			v[deploy.SecretPath(group, name)] = map[string]string{deploy.SecretItemKey: value}
		}
	}
	for host, login := range b.Registries {
		v[deploy.RegistryPath(host)] = registryItems(login.Username, login.Password)
	}
	if b.DashboardPassword != "" {
		v[deploy.AdminPasswordPath] = map[string]string{deploy.AdminPasswordKey: b.DashboardPassword}
	}
	for host, c := range b.Certificates {
		v[deploy.CertPath(host)] = map[string]string{
			deploy.CertNameKey: host, deploy.CertOwnerKey: c.Owner,
			deploy.CertChainKey: c.Cert, deploy.CertKeyKey: c.Key,
		}
	}
	return v
}

// withoutCertificates is the bundle a person edits. Certificates are orca's
// to get and renew, long, and nothing anyone types, so they are kept out of
// an editor and carried across an edit untouched.
func (b Bundle) withoutCertificates() Bundle {
	b.Certificates = nil
	return b
}

// bundleOf is the store's variables as a bundle: the inverse of
// Bundle.variables, except that it keeps only what a bundle holds. A
// variable can carry more (a certificate's record notes where it came from
// and why its last renewal failed), and none of that is a secret to keep.
func bundleOf(v variables) Bundle {
	b := Bundle{Secrets: map[string]map[string]string{}, Registries: map[string]RegistryLogin{}, Certificates: map[string]Certificate{}}
	for path, items := range v {
		if path == deploy.AdminPasswordPath {
			b.DashboardPassword = items[deploy.AdminPasswordKey]
		} else if host, ok := deploy.RegistryHost(path); ok {
			b.Registries[host] = RegistryLogin{Username: items[registryUsernameKey], Password: items[registryPasswordKey]}
		} else if strings.HasPrefix(path, deploy.CertPrefix+"/") {
			// One still being asked for holds nothing to keep.
			if items[deploy.CertChainKey] != "" {
				b.Certificates[items[deploy.CertNameKey]] = Certificate{
					Owner: items[deploy.CertOwnerKey], Cert: items[deploy.CertChainKey], Key: items[deploy.CertKeyKey],
				}
			}
		} else if ref, ok := secretRefOf(path); ok {
			if b.Secrets[ref.Group] == nil {
				b.Secrets[ref.Group] = map[string]string{}
			}
			b.Secrets[ref.Group][ref.Name] = items[deploy.SecretItemKey]
		}
	}
	return b
}

// secretRefOf reads a secret's group and name back out of its path, reporting
// false for a variable that is not a secret.
func secretRefOf(path string) (secretRef, bool) {
	rest, ok := strings.CutPrefix(path, deploy.SecretPrefix+"/")
	if !ok {
		return secretRef{}, false
	}
	group, name, ok := strings.Cut(rest, "/")
	if !ok || group == "" || name == "" || strings.Contains(name, "/") {
		return secretRef{}, false
	}
	return secretRef{Group: group, Name: name}, true
}

// variableLabel is a variable under the name its owner knows it by.
func variableLabel(path string) string {
	if path == deploy.AdminPasswordPath {
		return "the dashboard password"
	}
	if host, ok := deploy.RegistryHost(path); ok {
		return "registry login " + host
	}
	if ref, ok := secretRefOf(path); ok {
		return ref.String()
	}
	if key, ok := strings.CutPrefix(path, deploy.CertPrefix+"/"); ok {
		return "certificate " + strings.ReplaceAll(key, "_", ".")
	}
	return path
}

// bundleDiff is what applying a bundle to a cluster would do, as sorted
// variable paths.
//
// There is no "remove": a bundle that lacks something the cluster has is far
// more often an old or partial copy than an instruction, and only `orca
// secret rm` and `orca purge` delete.
type bundleDiff struct {
	Add      []string
	Change   []string
	Same     int
	Unlisted []string // on the cluster and not in the bundle; left alone
}

func diffVariables(want, have variables) bundleDiff {
	var d bundleDiff
	for _, path := range slices.Sorted(maps.Keys(want)) {
		switch items, ok := have[path]; {
		case !ok:
			d.Add = append(d.Add, path)
		case !maps.Equal(items, want[path]):
			d.Change = append(d.Change, path)
		default:
			d.Same++
		}
	}
	for _, path := range slices.Sorted(maps.Keys(have)) {
		if _, ok := want[path]; !ok {
			d.Unlisted = append(d.Unlisted, path)
		}
	}
	return d
}

// A sealed bundle is an age file encrypted to a passphrase, and nothing
// orca-specific: `age -d` opens it on a machine that has never seen orca,
// which is what a file kept for the day the cluster is gone has to allow.
// It is armored, so it is text wherever it is pasted or committed.

// sealed reports whether data is an age file, armored or not.
func sealed(data []byte) bool {
	data = bytes.TrimLeft(data, " \t\r\n")
	return bytes.HasPrefix(data, []byte(armor.Header)) || bytes.HasPrefix(data, []byte("age-encryption.org/"))
}

// seal encrypts a document to a passphrase.
func seal(plain []byte, passphrase string) ([]byte, error) {
	recipient, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	var out bytes.Buffer
	text := armor.NewWriter(&out)
	w, err := age.Encrypt(text, recipient)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	if _, err := w.Write(plain); err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	if err := errors.Join(w.Close(), text.Close()); err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	return out.Bytes(), nil
}

// unseal decrypts what seal, or age itself, wrote.
func unseal(data []byte, passphrase string) ([]byte, error) {
	identity, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	var src io.Reader = bytes.NewReader(data)
	if bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"), []byte(armor.Header)) {
		src = armor.NewReader(src)
	}
	r, err := age.Decrypt(src, identity)
	if err != nil {
		var wrong *age.NoIdentityMatchError
		if errors.As(err, &wrong) {
			return nil, errors.New("wrong passphrase")
		}
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plain, nil
}
