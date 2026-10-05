package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// errCanceled is someone answering no.
var errCanceled = errors.New("canceled")

// ask prints a prompt and reads the line typed at it, "" when none could be
// read.
func ask(prompt string) string {
	fmt.Print(prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return ""
	}
	return strings.TrimSpace(line)
}

// saidYes asks a yes/no question, defaulting to no.
func saidYes(prompt string) bool {
	switch strings.ToLower(ask(prompt)) {
	case "y", "yes":
		return true
	}
	return false
}

// confirmAction asks before doing something that keeps data but may not have
// been meant: action reads as what orca is about to do, "stop 3 running
// service(s)". yes is the answer given ahead of time.
//
// Refused rather than assumed when there is nobody to ask. A pipeline that
// meant it says so with --yes; one that did not would otherwise discover it
// had stopped a database.
func confirmAction(action string, yes bool) error {
	if yes {
		return nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("this would %s and there is no terminal to confirm at; re-run with --yes if that is what you meant", action)
	}
	if !saidYes(action + "? [y/N] ") {
		return errCanceled
	}
	return nil
}

// confirmTyped asks for a specific word rather than y/n, before something
// that destroys data. Typing the name of the thing being deleted is hard to
// do by reflex.
func confirmTyped(prompt, want string, yes bool) error {
	if yes {
		return nil
	}
	if ask(fmt.Sprintf("%stype %q to confirm: ", prompt, want)) != want {
		return errCanceled
	}
	return nil
}
