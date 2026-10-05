package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// command is one thing orca can be asked to do. The table of them is the
// whole of orca's command line: what is parsed, what is refused, and every
// line of usage are read from it, so none of them can say something the
// others do not.
type command struct {
	// name is what is typed after `orca`: one word, or two for a command that
	// belongs to a family ("secret set").
	name string

	// args describes what follows it, for a person. min and max are how many
	// of them there may be; max is many for no limit.
	args     string
	min, max int

	flags []flagSpec

	// summary is the one line the list of commands gives it, and help is the
	// rest, shown when the command itself is asked.
	summary string
	help    string

	// offline is a command that needs no cluster.yaml.
	offline bool

	// internal is a command a job runs on a machine, not one a person does:
	// it is left out of the usage, and reads its own arguments.
	internal bool

	run func(ctx context.Context, cfg Config, in invocation) error
}

const many = -1

// flagSpec is one flag a command takes.
type flagSpec struct {
	// name is how the usage writes it, and alias another spelling of it.
	name, alias string

	// value names what a flag that takes one is given, and def is what it is
	// when the flag is left out. A flag with neither is a switch.
	value, def string

	help string
}

// The flags, by the name each is looked up with.
const (
	flagYes    = "--yes"
	flagForce  = "--force"
	flagPlain  = "--plain"
	flagWatch  = "-w"
	flagJSON   = "--json"
	flagFollow = "-f"
	flagSince  = "--since"
	flagLines  = "-n"
	flagUser   = "-u"
)

func yes(help string) flagSpec   { return flagSpec{name: flagYes, alias: "-y", help: help} }
func force(help string) flagSpec { return flagSpec{name: flagForce, help: help} }
func plain(help string) flagSpec { return flagSpec{name: flagPlain, help: help} }

// invocation is a command line once it has been read against a command: the
// arguments in order, and the flags wherever they were.
type invocation struct {
	args  []string
	flags map[string]string
}

// Has reports whether a switch was given.
func (in invocation) Has(flag string) bool { return in.flags[flag] != "" }

// Value is what a flag was given, or its default.
func (in invocation) Value(flag string) string { return in.flags[flag] }

// Arg is the i'th argument, or "" when there were fewer.
func (in invocation) Arg(i int) string {
	if i < len(in.args) {
		return in.args[i]
	}
	return ""
}

