package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const usage = `orca — deploy things onto bare metal you own

Usage:
  orca <command> [args]

orca works on the directory tree rooted at the nearest cluster.yaml, the way
git works on the tree rooted at the nearest .git. Each directory beside it is
a group of services; the directory is the group's name.

  infra/
    cluster.yaml      the machines, and cluster-wide settings
    blog/             group "blog"
      db.yaml
      web.yaml
    notifier/         group "notifier"
      notifier.yaml

Commands:
  bootstrap [node]   Bring a machine to a ready state: packages, docker, nomad.
                     Runs against every node in the config unless one is named.
  validate [group..] Parse and check the manifests without touching a machine.
  plan [group...]    Show what apply would change. Changes nothing.
  apply [group...]   Converge the cluster to the manifests. With no argument,
                     everything; named groups narrow the scope and anything
                     outside it is left alone. Stopping a service the manifests
                     no longer declare asks first; --yes skips the question.
  status [group...]  Show what is actually running, and whether it is healthy.
  top                Every node, store and service with its status: each node's
                     CPU, memory, disks and network, and what each service is
                     using. What status.<domain> shows, read over SSH. Flags:
                     -w refresh, --json
  password           The dashboards' password (user "admin"), generated for the
                     cluster at bootstrap.
  password set       Change it. Read from stdin, or prompted; takes effect at
                     the next apply.
  stop <group>       Stop a group's services, keeping its data. The manifests
                     are unchanged, so the next apply brings it back.
  purge <group>      Delete a group AND its data. Irreversible; only works on
                     a group the manifests no longer declare.
  logs [target]      Query the log store. A target is a group, a service, or
                     <group>/<service>; extra words are searched for.
                     Flags: -f follow, --since 30m, -n 200
  db list <g>/<s>    List the backups held for a database.
  db restore <g>/<s> Restore a database from a backup (newest by default).
  secret set <g>/<n> Set a secret. The value is read from stdin, or prompted.
  secret list        Every secret the manifests reference, and whether it is set.
  secret rm <g>/<n>  Remove a secret. A secret orca generated for a template
                     is refused by set and rm unless --force is given.
  registry login <h> Give the cluster credentials to pull private images from a
                     registry (ghcr.io, docker.io, ...). Prompts for the
                     username unless -u is given; the token is read from
                     stdin, or prompted.
  registry list      The registries the cluster has credentials for.
  registry logout <h> Remove a registry's credentials.
  nodes              List the machines in the config.
  version            Print orca's version and the stack it installs.

Flags:
  -C <dir>           Directory to search upward from (default ".")
  --ssh-copy-id      Authorize your SSH key on the host first (fresh boxes)
`

const version = "0.0.1"

// build is the git revision this binary was built from, stamped by the
// Makefile. Two builds of an unreleased tool are otherwise indistinguishable,
// and telling them apart matters more here than it would for a released one:
// the binary on your PATH came from `make install` at some point in the past,
// and the tree has moved since.
var build = "unknown"

func main() {
	var (
		rootDir   = flag.String("C", ".", "directory to search upward from for cluster.yaml")
		sshCopyID = flag.Bool("ssh-copy-id", false, "authorize your SSH key on the host before bootstrapping")
	)
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	// Ctrl-C cancels the in-flight ssh command rather than orphaning it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, args, *rootDir, *sshCopyID); err != nil {
		fmt.Fprintf(os.Stderr, "\norca: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, rootDir string, sshCopyID bool) error {
	cmd, rest := args[0], args[1:]

	// Go's flag package stops at the first non-flag argument, so a global flag
	// written after the command is not parsed; it arrives here as a
	// positional. Silently ignored, `orca validate -C infra` would search the
	// working directory instead and report that there is no cluster.yaml in
	// it: an error about the wrong thing entirely.
	for _, a := range rest {
		switch a {
		case "-C", "--ssh-copy-id":
			return fmt.Errorf("%s is a global flag and has to come before the command: orca %s ... %s", a, a, cmd)
		}
	}

	// The cluster config is loaded on demand, not up front, so `version` works
	// before there is one. `validate` needs it to find the tree's root, but
	// touches no machine.
	var cfg Config
	loaded := false
	needConfig := func() error {
		if loaded {
			return nil
		}
		c, err := LoadConfigFrom(rootDir)
		if err != nil {
			return err
		}
		cfg, loaded = c, true
		return nil
	}

	switch cmd {
	case "version":
		fmt.Printf("orca %s (%s)\n  nomad  %s\n  docker %s\n", version, build, Versions.Nomad, Versions.Docker)
		return nil

	case "validate":
		if err := needConfig(); err != nil {
			return err
		}
		return cmdValidate(cfg, rest)

	case "serve-status":
		// What the status job runs on the machine, where there is no
		// cluster.yaml; not listed in the usage.
		return cmdServeStatus(ctx, rest)
	}

	if err := needConfig(); err != nil {
		return err
	}

	switch cmd {
	case "bootstrap":
		ref := ""
		if len(rest) > 0 {
			ref = rest[0]
		}
		return cmdBootstrap(ctx, cfg, ref, sshCopyID)

	case "apply":
		return cmdApply(ctx, cfg, rest, false)

	case "plan":
		return cmdApply(ctx, cfg, rest, true)

	case "stop":
		return cmdStop(ctx, cfg, rest)

	case "purge":
		return cmdPurge(ctx, cfg, rest)

	case "logs":
		return cmdLogs(ctx, cfg, rest)

	case "db":
		return cmdDB(ctx, cfg, rest)

	case "secret":
		return cmdSecret(ctx, cfg, rest)

	case "registry":
		return cmdRegistry(ctx, cfg, rest)

	case "status":
		return cmdStatus(ctx, cfg, rest)

	case "top":
		return cmdTop(ctx, cfg, rest)

	case "password":
		return cmdPassword(ctx, cfg, rest)

	case "nodes":
		for _, n := range cfg.Nodes {
			ip := n.PrivateIP
			if ip == "" {
				ip = "(loopback: single machine)"
			}
			fmt.Printf("%-16s %-24s %-8s %s\n", n.Name, n.Host, n.Role, ip)
		}
		return nil

	default:
		return fmt.Errorf("unknown command %q (try: orca -h)", cmd)
	}
}
