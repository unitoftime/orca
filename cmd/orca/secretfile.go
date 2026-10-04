package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/unitoftime/orca/pkg/deploy"
	"golang.org/x/term"
)

// The commands that move a cluster's secrets in and out of a file: `orca
// secret export`, `import` and `edit`. Each is a composition of the same four
// things: read the cluster, read a file, put a bundle in front of an editor,
// and apply a bundle to the cluster as a diff.

// secretExport writes every secret the cluster holds to a file, or to stdout
// when none is named. Encrypted to a passphrase unless plain.
func secretExport(ctx context.Context, cluster *Cluster, file string, plain bool) error {
	vars, err := cluster.Variables(ctx)
	if err != nil {
		return err
	}
	passphrase := ""
	if !plain {
		if passphrase, err = newPassphrase(); err != nil {
			return err
		}
	}
	doc, err := encodeBundle(bundleOf(vars), passphrase)
	if err != nil {
		return err
	}
	if file == "" {
		_, err = os.Stdout.Write(doc)
		return err
	}
	if err := writeFileAtomic(file, doc); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "exported %d to %s\n", len(vars), file)
	return nil
}

// secretImport applies a file's secrets to the cluster.
func secretImport(ctx context.Context, cfg Config, cluster *Cluster, file string, yes, force bool) error {
	want, _, err := readBundleFile(file)
	if err != nil {
		return err
	}
	have, err := cluster.Variables(ctx)
	if err != nil {
		return err
	}
	return applyBundle(ctx, cfg, cluster, want, have, yes, force)
}

// secretEditCluster opens the cluster's own secrets in an editor and applies
// what changed.
func secretEditCluster(ctx context.Context, cfg Config, cluster *Cluster, force bool) error {
	if err := needTerminal(); err != nil {
		return err
	}
	have, err := cluster.Variables(ctx)
	if err != nil {
		return err
	}
	want, err := editBundle(bundleOf(have))
	if err != nil {
		return err
	}
	return applyBundle(ctx, cfg, cluster, want, have, false, force)
}

// secretEditFile opens a file's secrets in an editor and writes it back the
// way it was found: encrypted to the same passphrase, or in plaintext. A file
// that does not exist yet is started empty. No cluster is involved; `orca
// secret import` is what applies the result.
func secretEditFile(file string, plain bool) error {
	if err := needTerminal(); err != nil {
		return err
	}
	before, passphrase, err := readBundleFile(file)
	switch {
	case errors.Is(err, fs.ErrNotExist) && plain:
	case errors.Is(err, fs.ErrNotExist):
		// Asked for before the edit rather than after, so a mistyped
		// confirmation costs nothing.
		if passphrase, err = newPassphrase(); err != nil {
			return err
		}
	case err != nil:
		return err
	}

	after, err := editBundle(before)
	if err != nil {
		return err
	}
	if reflect.DeepEqual(before.variables(), after.variables()) {
		fmt.Fprintln(os.Stderr, "no changes")
		return nil
	}
	doc, err := encodeBundle(after, passphrase)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(file, doc); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s; `orca secret import %s` applies it to the cluster\n", file, file)
	return nil
}

// applyBundle makes the cluster hold what a bundle holds, writing only what
// differs: a written secret restarts the services using it, so writing one
// that has not changed would restart them for nothing.
//
// have is the cluster as it was read. Nothing is ever removed; see bundleDiff.
func applyBundle(ctx context.Context, cfg Config, cluster *Cluster, want Bundle, have variables, yes, force bool) error {
	wantVars := want.variables()
	diff := diffVariables(wantVars, have)

	for _, p := range diff.Add {
		fmt.Printf("  add     %s\n", variableLabel(p))
	}
	for _, p := range diff.Change {
		fmt.Printf("  change  %s\n", variableLabel(p))
	}
	if diff.Same > 0 {
		fmt.Printf("  %d unchanged\n", diff.Same)
	}
	if len(diff.Unlisted) > 0 {
		fmt.Println("on the cluster and not listed here, so left alone (`orca secret rm` removes a secret):")
		for _, p := range diff.Unlisted {
			fmt.Printf("  %s\n", variableLabel(p))
		}
	}

	changed := slices.Concat(diff.Add, diff.Change)
	if len(changed) == 0 {
		fmt.Println("nothing to change")
		return nil
	}

	// Only a change is guarded. Adding a generated secret is how a rebuilt
	// cluster gets back the password its restored data was initialised with.
	var changedSecrets []secretRef
	for _, p := range diff.Change {
		if ref, ok := secretRefOf(p); ok {
			changedSecrets = append(changedSecrets, ref)
		}
	}
	if err := guardGenerated(cfg, changedSecrets, "change it", force); err != nil {
		return err
	}

	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("this would write %d to the cluster and there is no terminal to confirm at; "+
				"re-run with --yes if that is what you meant", len(changed))
		}
		if !confirmYes(fmt.Sprintf("write %d to the cluster? [y/N] ", len(changed))) {
			return fmt.Errorf("cancelled")
		}
	}

	write := variables{}
	for _, p := range changed {
		write[p] = wantVars[p]
	}
	if err := cluster.PutVariables(ctx, write); err != nil {
		return err
	}
	fmt.Printf("wrote %d; services using a changed secret restart with the new value\n", len(changed))
	if _, ok := write[deploy.AdminPasswordPath]; ok {
		fmt.Println("the dashboards use the new password from the next `orca apply`")
	}
	return nil
}

