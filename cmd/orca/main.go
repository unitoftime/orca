package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
)

// build names this build where Go has not recorded it: only in the one orca
// builds for the machines, from a copy of its source that has no history.
var build string

// buildVersion says which build of orca this is, and whether it is one that
// `go install` made from a published version, which can be fetched and built
// again.
//
// Go records it in the binary: the version asked for, or for a build from a
// checkout the commit, its time and whether the tree had changes. So two
// builds are told apart without a number kept by hand, which matters most for
// the one on your PATH from `make install` at some point in the past.
func buildVersion() (version string, published bool) {
	if build != "" {
		return build, false
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "unknown", false
	}
	for _, s := range info.Settings {
		if s.Key == "vcs" {
			return info.Main.Version, false
		}
	}
	return info.Main.Version, info.Main.Path == modulePath
}

func main() {
	line, err := parseCommandLine(os.Args[1:])
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "orca: %v\n", strings.TrimPrefix(err.Error(), errUsage.Error()+": "))
		os.Exit(2)
	case line.help && line.cmd == nil:
		fmt.Print(usage())
		return
	case line.help:
		fmt.Print(line.cmd.usage())
		return
	}

	// Ctrl-C cancels the in-flight ssh command rather than orphaning it. A
	// closed terminal is handled the same way, not left to end the process
	// where it stands: `orca secret edit` has a plaintext file to remove.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	if err := run(ctx, line); err != nil {
		fmt.Fprintf(os.Stderr, "\norca: %v\n", err)
		os.Exit(1)
	}
}

// run does what a command line asks. The cluster config is loaded only for a
// command that needs one, so `version` works before there is a cluster.yaml.
func run(ctx context.Context, line commandLine) error {
	var cfg Config
	if !line.cmd.offline {
		root := line.root
		if root == "" {
			root = "."
		}
		var err error
		if cfg, err = loadConfigFrom(root); err != nil {
			return err
		}
	}
	return line.cmd.run(ctx, cfg, line.in)
}

// cmdNodes lists the machines in cluster.yaml.
func cmdNodes(cfg Config) error {
	for _, n := range cfg.Nodes {
		ip := n.PrivateIP
		if ip == "" {
			ip = "(loopback: single machine)"
		}
		fmt.Printf("%-16s %-24s %-8s %s\n", n.Name, n.Host, n.Role, ip)
	}
	return nil
}
