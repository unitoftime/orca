package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/unitoftime/orca/pkg/deploy"
	"github.com/unitoftime/orca/pkg/manifest"
)

// resolveBackup turns a service's `backup:` policy into the spec that builds
// its periodic job, by finding the target it names.
//
// The target is looked up here rather than during manifest validation because
// it is usually in another group, and a group is validated on its own. This is
// the same split as `node:`, which is checked for shape in the manifest and for
// existence by apply.
func resolveBackup(groups []*manifest.Manifest, s *manifest.Service) (deploy.BackupSpec, error) {
	group, name, err := manifest.TargetRef(s.Backup.To)
	if err != nil {
		return deploy.BackupSpec{}, err
	}

	var target *manifest.Service
	for _, m := range groups {
		if m.App != group {
			continue
		}
		if svc, ok := m.Service(name); ok {
			target = svc
		}
	}
	if target == nil {
		return deploy.BackupSpec{}, fmt.Errorf(
			"backup.to %q names no service; declared targets: %s",
			s.Backup.To, describeTargets(groups))
	}
	// A backup pointed at a database rather than a store would create the job
	// and fail inside a container nobody is watching.
	if !target.IsTarget() {
		return deploy.BackupSpec{}, fmt.Errorf(
			"backup.to %q is a service, not a target; declared targets: %s",
			s.Backup.To, describeTargets(groups))
	}

	return deploy.BackupSpec{
		Endpoint:        target.Endpoint,
		Bucket:          target.Bucket,
		Region:          target.Region,
		Schedule:        s.Backup.Schedule,
		Keep:            s.Backup.Keep,
		Image:           "rclone/rclone:" + Versions.Rclone,
		SecretGroup:     group,
		KeyIDSecret:     manifest.TargetKeyID(name),
		SecretKeySecret: manifest.TargetSecretKey(name),
	}, nil
}

// describeTargets lists every target declared anywhere, so a bad reference
// names what does exist instead of only what does not.
func describeTargets(groups []*manifest.Manifest) string {
	var out []string
	for _, m := range groups {
		for _, s := range m.Services {
			if s.IsTarget() {
				out = append(out, m.App+"/"+s.Name)
			}
		}
	}
	if len(out) == 0 {
		return "none (declare one with `target: s3`)"
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// backedUp is every service in scope that has a backup policy, paired with the
// resolved spec. Reported together so one bad reference does not hide another.
type backedUp struct {
	Manifest *manifest.Manifest
	Service  *manifest.Service
	Spec     deploy.BackupSpec
}

func collectBackups(all []*manifest.Manifest, scope []*manifest.Manifest) ([]backedUp, error) {
	var out []backedUp
	var errs []string
	for _, m := range scope {
		for _, s := range m.Services {
			if s.Backup == nil {
				continue
			}
			spec, err := resolveBackup(all, s)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s/%s: %v", m.App, s.Name, err))
				continue
			}
			out = append(out, backedUp{Manifest: m, Service: s, Spec: spec})
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	return out, nil
}
