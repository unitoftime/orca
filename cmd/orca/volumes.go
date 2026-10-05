package main

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/unitoftime/orca/internal/deploy"
	"github.com/unitoftime/orca/internal/manifest"
)

// VolumeDir is a volume directory and the user that must own it.
type VolumeDir struct {
	Path  string
	Owner int

	// Node is the machine the volume lives on, empty when placement is left
	// to Nomad.
	Node string

	// VersionFile is a file in it naming the version its data was written
	// by, empty when it has none. See manifest.TemplateSpec.VersionFile.
	VersionFile string
}

// VolumeFacts is what a volume directory holds, as far as deploying over it
// is concerned.
type VolumeFacts struct {
	// HasData is a directory that exists and is not empty.
	HasData bool

	// Version is its VersionFile's contents, empty without one.
	Version string

	// Used is how much it holds, in bytes; -1 when it was not measured or
	// the measuring took too long.
	Used int64
}

// InspectVolumes reports what each volume directory on this machine holds,
// keyed by path. It only reads. measure adds how much each one holds, which
// walks every file in it, so it is asked for only where it is shown.
func (n Node) InspectVolumes(ctx context.Context, dirs []VolumeDir, measure bool) (map[string]VolumeFacts, error) {
	if len(dirs) == 0 {
		return map[string]VolumeFacts{}, nil
	}
	out, err := n.RunOutput(ctx, inspectVolumesScript(dirs, measure))
	if err != nil {
		return nil, fmt.Errorf("inspect volumes: %w", err)
	}
	return parseVolumeFacts(out), nil
}

// inspectVolumesScript prints one line per directory:
// <path> TAB <has data 0|1> TAB <used KiB, or -1> TAB <version>.
func inspectVolumesScript(dirs []VolumeDir, measure bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, `inspect() {
  data=0; used=-1; ver=
  if [ -d "$1" ] && [ -n "$(ls -A "$1" 2>/dev/null)" ]; then
    data=1
    [ -n "$2" ] && ver=$(head -c 64 "$1/$2" 2>/dev/null | tr -d '\t\n' || true)
    if [ %t = true ]; then used=$(timeout 10 du -sk "$1" 2>/dev/null | cut -f1) || used=-1; fi
  fi
  printf '%%s\t%%s\t%%s\t%%s\n' "$1" "$data" "${used:--1}" "$ver"
}
`, measure)
	for _, d := range dirs {
		fmt.Fprintf(&b, "inspect %s %s\n", shQuote(d.Path), shQuote(d.VersionFile))
	}
	return b.String()
}

func parseVolumeFacts(out string) map[string]VolumeFacts {
	facts := map[string]VolumeFacts{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(line, "\t", 4)
		if len(f) < 3 {
			continue
		}
		used, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || used < 0 {
			used = -1
		} else {
			used *= 1024
		}
		v := VolumeFacts{HasData: f[1] == "1", Used: used}
		if len(f) == 4 {
			v.Version = strings.TrimSpace(f[3])
		}
		facts[f[0]] = v
	}
	return facts
}

// EnsureDirs creates host directories for volumes before the jobs that bind
// them start.
//
// Docker would create a missing bind-mount source itself, but as root and at
// whatever path was asked for. Creating them deliberately keeps every byte
// orca writes under the data directory. The ownership matters for the same
// reason: a bind mount keeps the host's ownership, so an image that drops
// privileges cannot write to a directory created as root, and fails at
// startup with a permission error that says nothing about volumes.
//
// Only a directory orca has just created is given an owner. Changing the
// ownership of one that already holds data would be a surprising thing to do
// to a database.
func (n Node) EnsureDirs(ctx context.Context, dirs []VolumeDir) error {
	if len(dirs) == 0 {
		return nil
	}
	if err := n.RunQuiet(ctx, ensureDirsScript(dirs)); err != nil {
		return fmt.Errorf("create volume directories: %w", err)
	}
	return nil
}

// ensureDirsScript is EnsureDirs' script, separate so it can be asserted on
// without a machine.
func ensureDirsScript(dirs []VolumeDir) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, d := range dirs {
		fmt.Fprintf(&b, "if [ ! -d %s ]; then\n", shQuote(d.Path))
		fmt.Fprintf(&b, "  mkdir -p %s\n", shQuote(d.Path))
		if d.Owner != 0 {
			fmt.Fprintf(&b, "  chown %d:%d %s\n", d.Owner, d.Owner, shQuote(d.Path))
		}
		b.WriteString("fi\n")
	}
	return b.String()
}

// Volume identifies one service's volume on disk.
type Volume struct {
	Group   string
	Service string
}

// Path is where the volume lives on its machine.
func (v Volume) Path() string { return deploy.VolumePath(dataDir, v.Group, v.Service) }