// commands is every command, in the order the usage lists them.
var commands = []command{
	{
		name: "bootstrap", args: "[node]", max: 1,
		summary: "set up every machine, or one",
		help: `Brings a machine to a ready state: packages, Docker, Nomad, running and
joined. It is safe to run again, which is also how a new version of orca
reaches the machines. A machine that does not accept your SSH key yet is
given it first, which asks for its password once.`,
		run: func(ctx context.Context, cfg Config, in invocation) error {
			return cmdBootstrap(ctx, cfg, in.Arg(0))
		},
	},
	{
		name: "validate", args: "[group...]", max: many,
		summary: "check the files without touching the server",
		run: func(_ context.Context, cfg Config, in invocation) error {
			return cmdValidate(cfg, in.args)
		},
	},
	{
		name: "plan", args: "[group...]", max: many,
		summary: "show what apply would change",
		help:    `Changes nothing: not a job, not a firewall rule, not a secret.`,
		run: func(ctx context.Context, cfg Config, in invocation) error {
			return cmdApply(ctx, cfg, in, true)
		},
	},
	{
		name: "apply", args: "[group...]", max: many,
		flags:   []flagSpec{yes("stop the services the files no longer declare without asking")},
		summary: "make the server match the files",
		help: `With no argument, everything. Named groups narrow it, and anything outside
them is left alone.`,
		run: func(ctx context.Context, cfg Config, in invocation) error {
			return cmdApply(ctx, cfg, in, false)
		},
	},
	{
		name: "status", args: "[group...]", max: many,
		summary: "what is running, and whether it is healthy",
		run: func(ctx context.Context, cfg Config, in invocation) error {
			return cmdStatus(ctx, cfg, in.args)
		},
	},
	{
		name: "top",
		flags: []flagSpec{
			{name: flagWatch, alias: "--watch", help: fmt.Sprintf("refresh every %s", topInterval)},
			{name: flagJSON, help: "print it once, as JSON"},
		},
		summary: "machines and services at a glance",
		help: `Every machine, store and service with its status: each machine's CPU,
memory, disks and network, and what each service is using. It is what
status.<domain> shows, read over SSH.`,
		run: cmdTop,
	},
	{
		name: "logs", args: "[target] [words...]", max: many,
		flags: []flagSpec{
			{name: flagFollow, alias: "--follow", help: "keep printing new lines as they arrive"},
			{name: flagSince, value: "duration", def: "1h", help: "how far back to read, such as 30m, 24h or 7d"},
			{name: flagLines, alias: "--lines", value: "count", def: "200", help: "how many lines to print"},
		},
		summary: "read or search a service's logs",
		help: `A target is a group, a service, or <group>/<service>; with none, every
service. The words after it are searched for, as one phrase.`,
		run: cmdLogs,
	},
	{
		name: "stop", args: "<group>", min: 1, max: 1,
		summary: "stop a group, keeping its data",
		help:    `The files are unchanged, so the next apply starts it again.`,
		run: func(ctx context.Context, cfg Config, in invocation) error {
			return cmdStop(ctx, cfg, in.Arg(0))
		},
	},
	{
		name: "purge", args: "<group>", min: 1, max: 1,
		flags:   []flagSpec{yes("delete without asking for the group's name to be typed")},
		summary: "delete a removed group and its data",
		help: `Irreversible, and the only command that deletes data. It works only on a
group the files no longer declare.`,
		run: cmdPurge,
	},
	{
		name: "secret set", args: "<group>/<name>", min: 1, max: 1,
		flags:   []flagSpec{force("replace a secret orca generated for a template")},
		summary: "set a secret, read from stdin or prompted",
		run: func(ctx context.Context, cfg Config, in invocation) error {
			return withCluster(ctx, cfg, func(c *Cluster) error {
				return secretSet(ctx, cfg, c, in.Arg(0), in.Has(flagForce))
			})
		},
	},
	{
		name:    "secret list",
		summary: "the secrets referenced, and which are missing",
		run: func(ctx context.Context, cfg Config, in invocation) error {
			return withCluster(ctx, cfg, func(c *Cluster) error { return secretList(ctx, cfg, c) })
		},
	},
	{
		name: "secret rm", args: "<group>/<name>", min: 1, max: 1,
		flags:   []flagSpec{force("remove a secret orca generated for a template")},
		summary: "remove a secret",
		run: func(ctx context.Context, cfg Config, in invocation) error {
			return withCluster(ctx, cfg, func(c *Cluster) error {
				return secretRemove(ctx, cfg, c, in.Arg(0), in.Has(flagForce))
			})
		},
	},
	{
		name: "secret export", args: "[file]", max: 1,
		flags:   []flagSpec{plain("write plaintext instead of encrypting to a passphrase")},
		summary: "write the cluster's secrets to a file",
		help:    `Every secret it holds, encrypted to a passphrase. Without a file, to stdout.`,
		run: func(ctx context.Context, cfg Config, in invocation) error {
			return withCluster(ctx, cfg, func(c *Cluster) error {
				return secretExport(ctx, c, in.Arg(0), in.Has(flagPlain))
			})
		},
	},
	{
		name: "secret import", args: "<file>", min: 1, max: 1,
		flags: []flagSpec{
			yes("apply without asking"),
			force("change a secret orca generated for a template"),
		},
		summary: "put a file's secrets on the cluster",
		help: `Shows what would be added or changed, and asks. It writes only what
differs and never removes anything.`,
		run: func(ctx context.Context, cfg Config, in invocation) error {
			return withCluster(ctx, cfg, func(c *Cluster) error {
				return secretImport(ctx, cfg, c, in.Arg(0), in.Has(flagYes), in.Has(flagForce))
			})
		},
	},
	{
		name: "secret edit", args: "[file]", max: 1,
		flags: []flagSpec{
			plain("the file is plaintext, not encrypted"),
			force("change a secret orca generated for a template"),
		},
		summary: "edit the cluster's or a file's secrets",
		help: `Opens them in $EDITOR. Without a file, the cluster's secrets, and what
changed is applied. With one, the file's, creating it if need be, and no
cluster is touched.`,
		run: func(ctx context.Context, cfg Config, in invocation) error {
			// Editing a file is the one command here that touches no cluster.
			if file := in.Arg(0); file != "" {
				return secretEditFile(file, in.Has(flagPlain))
			}
			return withCluster(ctx, cfg, func(c *Cluster) error {
				return secretEditCluster(ctx, cfg, c, in.Has(flagForce))
			})
		},
	},
	{
		name: "registry login", args: "<host>", min: 1, max: 1,
		flags: []flagSpec{
			{name: flagUser, alias: "--username", value: "user", help: "the username, instead of being asked for it"},
		},
		summary: "log the cluster in to a private registry",
		help: `Gives the cluster credentials to pull private images from a registry
(ghcr.io, docker.io, ...). The token is read from stdin, or prompted. Use a
read-only one: on GitHub, one with only read:packages.`,
		run: func(ctx context.Context, cfg Config, in invocation) error {
			host, username, err := loginArgs(in)
			if err != nil {
				return err
			}
			return registryLogin(ctx, cfg, host, username)
		},
	},
	{
		name:    "registry list",
		summary: "the registries the cluster has a login for",
		run: func(ctx context.Context, cfg Config, in invocation) error {
			return registryList(ctx, cfg)
		},
	},
	{
		name: "registry logout", args: "<host>", min: 1, max: 1,
		summary: "remove a registry's login",
		run: func(ctx context.Context, cfg Config, in invocation) error {
			return registryLogout(ctx, cfg, in.Arg(0))
		},
	},
	{
		name: "db list", args: "<group>/<service>", min: 1, max: 1,
		summary: "a database's backups, oldest first",
		run: func(ctx context.Context, cfg Config, in invocation) error {
			return withCluster(ctx, cfg, func(c *Cluster) error { return dbList(ctx, cfg, c, in.Arg(0)) })
		},
	},
	{
		name: "db restore", args: "<group>/<service> [backup]", min: 1, max: 2,
		flags:   []flagSpec{yes("restore without asking for the service's name to be typed")},
		summary: "restore a backup, the newest by default",
		run: func(ctx context.Context, cfg Config, in invocation) error {
			return withCluster(ctx, cfg, func(c *Cluster) error { return dbRestore(ctx, cfg, c, in) })
		},
	},
	{
		name:    "password",
		summary: "the dashboards' password",
		help:    `The user is "admin". The password is generated for the cluster at bootstrap.`,
		run: func(ctx context.Context, cfg Config, in invocation) error {
			return withCluster(ctx, cfg, func(c *Cluster) error { return passwordShow(ctx, cfg, c) })
		},
	},
	{
		name:    "password set",
		summary: "change the dashboards' password",
		help: fmt.Sprintf(`Read from stdin, or prompted: %d to %d characters. It takes effect at the
next apply.`, minPasswordLength, maxPasswordLength),
		run: func(ctx context.Context, cfg Config, in invocation) error {
			return withCluster(ctx, cfg, func(c *Cluster) error { return passwordSet(ctx, c) })
		},
	},
	{
		name:    "nodes",
		summary: "list the machines",
		run:     func(_ context.Context, cfg Config, _ invocation) error { return cmdNodes(cfg) },
	},
	{
		name: "reboot", args: "[node]", max: 1,
		flags:   []flagSpec{yes("reboot without asking")},
		summary: "restart a machine and wait for it",
		help: `For the "reboot required" an installed update leaves: orca never reboots a
machine itself. The node can be left out when there is only one.`,
		run: cmdReboot,
	},
	{
		name:    "version",
		summary: "orca's version and what it installs",
		offline: true,
		run: func(context.Context, Config, invocation) error {
			version, _ := buildVersion()
			fmt.Printf("orca %s\n  nomad  %s\n  docker %s\n", version, Versions.Nomad, Versions.Docker)
			return nil
		},
	},

	// What the status and certificate jobs run on a machine, where there
	// is no cluster.yaml.
	{name: "serve-status", internal: true, offline: true,
		run: func(ctx context.Context, _ Config, in invocation) error { return cmdServeStatus(ctx, in.args) }},
	{name: "serve-certs", internal: true, offline: true,
		run: func(ctx context.Context, _ Config, in invocation) error { return cmdServeCerts(ctx, in.args) }},
}

