package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/unitoftime/orca/pkg/registry"
	"golang.org/x/term"
)

// cmdRegistry implements `orca registry login|list|logout`: the credentials
// the cluster's machines pull private images with.
//
// They are the cluster's, not yours. apply resolves digests with your own
// docker credentials, but the pull happens on the machine, which has none of
// them — so an image that is private needs a login here as well, and apply
// refuses to deploy one that has none.
//
// Credentials live in Nomad's variable store like every other secret, one
// variable per registry host. Nomad's docker driver reads them at pull time
// through a credential helper bootstrap installs on every machine, so they are
// never written into a job spec or a container's environment, and changing
// them redeploys nothing.
func cmdRegistry(ctx context.Context, cfg Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: orca registry login|list|logout [<host>]")
	}

	switch args[0] {
	case "login":
		host, username, err := parseLoginArgs(args[1:])
		if err != nil {
			return err
		}
		return registryLogin(ctx, cfg, host, username)
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("usage: orca registry list")
		}
		return registryList(ctx, cfg)
	case "logout":
		if len(args) != 2 {
			return fmt.Errorf("usage: orca registry logout <host>")
		}
		return registryLogout(ctx, cfg, args[1])
	default:
		return fmt.Errorf("unknown registry command %q (login, list, logout)", args[0])
	}
}

func parseLoginArgs(args []string) (host, username string, err error) {
	const usage = "usage: orca registry login <host> [-u <username>]"
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-u" || a == "--username":
			if i+1 >= len(args) {
				return "", "", errors.New(usage)
			}
			i++
			username = args[i]
		case strings.HasPrefix(a, "--username="):
			username = strings.TrimPrefix(a, "--username=")
		case strings.HasPrefix(a, "-"):
			return "", "", fmt.Errorf("unknown flag %q\n%s", a, usage)
		case host == "":
			host = a
		default:
			return "", "", errors.New(usage)
		}
	}
	if host == "" {
		return "", "", errors.New(usage)
	}
	host, err = registry.Host(host)
	return host, username, err
}

func registryLogin(ctx context.Context, cfg Config, host, username string) error {
	if username == "" {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("pass the username with -u when the token comes from stdin: orca registry login %s -u <username>", host)
		}
		fmt.Fprintf(os.Stderr, "username for %s: ", host)
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return fmt.Errorf("read username: %w", err)
		}
		username = strings.TrimSpace(line)
		if username == "" {
			return fmt.Errorf("refusing an empty username")
		}
	}

	// The same reading rules as a secret: piped input whole, or a prompt with
	// no echo, and never an argument — a command line is in the process table
	// and the shell's history.
	token, err := readSecretValue("token for " + host)
	if err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("refusing an empty token")
	}

	// From here, before storing anything, so a typo or an expired token is
	// caught with someone at the keyboard rather than at a pull on the machine.
	if err := registry.Verify(ctx, host, username, token); err != nil {
		return err
	}

	cluster, err := clusterFor(ctx, cfg)
	if err != nil {
		return err
	}
	if err := cluster.PutRegistry(ctx, host, username, token); err != nil {
		return err
	}
	fmt.Printf("logged in to %s as %s\n", host, username)

	// Stored is not usable: a machine bootstrapped before the helper existed
	// pulls anonymously whatever the store holds. Said now, rather than left
	// to be found as "unauthorized" at the next deploy.
	var stale []string
	for _, nc := range cfg.Nodes {
		ok, err := NewCluster(Node{Host: nc.Host}).HasCredentialHelper(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not check %s for the credential helper: %v\n", nc.Name, err)
			continue
		}
		if !ok {
			stale = append(stale, nc.Name)
		}
	}
	if len(stale) > 0 {
		fmt.Printf("\n%s cannot use it yet: bootstrapped by an older orca, it pulls with no credentials.\n"+
			"run: orca bootstrap\n", strings.Join(stale, ", "))
	}
	return nil
}

func registryList(ctx context.Context, cfg Config) error {
	cluster, err := clusterFor(ctx, cfg)
	if err != nil {
		return err
	}
	hosts, err := cluster.RegistryHosts(ctx)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		fmt.Println("no registry credentials set")
		return nil
	}
	for _, h := range sortedKeys(hosts) {
		fmt.Println(h)
	}
	return nil
}

func registryLogout(ctx context.Context, cfg Config, raw string) error {
	host, err := registry.Host(raw)
	if err != nil {
		return err
	}
	cluster, err := clusterFor(ctx, cfg)
	if err != nil {
		return err
	}
	hosts, err := cluster.RegistryHosts(ctx)
	if err != nil {
		return err
	}
	if !hosts[host] {
		return fmt.Errorf("the cluster has no credentials for %s", host)
	}
	if err := cluster.DeleteRegistry(ctx, host); err != nil {
		return err
	}
	fmt.Printf("logged out of %s\n", host)
	return nil
}

// imageResolver pins images to digests for one apply, and remembers which
// registries would not serve an image without credentials.
type imageResolver struct {
	ctx context.Context
	r   registry.Resolver

	// private is every image that needed credentials to resolve, by the
	// registry it is pulled from.
	private map[string][]string
}

func newImageResolver(ctx context.Context, r registry.Resolver) *imageResolver {
	return &imageResolver{ctx: ctx, r: r, private: map[string][]string{}}
}

// Pin resolves one reference to a digest.
func (ir *imageResolver) Pin(ref string) (string, error) {
	p, err := ir.r.Resolve(ir.ctx, ref)
	if err != nil {
		return "", err
	}
	if p.Private {
		ir.private[p.Registry] = append(ir.private[p.Registry], ref)
	}
	return p.Ref, nil
}

// preflightRegistries refuses an apply whose private images the cluster has
// no credentials to pull. It asks the cluster only when there is a private
// image, so an apply of public images costs nothing more than it did.
func preflightRegistries(ctx context.Context, cluster *Cluster, private map[string][]string) error {
	if len(private) == 0 {
		return nil
	}
	have, err := cluster.RegistryHosts(ctx)
	if err != nil {
		return err
	}
	return checkRegistries(private, have)
}

// checkRegistries is preflightRegistries' judgement, without the cluster.
func checkRegistries(private map[string][]string, have map[string]bool) error {
	var lines []string
	for _, host := range sortedKeys(private) {
		if have[host] {
			continue
		}
		refs := append([]string(nil), private[host]...)
		sort.Strings(refs)
		lines = append(lines, fmt.Sprintf("  %-20s %s", host, strings.Join(dedupe(refs), ", ")))
	}
	if len(lines) == 0 {
		return nil
	}
	return errors.New("these images are private, and the cluster has no credentials for their registry:\n" +
		strings.Join(lines, "\n") +
		"\n\nlog in with: orca registry login <host>")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dedupe drops adjacent repeats from a sorted list.
func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}