// VolumeListing is every service volume one machine holds.
type VolumeListing struct {
	Volumes []Volume
}

// ListVolumes lists the volumes on this machine.
//
// Listing the one directory everything lives under is what stops "kept" from
// meaning "invisible": data whose service was removed is found by looking,
// so nothing has to be remembered for it to be findable later.
func (n Node) ListVolumes(ctx context.Context) (VolumeListing, error) {
	script := fmt.Sprintf(`set -e
if [ -d %[1]s ]; then
  cd %[1]s && find . -mindepth 2 -maxdepth 2 -type d | sed 's|^\./||'
fi`, shQuote(deploy.VolumeRoot(dataDir)))

	out, err := n.RunOutput(ctx, script)
	if err != nil {
		return VolumeListing{}, fmt.Errorf("list volumes: %w", err)
	}
	return parseVolumeListing(out), nil
}

func parseVolumeListing(out string) VolumeListing {
	var l VolumeListing
	for _, line := range strings.Split(out, "\n") {
		g, s, ok := strings.Cut(strings.TrimSpace(line), "/")
		if ok && g != "" && s != "" {
			l.Volumes = append(l.Volumes, Volume{Group: g, Service: s})
		}
	}
	sort.Slice(l.Volumes, func(i, j int) bool {
		if l.Volumes[i].Group != l.Volumes[j].Group {
			return l.Volumes[i].Group < l.Volumes[j].Group
		}
		return l.Volumes[i].Service < l.Volumes[j].Service
	})
	return l
}

// validVolumePart is the guard in front of an rm -rf.
//
// A group or service is one directory name, never a path. The inputs come from
// a listing of the volume root, so this can only fire on a bug, which is
// exactly when a check in front of a recursive delete earns its place.
func validVolumePart(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("refusing to delete a volume with an empty name")
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("refusing to delete volume %q: a name is one directory, not a path", name)
	case strings.Contains(name, ".."):
		return fmt.Errorf("refusing to delete volume %q: contains ..", name)
	case strings.HasPrefix(name, "-"):
		return fmt.Errorf("refusing to delete volume %q: would be read as a flag", name)
	}
	return nil
}

// DeleteVolumes permanently removes volumes, then the group directories they
// leave empty.
func (n Node) DeleteVolumes(ctx context.Context, vols []Volume) error {
	script, err := deleteVolumesScript(vols)
	if err != nil {
		return err
	}
	if err := n.RunQuiet(ctx, script); err != nil {
		return fmt.Errorf("delete volumes: %w", err)
	}
	return nil
}

func deleteVolumesScript(vols []Volume) (string, error) {
	var b strings.Builder
	b.WriteString("set -e\n")
	groups := map[string]bool{}
	for _, v := range vols {
		for _, part := range []string{v.Group, v.Service} {
			if err := validVolumePart(part); err != nil {
				return "", err
			}
		}
		fmt.Fprintf(&b, "rm -rf %s\n", shQuote(v.Path()))
		groups[v.Group] = true
	}
	for g := range groups {
		fmt.Fprintf(&b, "rmdir --ignore-fail-on-non-empty %s\n", shQuote(path.Join(deploy.VolumeRoot(dataDir), g)))
	}
	return b.String(), nil
}

// declaredVolumeDirs is the host directory of every volume these groups
// declare, and the machine it is on.
func declaredVolumeDirs(cfg Config, groups []*manifest.Manifest) ([]VolumeDir, error) {
	var dirs []VolumeDir
	var errs []error
	for _, m := range groups {
		for _, s := range m.Services {
			if s.Volume == nil {
				continue
			}
			node, err := nodeFor(cfg, s)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: service %q: %w", m.Path, s.Name, err))
				continue
			}
			d := VolumeDir{
				Path:  deploy.VolumePath(dataDir, m.Group, s.Name),
				Owner: deploy.VolumeOwner(s),
				Node:  node,
			}
			if t := s.Tmpl(); t != nil {
				d.VersionFile = t.Spec().VersionFile
			}
			dirs = append(dirs, d)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Path < dirs[j].Path })
	return dirs, errors.Join(errs...)
}

// ensureVolumeDirs creates each volume's directory on the machine that volume
// lives on.
//
// Not on whichever machine orca happens to be talking to: a service pinned
// elsewhere would then find no directory, and Docker would create one as root
// that the image cannot write to.
func ensureVolumeDirs(ctx context.Context, cfg Config, fallback Node, dirs []VolumeDir) error {
	return onVolumeNodes(cfg, fallback, dirs, func(n Node, list []VolumeDir) error {
		return n.EnsureDirs(ctx, list)
	})
}

