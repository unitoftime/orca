package manifest

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// TargetS3 is the only kind of target there is: an S3-compatible object store,
// named by endpoint.
//
// Generic on purpose. R2, Backblaze, Wasabi, MinIO and S3 itself differ only in
// the endpoint, so naming one would have been a smaller feature for no less
// work.
const TargetS3 = "s3"

// A target is somewhere outside the cluster that orca puts things, declared in
// a group like anything else:
//
//	# storage/offsite.yaml
//	name: offsite
//	target: s3
//	endpoint: https://<account>.r2.cloudflarestorage.com
//	bucket: orca-backups
//
// It is the one service document that runs no container. That is a real cost
// (every other noun here is something running), and it buys what nothing else
// does: the endpoint and the credentials are written once, and everything that
// backs up somewhere refers to them by name.
//
// Its credentials are two secrets in its own group, named after it the way a
// template's generated secrets are:
//
//	orca secret set storage/offsite_key_id
//	orca secret set storage/offsite_secret_key
const (
	TargetKeyIDSuffix  = "key_id"
	TargetSecretSuffix = "secret_key"
)

// TargetKeyID and TargetSecretKey name a target's credentials in its group.
func TargetKeyID(service string) string { return GeneratedSecret(service, TargetKeyIDSuffix) }
func TargetSecretKey(service string) string {
	return GeneratedSecret(service, TargetSecretSuffix)
}

// DefaultRegion is what an S3 target gets when it does not say. "auto" is R2's
// value; most S3-compatible stores ignore the field but require it set.
const DefaultRegion = "auto"

// Backup is a service's backup policy: where its dumps go, and how often.
//
// Only a templated service whose template knows how to dump itself may have
// one: orca can back up a database because it knows what a database is, and
// cannot back up an arbitrary volume because it does not know what is safe to
// copy while it is being written.
type Backup struct {
	// To names the target, as "<group>/<service>". Cross-group on purpose: one
	// target usually serves every database in the cluster.
	To string `yaml:"to"`

	// Schedule is a cron expression. Defaults to nightly.
	Schedule string `yaml:"schedule,omitempty"`

	// Keep is how many backups to retain for this database.
	Keep int `yaml:"keep,omitempty"`
}

// Backup defaults, applied per database rather than per cluster so one noisy
// database can keep more history without everything else doing the same.
const (
	DefaultBackupSchedule = "0 3 * * *"
	DefaultBackupKeep     = 14
)

func (b *Backup) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("backup must be a mapping with to, and optionally schedule and keep")
	}
	// node.Decode drops the parent decoder's KnownFields setting, so a typo
	// inside `backup:` would silently take a default without this.
	if err := checkKnownFields(node, "backup", "to", "schedule", "keep"); err != nil {
		return err
	}
	type plain Backup
	var out plain
	if err := node.Decode(&out); err != nil {
		return err
	}
	*b = Backup(out)
	return nil
}

// SplitRef splits "<group>/<name>", which is how a service, a secret or a
// target is named from outside its group. ok is false for anything else: a
// part missing, or a slash too many.
func SplitRef(ref string) (group, name string, ok bool) {
	group, name, ok = strings.Cut(ref, "/")
	return group, name, ok && group != "" && name != "" && !strings.Contains(name, "/")
}

// TargetRef splits a backup's "<group>/<service>" reference to its target.
func TargetRef(ref string) (group, service string, err error) {
	group, service, ok := SplitRef(ref)
	if !ok {
		return "", "", fmt.Errorf(
			"backup.to %q must be <group>/<service>, e.g. storage/offsite", ref)
	}
	return group, service, nil
}

// IsTarget reports whether this document declares a target rather than a
// container.
func (s *Service) IsTarget() bool { return s.Target != "" }
