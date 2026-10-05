package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// cmdReboot implements `orca reboot [node] [--yes]`: restart one machine, and
// wait until it and the scheduler on it are back.
//
// orca never restarts a machine on its own. Security updates install
// themselves, and one that needs a reboot (a kernel, mostly) waits, shown as
// "reboot required" by `orca top` and the status page, until someone decides
// when everything on that machine can go away for a minute. This is how they
// say so.
func cmdReboot(ctx context.Context, cfg Config, args []string) error {
	yes := false
	var rest []string
	for _, a := range args {
		if a == "--yes" || a == "-y" {
			yes = true
			continue
		}
		rest = append(rest, a)
	}

	var nc NodeConfig
	switch {
	case len(rest) == 1:
		n, ok := cfg.FindNode(rest[0])
		if !ok {
			return fmt.Errorf("no machine named %q; the machines are: %s", rest[0], nodeNames(cfg))
		}
		nc = n
	case len(rest) == 0 && len(cfg.Nodes) == 1:
		nc = cfg.Nodes[0]
	default:
		return fmt.Errorf("usage: orca reboot <node> [--yes]; the machines are: %s", nodeNames(cfg))
	}
	node := Node{Host: nc.Host}

	// Read before anything is asked, so a machine that cannot be reached is
	// said so now. It is also what tells "back" from "has not gone down yet":
	// the kernel makes a new one at every boot.
	before, err := bootID(ctx, node)
	if err != nil {
		return fmt.Errorf("reach %s: %w", nc.Name, err)
	}

	fmt.Printf("Everything running on %s stops until it is back, usually a minute or two.\n", nc.Name)
	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("this would reboot %s and there is no terminal to confirm at; re-run with --yes if that is what you meant", nc.Name)
		}
		if !confirmYes(fmt.Sprintf("reboot %s? [y/N] ", nc.Name)) {
			return fmt.Errorf("cancelled")
		}
	}

	// Detached and a moment late, so this command has returned before the
	// machine takes the connection away under it.
	fmt.Printf("  reboot  %s ... ", nc.Name)
	if err := node.RunQuiet(ctx, "nohup sh -c 'sleep 2; systemctl reboot' >/dev/null 2>&1 &"); err != nil {
		fmt.Println("FAILED")
		return err
	}
	if err := waitForBoot(ctx, node, before); err != nil {
		fmt.Println("FAILED")
		return fmt.Errorf("%s: %w", nc.Name, err)
	}
	fmt.Println("back")

	// The scheduler starts with the machine and restarts what was running
	// from its own saved state; nothing has to be applied again.
	server := node
	if nc.Role != RoleServer {
		server = Node{Host: cfg.Servers()[0].Host}
	}
	if err := waitForNomad(ctx, server); err != nil {
		return err
	}
	if len(cfg.Nodes) > 1 {
		if err := waitForNodes(ctx, server, len(cfg.Nodes)); err != nil {
			return err
		}
	}
	fmt.Println("its services are starting again; `orca status` shows them")
	return nil
}

func nodeNames(cfg Config) string {
	names := make([]string, len(cfg.Nodes))
	for i, n := range cfg.Nodes {
		names[i] = n.Name
	}
	return strings.Join(names, ", ")
}

const (
	rebootWait = 10 * time.Minute
	rebootPoll = 5 * time.Second
)

// bootID identifies the machine's current boot.
func bootID(ctx context.Context, node Node) (string, error) {
	// RunStdin, for its quiet: the waiting below asks a machine that is down
	// over and over, and every refusal would otherwise be printed.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := node.RunStdin(ctx, "cat /proc/sys/kernel/random/boot_id", nil)
	return strings.TrimSpace(out), err
}

// waitForBoot waits for the machine to answer from a boot other than the one
// it was in.
func waitForBoot(ctx context.Context, node Node, before string) error {
	for deadline := time.Now().Add(rebootWait); time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(rebootPoll):
		}
		if id, err := bootID(ctx, node); err == nil && id != "" && id != before {
			return nil
		}
	}
	return fmt.Errorf("has not come back within %s; look at it from your provider's console", rebootWait)
}