// inspectVolumes reports what every volume holds, asking each machine about
// its own. It only reads, so plan runs it too.
func inspectVolumes(ctx context.Context, cfg Config, fallback Node, dirs []VolumeDir, measure bool) (map[string]VolumeFacts, error) {
	facts := map[string]VolumeFacts{}
	err := onVolumeNodes(cfg, fallback, dirs, func(n Node, list []VolumeDir) error {
		got, err := n.InspectVolumes(ctx, list, measure)
		for path, f := range got {
			facts[path] = f
		}
		return err
	})
	return facts, err
}

// onVolumeNodes runs fn once per machine holding any of dirs, with that
// machine's share of them. A volume left to Nomad goes to fallback.
func onVolumeNodes(cfg Config, fallback Node, dirs []VolumeDir, fn func(Node, []VolumeDir) error) error {
	byNode := map[string][]VolumeDir{}
	for _, d := range dirs {
		byNode[d.Node] = append(byNode[d.Node], d)
	}

	for node, list := range byNode {
		target := fallback
		if node != "" {
			nc, ok := cfg.FindNode(node)
			if !ok {
				return fmt.Errorf("volume pinned to unknown node %q", node)
			}
			target = nc.Node()
		}
		if err := fn(target, list); err != nil {
			return err
		}
	}
	return nil
}

// checkVolumePlacement refuses to run a service with a volume on a machine
// other than the one holding its data.
//
// Where a volume lives is worked out from the config on every apply, not
// remembered: its `node:`, or else the first server. Reordering the machines,
// adding one above the first or giving a database a `node:` all move it, and
// on the new machine it starts against an empty directory, initializes itself
// and reports healthy while holding nothing. So above one machine every
// volume is looked for on every machine, and data found anywhere but where the
// service is going stops the apply. A machine that cannot be reached is warned
// about: it may hold data this cannot see.
func checkVolumePlacement(ctx context.Context, cfg Config, dirs []VolumeDir) error {
	if !cfg.MultiNode() || len(dirs) == 0 {
		return nil
	}

	holders := map[string][]string{} // volume path: machines holding data there
	for _, nc := range cfg.Nodes {
		facts, err := nc.Node().InspectVolumes(ctx, dirs, false)
		if unreachable(err) {
			fmt.Printf("warning: node %s is unreachable; volumes on it could not be checked\n", nc.Name)
			continue
		}
		if err != nil {
			return fmt.Errorf("node %s: %w", nc.Name, err)
		}
		for path, f := range facts {
			if f.HasData {
				holders[path] = append(holders[path], nc.Name)
			}
		}
	}
	return misplacedVolumes(dirs, holders)
}

// misplacedVolumes is checkVolumePlacement's judgement, without the machines.
func misplacedVolumes(dirs []VolumeDir, holders map[string][]string) error {
	var errs []error
	for _, d := range dirs {
		on := holders[d.Path]
		if len(on) == 0 || slices.Contains(on, d.Node) {
			continue
		}
		errs = append(errs, fmt.Errorf(
			"%s holds data on %s, but would now run on %s, against an empty directory; "+
				"pin it where its data is with `node: %s`, or move the data first",
			d.Path, strings.Join(on, ", "), d.Node, on[0]))
	}
	return errors.Join(errs...)
}

// checkVolumeData refuses to deploy over data a service could not start on.
//
// Both cases look fine to Nomad and fail only once the container is running:
// Postgres crash-loops on another major version's data directory, and a
// database whose password was lost with the cluster's secrets, then generated
// afresh, starts but lets nothing in. Neither is fixed by retrying, and both
// are worse after the old version has been stopped.
func checkVolumeData(groups []*manifest.Manifest, facts map[string]VolumeFacts, toGenerate []generatedSecret) error {
	var errs []error
	for _, m := range groups {
		for _, s := range m.Services {
			t := s.Tmpl()
			if t == nil || s.Volume == nil || t.Spec().VersionFile == "" {
				continue
			}
			f := facts[deploy.VolumePath(dataDir, m.Group, s.Name)]
			if f.Version != "" && f.Version != t.Version {
				errs = append(errs, fmt.Errorf(
					"%s/%s: its data was written by %s %s, and %s cannot start on it; keep %s:%s, or dump the data, move the volume aside and restore into %s",
					m.Group, s.Name, t.Name, f.Version, t, t.Name, f.Version, t))
			}
		}
	}
	for _, g := range toGenerate {
		if !g.spec.SetAtInit {
			continue
		}
		if f := facts[deploy.VolumePath(dataDir, g.Group, g.service)]; f.HasData {
			errs = append(errs, fmt.Errorf(
				"%s/%s is not set, but %s/%s already holds data, which accepts only the value it was created with; "+
					"set that with `orca secret set --force %s/%s`, or remove the data to start again",
				g.Group, g.Name, g.Group, g.service, g.Group, g.Name))
		}
	}
	return errors.Join(errs...)
}
