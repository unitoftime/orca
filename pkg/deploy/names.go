package deploy

import (
	"path"
	"strings"

	"github.com/unitoftime/orca/pkg/manifest"
)

// Every name orca derives from a group and a service lives in this file.
//
// Names built inline wherever they are needed drift apart, and a mismatch is
// quiet: a purge matching volumes by a "<group>-" prefix also matches a group
// with a longer name. One place makes each name's shape, and whether it can be
// read back, a single decision.
//
// The flat names below, <group>-<service>, are one namespace across every
// group, and cannot be split back because both halves may contain dashes.
// That is safe only because apply refuses two services producing the same one
// (see ServiceJobIDs); nothing may parse them. Where a name has to be read
// back, it is structured instead: a volume is a directory per group, and a
// DNS name travels as a tag.

// JobID is the cluster-wide name of a service's job. Host ports are not the
// only global namespace on the machine; job IDs are another, which is why the
// loader checks these for collisions across apps too.
func JobID(app, service string) string { return app + "-" + service }

// CatalogName is how a service appears in Nomad's service catalog. It carries
// the app because the catalog is one flat namespace across every app.
func CatalogName(app, service string) string { return app + "-" + service }

// BackupJobID is the periodic job that backs up one database.
func BackupJobID(group, service string) string {
	return JobID(group, service) + "-backup"
}

// BackupPrefix is where one database's backups live in the bucket: under the
// cluster's name when it has one, so clusters sharing a bucket keep apart.
func BackupPrefix(cluster, group, service string) string {
	if cluster == "" {
		return group + "/" + service
	}
	return cluster + "/" + group + "/" + service
}

// PreRestoreSuffix marks the copy a Postgres restore keeps of a database it
// replaced: <name>_before_restore_<time>, on the same server.
const PreRestoreSuffix = "_before_restore_"

// SecretPrefix is the root of orca's namespace in Nomad's variable store.
const SecretPrefix = "orca"

// SecretItemKey is the item every orca secret variable stores its value under.
const SecretItemKey = "value"

// SecretPath is where one secret lives in Nomad's variable store: one variable
// per secret rather than one per group.
//
// That makes listing which secrets exist a listing of paths, which returns no
// values at all, so `orca secret list` never pulls plaintext off the machine
// just to tell you a name is set.
func SecretPath(group, name string) string {
	return SecretPrefix + "/" + group + "/" + name
}

// AdminPasswordPath is where the dashboards' generated password lives in
// Nomad's variable store, under AdminPasswordKey. A sibling of SecretPrefix
// for the same reason RegistryPrefix is: under it, `orca secret list` would
// report it as a secret no manifest references.
const (
	AdminPasswordPath = "orca-admin/password"
	AdminPasswordKey  = "password"
)

// ApplyLockPath is the variable an apply holds a lock on for as long as it
// runs, so two cannot interleave. A sibling of SecretPrefix, like the others.
const ApplyLockPath = "orca-lock/apply"

// RegistryPrefix is where registry credentials live in Nomad's variable
// store. A sibling of SecretPrefix rather than a directory under it: under it,
// `orca secret list` would read every login as a secret no manifest
// references, and a group named after the directory would collide with it.
const RegistryPrefix = "orca-registry"

// RegistryPath is where one registry's credentials live: one variable per
// registry host, holding a username and a password.
//
// Nomad refuses a "." or ":" in a variable path, so they are written as "_"
// and "~". Neither can appear in a host name, which is what makes the path
// readable back into the host by RegistryHost. The credential helper
// on each machine does the same substitution, so the two must stay in step.
func RegistryPath(host string) string {
	return RegistryPrefix + "/" + registryPathReplacer.Replace(host)
}

// RegistryHost reads a registry host back out of a RegistryPath, reporting
// false for a path that is not one.
func RegistryHost(varPath string) (string, bool) {
	key, ok := strings.CutPrefix(varPath, RegistryPrefix+"/")
	if !ok || key == "" || strings.Contains(key, "/") {
		return "", false
	}
	return strings.NewReplacer("_", ".", "~", ":").Replace(key), true
}

var registryPathReplacer = strings.NewReplacer(".", "_", ":", "~")

// DNSName is the fully qualified name a service answers to.
func DNSName(group, service string) string {
	return service + "." + group + "." + Zone
}

// SearchDomains are what a task in this group gets in its resolver
// configuration, which is what makes a bare `db` mean this group's db.
func SearchDomains(group string) []string {
	return []string{group + "." + Zone, Zone}
}

// DNSTagPrefix marks a catalog registration with the name the resolver
// answers for it.
//
// The resolver's hosts file is rendered from the catalog by this tag rather
// than from a list of services baked into its own job. With a list, only an
// apply including orca's own group would update the resolver, so a service
// added by `orca apply shop` would not resolve until some later, unrelated full
// apply. A tag travels with the service's own registration, so whatever
// registers is resolvable, from whichever apply deployed it.
const DNSTagPrefix = "orca-dns="

// DNSTag is the tag a service registers with: its short name,
// <service>.<group>, which the resolver also answers under the zone.
//
// A catalog name is <group>-<service> and both halves may contain dashes, so
// the name cannot be split back reliably. The tag says it outright.
func DNSTag(group, service string) string {
	return DNSTagPrefix + service + "." + group
}

// MetricsCatalogName is the one catalog name every scrape target registers
// under. It is filed under orca's own group, so no service of yours can take it.
var MetricsCatalogName = CatalogName(OrcaApp, "metrics")

// MetricsTagGroup and MetricsTagService carry a scrape target's group and
// service, as <key>=<value>. The keys are the label names its series get.
const (
	MetricsTagGroup   = "group"
	MetricsTagService = "service"
)

// MetricsTags are what a scrape target registers with. Both values are DNS
// labels, which is what lets the scrape config write them into YAML as-is.
func MetricsTags(group, service string) []string {
	return []string{MetricsTagGroup + "=" + group, MetricsTagService + "=" + service}
}

// VolumeRoot is the directory every app volume lives under. Having one root
// is what lets orphaned data be found by listing rather than remembered.
func VolumeRoot(dataDir string) string {
	return path.Join(dataDir, "volumes", "services")
}

// VolumePath is where a service's volume lives on the machine:
// <root>/<group>/<service>.
//
// Two levels rather than one flattened name. Group names contain dashes, so a
// flat "<group>-<service>" cannot be split back: "shop-prod-db" is shop's
// prod-db or shop-prod's db, and purging by the prefix "shop-" would delete
// shop-prod's data along with shop's. A directory per group makes the owner of
// every volume a fact of the path, and purging a group one directory.
func VolumePath(dataDir, group, service string) string {
	return path.Join(VolumeRoot(dataDir), group, service)
}

// PlatformVolumePath is where a platform component keeps its data. These
// directories are created at bootstrap, before any job exists to want them.
func PlatformVolumePath(dataDir, name string) string {
	return dataDir + "/volumes/" + name
}

// BucketName is the bucket a storage template creates for its service.
func BucketName(group, service string) string { return group + "-" + service }

// ServiceJobIDs is every Nomad job one service produces or reserves: its own,
// and the periodic backup job a template that can dump itself may have. It is
// the single list collision checks and log targets are derived from.
func ServiceJobIDs(group string, s *manifest.Service) []string {
	if s.IsTarget() {
		return nil
	}
	ids := []string{JobID(group, s.Name)}
	if t := s.Tmpl(); t != nil && t.Spec().Backup != "" {
		ids = append(ids, BackupJobID(group, s.Name))
	}
	return ids
}
