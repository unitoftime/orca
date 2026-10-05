package main

import (
	"os"
	"strings"
	"testing"
)

// invoke reads a command line the way main does, for a test that wants what a
// command would be handed.
func invoke(t *testing.T, args ...string) invocation {
	t.Helper()
	line, err := parseCommandLine(args)
	if err != nil {
		t.Fatalf("orca %s: %v", strings.Join(args, " "), err)
	}
	return line.in
}

// The README's command reference is written from the same table the command
// line is read from. It was kept by hand once, and said things orca did not
// do.
func TestREADMECommandReference(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	want := "```\n" + reference(allCommands()) + "\n" + globalFlags + "```\n"
	if !strings.Contains(string(readme), want) {
		t.Errorf("README.md's command reference is out of date. It should be:\n\n%s", want)
	}
}

// A flag a command does not take is refused, not read as an argument: `orca
// status --json` would otherwise look for a group by that name.
func TestUnknownFlagsAreRefused(t *testing.T) {
	for _, args := range [][]string{
		{"status", "--json"},
		{"plan", "--yes"},
		{"secret", "list", "--force"},
		{"top", "extra"},
		{"stop"},
		{"secret"},
		{"nope"},
		{"logs", "--since"},
		{"top", "--json=yes"},
	} {
		if _, err := parseCommandLine(args); err == nil {
			t.Errorf("orca %s should be refused", strings.Join(args, " "))
		}
	}
}

func TestFlagsAndArgumentsInAnyOrder(t *testing.T) {
	line, err := parseCommandLine([]string{"apply", "shop", "-y", "-C", "infra", "blog"})
	if err != nil {
		t.Fatal(err)
	}
	if line.cmd.name != "apply" || line.root != "infra" || !line.in.Has(flagYes) || strings.Join(line.in.args, " ") != "shop blog" {
		t.Errorf("got %+v", line)
	}

	// A family's command is two words, and one that is also a command on its
	// own is told apart by what follows it.
	for args, want := range map[string]string{"password": "password", "password set": "password set", "db restore shop/db": "db restore"} {
		line, err := parseCommandLine(strings.Fields(args))
		if err != nil || line.cmd.name != want {
			t.Errorf("orca %s: got %v, %v", args, line.cmd, err)
		}
	}

	// What a job runs on a machine reads its own arguments.
	line, err = parseCommandLine([]string{"serve-status", "--listen=:1", "-h"})
	if err != nil || line.help || len(line.in.args) != 2 {
		t.Errorf("serve-status: %+v, %v", line, err)
	}
}