// encodeBundle is a bundle as a file's contents: its document, encrypted when
// there is a passphrase.
func encodeBundle(b Bundle, passphrase string) ([]byte, error) {
	doc, err := marshalBundle(b)
	if err != nil || passphrase == "" {
		return doc, err
	}
	return seal(doc, passphrase)
}

// readBundleFile reads a bundle, asking for the passphrase if the file is
// encrypted. The passphrase is returned so an edit can write the file back
// under it; it is empty for a plaintext file.
func readBundleFile(file string) (Bundle, string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return Bundle{}, "", err
	}
	passphrase := ""
	if sealed(data) {
		if passphrase, err = readPassphrase("passphrase for " + file); err != nil {
			return Bundle{}, "", err
		}
		if data, err = unseal(data, passphrase); err != nil {
			return Bundle{}, "", fmt.Errorf("%s: %w", file, err)
		}
	}
	b, err := parseBundle(data)
	if err != nil {
		return Bundle{}, "", fmt.Errorf("%s: %w", file, err)
	}
	return b, passphrase, nil
}

// editBundle puts a bundle in front of an editor and returns what was saved.
// An edit that does not load is offered again rather than thrown away, since
// by then it holds values typed by hand.
//
// The plaintext sits in a file only its owner can read for as long as the
// editor is open, in memory rather than on disk where the system offers that,
// and is removed afterwards.
func editBundle(b Bundle) (Bundle, error) {
	doc, err := marshalBundle(b)
	if err != nil {
		return Bundle{}, err
	}
	dir, err := os.MkdirTemp(os.Getenv("XDG_RUNTIME_DIR"), "orca-secrets-*")
	if err != nil {
		return Bundle{}, err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "secrets.yaml")
	if err := os.WriteFile(file, doc, 0o600); err != nil {
		return Bundle{}, err
	}

	for {
		if err := runEditor(file); err != nil {
			return Bundle{}, err
		}
		edited, err := os.ReadFile(file)
		if err != nil {
			return Bundle{}, err
		}
		after, err := parseBundle(edited)
		if err == nil {
			return after, nil
		}
		fmt.Fprintf(os.Stderr, "the edit does not load:\n%v\n", err)
		if !confirmYes("edit it again? [y/N] ") {
			return Bundle{}, fmt.Errorf("cancelled; nothing changed")
		}
	}
}

// runEditor opens a file in $VISUAL or $EDITOR and waits for it to exit. The
// variable is a command line and not a path ("code -w"), so a shell runs it.
//
// Not tied to orca's context: Ctrl-C is a key an editor handles itself, and
// killing the editor on it would lose the edit.
func runEditor(file string) error {
	editor := cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"), "vi")
	c := exec.Command("sh", "-c", editor+` "$1"`, "sh", file)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("editor %q: %w", editor, err)
	}
	return nil
}

func needTerminal() error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("orca secret edit opens an editor, and there is no terminal to open it at")
	}
	return nil
}

// readPassphrase reads a passphrase the way a secret is read: prompted
// without echo, or piped, never an argument.
func readPassphrase(prompt string) (string, error) {
	passphrase, err := readSecretValue(prompt)
	if err != nil {
		return "", err
	}
	if passphrase == "" {
		return "", fmt.Errorf("refusing an empty passphrase")
	}
	return passphrase, nil
}

// newPassphrase reads a passphrase to encrypt under. At a terminal it is asked
// for twice: a typo in the only passphrase a file was ever given is a file
// nobody can open.
func newPassphrase() (string, error) {
	passphrase, err := readPassphrase("passphrase")
	if err != nil || !term.IsTerminal(int(os.Stdin.Fd())) {
		return passphrase, err
	}
	again, err := readPassphrase("passphrase again")
	if err != nil {
		return "", err
	}
	if again != passphrase {
		return "", fmt.Errorf("the passphrases differ; nothing written")
	}
	return passphrase, nil
}

// writeFileAtomic replaces a file with data, readable only by its owner. It
// is written beside the file and renamed into place, so a failure part way
// leaves the old contents rather than half of the new.
func writeFileAtomic(file string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), ".orca-secrets-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}