// withCluster runs fn against a machine the cluster can be driven through.
func withCluster(ctx context.Context, cfg Config, fn func(*Cluster) error) error {
	cluster, err := clusterFor(ctx, cfg)
	if err != nil {
		return err
	}
	return fn(cluster)
}

// commandLine is what was typed after `orca`, read against the table.
type commandLine struct {
	// root is where to look for cluster.yaml from, "" when it was not said.
	root string

	// help is a request to be told about cmd, or about orca when cmd is nil.
	help bool

	cmd *command
	in  invocation
}

// errUsage marks an error that is the command line's own: orca was asked
// something it does not understand, rather than failing at what it was asked.
var errUsage = errors.New("usage")

func usageError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(format, args...))
}

func findCommand(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

// family is the commands that begin with a word: "secret" has six.
func family(word string) []*command {
	var out []*command
	for i := range commands {
		if strings.HasPrefix(commands[i].name, word+" ") {
			out = append(out, &commands[i])
		}
	}
	return out
}

func (c *command) flag(name string) *flagSpec {
	for i := range c.flags {
		if c.flags[i].name == name || c.flags[i].alias == name {
			return &c.flags[i]
		}
	}
	return nil
}

// parseCommandLine reads what was typed after `orca`.
//
// Flags are recognised wherever they appear, and everything else is an
// argument. Stopping at the first argument would fold the rest of the line
// into it, so `orca logs worker tick -n 3` would search for the literal
// "tick -n 3" and silently find nothing. A flag the command does not take is
// refused rather than read as an argument, for the same reason: `orca status
// --json` would otherwise look for a group by that name and report nothing
// deployed.
func parseCommandLine(args []string) (commandLine, error) {
	var line commandLine

	// global takes -C and -h off the front of args, wherever in the line
	// that is, and reports whether it did.
	global := func() (bool, error) {
		switch a := args[0]; {
		case a == "-h" || a == "--help":
			line.help = true
			args = args[1:]
		case a == "-C":
			if len(args) < 2 {
				return false, usageError("-C needs a directory")
			}
			line.root = args[1]
			args = args[2:]
		case strings.HasPrefix(a, "-C="):
			line.root = strings.TrimPrefix(a, "-C=")
			args = args[1:]
		default:
			return false, nil
		}
		return true, nil
	}

	for len(args) > 0 {
		took, err := global()
		if err != nil {
			return line, err
		}
		if !took {
			break
		}
	}
	if len(args) == 0 {
		line.help = true
		return line, nil
	}

	word := args[0]
	args = args[1:]
	if len(args) > 0 {
		if cmd := findCommand(word + " " + args[0]); cmd != nil {
			line.cmd, args = cmd, args[1:]
		}
	}
	if line.cmd == nil {
		line.cmd = findCommand(word)
	}
	if line.cmd == nil {
		if members := family(word); len(members) > 0 {
			return line, usageError("orca %s is one of:\n%s", word, reference(members))
		}
		return line, usageError("unknown command %q (try: orca -h)", word)
	}
	cmd := line.cmd
	if cmd.internal {
		line.in.args = args
		return line, nil
	}

	line.in.flags = map[string]string{}
	for _, f := range cmd.flags {
		if f.def != "" {
			line.in.flags[f.name] = f.def
		}
	}
	for len(args) > 0 {
		if took, err := global(); err != nil {
			return line, err
		} else if took {
			continue
		}
		a := args[0]
		args = args[1:]

		// Everything after a bare "--" is an argument, for a search that
		// begins with a dash.
		if a == "--" {
			line.in.args = append(line.in.args, args...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			line.in.args = append(line.in.args, a)
			continue
		}

		name, value, inline := strings.Cut(a, "=")
		f := cmd.flag(name)
		switch {
		case f == nil:
			return line, usageError("orca %s takes no %s\nusage: %s", cmd.name, name, cmd.synopsis())
		case f.value == "" && inline:
			return line, usageError("%s takes no value", name)
		case f.value == "":
			value = "true"
		case !inline:
			if len(args) == 0 {
				return line, usageError("%s needs a %s", name, f.value)
			}
			value, args = args[0], args[1:]
		}
		line.in.flags[f.name] = value
	}

	if n := len(line.in.args); !line.help && (n < cmd.min || (cmd.max != many && n > cmd.max)) {
		return line, usageError("usage: %s", cmd.synopsis())
	}
	return line, nil
}

// synopsis is the command as it is typed, with what it takes.
func (c *command) synopsis() string {
	parts := []string{"orca", c.name}
	if c.args != "" {
		parts = append(parts, c.args)
	}
	for _, f := range c.flags {
		switch {
		case f.value == "":
			parts = append(parts, "["+f.name+"]")
		case f.def != "":
			parts = append(parts, "["+f.name+" "+f.def+"]")
		default:
			parts = append(parts, "["+f.name+" <"+f.value+">]")
		}
	}
	return strings.Join(parts, " ")
}

// referenceColumn is where a command's summary starts, when its synopsis is
// short enough to leave room.
const referenceColumn = 36

// reference lists commands, one to a line with its summary. The README's
// command reference is this, word for word.
func reference(cmds []*command) string {
	var b strings.Builder
	for _, c := range cmds {
		if c.internal {
			continue
		}
		syn := c.synopsis()
		if len(syn) < referenceColumn-1 {
			fmt.Fprintf(&b, "%-*s%s\n", referenceColumn, syn, c.summary)
			continue
		}
		fmt.Fprintf(&b, "%s\n%*s%s\n", syn, referenceColumn, "", c.summary)
	}
	return b.String()
}

// globalFlags is the part of the usage about the flags every command takes.
const globalFlags = `Flags that go anywhere on the line:
  -C <dir>          look for cluster.yaml starting here
  -h                what a command does and takes
`

func allCommands() []*command {
	out := make([]*command, len(commands))
	for i := range commands {
		out[i] = &commands[i]
	}
	return out
}

// usage is what `orca -h` prints.
func usage() string {
	return `orca — deploy things onto bare metal you own

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
` + reference(allCommands()) + "\n" + globalFlags
}

// usage is what `orca <command> -h` prints.
func (c *command) usage() string {
	var b strings.Builder
	fmt.Fprintf(&b, "usage: %s\n\n%s\n", c.synopsis(), sentence(c.summary))
	if c.help != "" {
		fmt.Fprintf(&b, "\n%s\n", c.help)
	}
	if len(c.flags) > 0 {
		b.WriteString("\nFlags:\n")
		for _, f := range c.flags {
			name := f.name
			if f.alias != "" {
				name += ", " + f.alias
			}
			help := f.help
			if f.def != "" {
				help += " (default " + f.def + ")"
			}
			fmt.Fprintf(&b, "  %-18s%s\n", name, help)
		}
	}
	return b.String()
}

// sentence makes a summary, which is written to follow a command's name, read
// as the start of a paragraph.
func sentence(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:] + "."
}
