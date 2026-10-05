package main

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/unitoftime/orca/pkg/deploy"
	"github.com/unitoftime/orca/pkg/manifest"
)

// cmdDB implements `orca db list|restore`.
//
// A backup nobody has restored is a hope, not a backup — so restoring is a
// first-class command rather than a runbook, and it runs the same way every
// time.
func cmdDB(ctx context.Context, cfg Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: orca db list|restore <group>/<service>")
	}

	cluster, err := clusterFor(ctx, cfg)
	if err != nil {
		return err
	}

	switch args[0] {
	case "list":
		if len(args) != 2 {
			return fmt.Errorf("usage: orca db list <group>/<service>")
		}
		return dbList(ctx, cfg, cluster, args[1])
	case "restore":
		return dbRestore(ctx, cfg, cluster, args[1:])
	default:
		return fmt.Errorf("unknown db command %q (list, restore)", args[0])
	}
}

// findDatabase resolves "<group>/<service>" to a database that is actually
// backed up, and to the target its backups go to.
//
// Only a service with a `backup:` is findable. One without is not half-set-up,
// it is a database nobody asked to back up, and listing its backups would
// return an empty bucket that looks like data loss.
func findDatabase(cfg Config, ref string) (*manifest.Manifest, *manifest.Service, deploy.BackupSpec, error) {
	group, name, ok := strings.Cut(ref, "/")
	if !ok {
		return nil, nil, deploy.BackupSpec{}, fmt.Errorf("database %q must be <group>/<service>, e.g. shop/db", ref)
	}

	groups, err := loadGroups(cfg)
	if err != nil {
		return nil, nil, deploy.BackupSpec{}, err
	}

	var known []string
	for _, m := range groups {
		for _, s := range m.Services {
			if s.Backup == nil {
				continue
			}
			known = append(known, m.App+"/"+s.Name)
			if m.App == group && s.Name == name {
				spec, err := resolveBackup(groups, m, s)
				return m, s, spec, err
			}
		}
	}

	sort.Strings(known)
	if len(known) == 0 {
		return nil, nil, deploy.BackupSpec{}, fmt.Errorf(
			"no database declares a backup; add backup: {to: <group>/<service>} to one")
	}
	return nil, nil, deploy.BackupSpec{}, fmt.Errorf("no backed-up database %q; known: %s", ref, strings.Join(known, ", "))
}

func dbList(ctx context.Context, cfg Config, cluster *Cluster, ref string) error {
	_, _, spec, err := findDatabase(cfg, ref)
	if err != nil {
		return err
	}

	names, err := cluster.ListBackups(ctx, spec)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Printf("no backups yet for %s\n", ref)
		return nil
	}

	for _, n := range names {
		fmt.Println(" ", n)
	}
	fmt.Printf("\n%d backup(s) in %s/%s\n", len(names), spec.Bucket, spec.Prefix)
	return nil
}

func dbRestore(ctx context.Context, cfg Config, cluster *Cluster, args []string) error {
	yes := false
	var rest []string
	for _, a := range args {
		if a == "--yes" || a == "-y" {
			yes = true
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) < 1 || len(rest) > 2 {
		return fmt.Errorf("usage: orca db restore <group>/<service> [backup] [--yes]")
	}

	m, s, spec, err := findDatabase(cfg, rest[0])
	if err != nil {
		return err
	}
	names, err := cluster.ListBackups(ctx, spec)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("no backups exist for %s", rest[0])
	}

	// Newest by default: names are timestamps, so the last one sorted is the
	// most recent.
	name := names[len(names)-1]
	if len(rest) == 2 {
		name = rest[1]
		found := false
		for _, n := range names {
			if n == name {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("no backup named %q; see `orca db list %s`", name, rest[0])
		}
	}

	kind := s.Tmpl().Spec().Backup

	fmt.Printf("Restoring %s into %s.\n", name, rest[0])
	switch kind {
	case manifest.BackupRedis:
		fmt.Printf("%s is stopped while its data is replaced by the backup's, then started again.\n", rest[0])
		fmt.Printf("The data it holds now is kept under %s-<time>.\n", PreRestorePrefix(m.App, s.Name))
	default:
		fmt.Printf("Each database in it is restored beside the one it replaces, then swapped in, ending connections to it.\n")
		fmt.Printf("What each held before is kept on the server as <name>%s<time>.\n", deploy.PreRestoreSuffix)
	}
	if !yes && !confirm(fmt.Sprintf("type %q to confirm: ", s.Name), s.Name) {
		return fmt.Errorf("cancelled")
	}

	// Checked whichever way the name arrived: typed, or read back from the
	// bucket listing. A name orca did not write is either a stranger's file in
	// your bucket or a typo, and both are worth stopping for.
	if err := checkBackupName(name, kind.Exts()); err != nil {
		return err
	}

	// The database's own image, like the backup uses: a restore run with a
	// different major version's tools is exactly the kind of surprise you do
	// not want during a restore.
	if kind == manifest.BackupRedis {
		// Run where the data is: the swap is a directory on that machine.
		nodeName, err := nodeFor(cfg, s)
		if err != nil {
			return err
		}
		node, _ := cfg.FindNode(nodeName)
		return NewCluster(Node{Host: node.Host}).RestoreRedis(ctx, spec, m.App, s.Name, name, s.ResolvedImage())
	}
	return cluster.RestoreBackup(ctx, spec, m.App, s.Name, name, s.ResolvedImage())
}

// backupName is the shape a backup file has: <group>-<service>-<stamp>.<ext>,
// where the stamp is what `date -u +%Y%m%dT%H%M%SZ` produces and the
// extension says what kind of dump it is.
//
// Checked because the name reaches a shell on the machine. It is quoted there
// too, and the quoting is the real defence. This is the second lock, and the
// one that produces a sentence rather than a silent nothing when a name is not
// what orca wrote. A bucket holding a file orca did not create is worth saying
// out loud either way.
var backupName = regexp.MustCompile(`^[a-z0-9-]+-[0-9]{8}T[0-9]{6}Z\.([a-z]+)$`)

// checkBackupName also refuses the wrong kind of file: a Postgres dump handed
// to a Redis restore would stop the service to load something it cannot.
// exts is every extension the database's kind of backup can have.
func checkBackupName(name string, exts []string) error {
	m := backupName.FindStringSubmatch(name)
	if m == nil || !slices.Contains(exts, m[1]) {
		return fmt.Errorf(
			"backup %q is not a name orca wrote (expected <group>-<service>-20060102T150405Z.%s); refusing to use it",
			name, exts[0])
	}
	return nil
}
