package main

import (
	"context"
	"errors"

	nomad "github.com/hashicorp/nomad/api"
)

// varStore is Nomad's variable store, where everything orca keeps on a
// cluster lives: secrets, registry logins, certificates, the apply lock. The
// CLI reaches it over SSH and the certificate job from on the machine, and
// both read and write it through this.
//
// Not found and a lost check-and-set are answers here, not errors: each is
// something a caller decides what to do about.
type varStore struct {
	api *nomad.Client
}

// list returns the paths under a prefix.
//
// Paths only: the listing returns no item values, so finding out what is set
// never pulls a single plaintext value off the machine. That is the reason a
// secret is one variable rather than one key inside a per-group variable.
func (s varStore) list(ctx context.Context, prefix string) ([]string, error) {
	metas, _, err := s.api.Variables().List((&nomad.QueryOptions{Prefix: prefix}).WithContext(ctx))
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(metas))
	for i, m := range metas {
		paths[i] = m.Path
	}
	return paths, nil
}

// get reads one variable, and whether it exists.
func (s varStore) get(ctx context.Context, path string) (nomad.Variable, bool, error) {
	v, _, err := s.api.Variables().Read(path, (&nomad.QueryOptions{}).WithContext(ctx))
	if errors.Is(err, nomad.ErrVariablePathNotFound) {
		return nomad.Variable{}, false, nil
	}
	if err != nil {
		return nomad.Variable{}, false, err
	}
	return *v, true, nil
}

// put writes a variable, replacing any there.
func (s varStore) put(ctx context.Context, path string, items map[string]string) error {
	_, _, err := s.api.Variables().Update(&nomad.Variable{Path: path, Items: items}, (&nomad.WriteOptions{}).WithContext(ctx))
	return err
}

// putChecked writes a variable only if it is unchanged since it was read (or,
// for one never read, only if it does not exist), and reports whether it
// wrote. The store enforces it, so "made once, never replaced" does not rest
// on a read made a moment earlier.
func (s varStore) putChecked(ctx context.Context, v nomad.Variable) (bool, error) {
	_, _, err := s.api.Variables().CheckedUpdate(&v, (&nomad.WriteOptions{}).WithContext(ctx))
	if errors.As(err, &nomad.ErrCASConflict{}) {
		return false, nil
	}
	return err == nil, err
}

// delete removes one variable. One that is already gone is not an error:
// Nomad answers a delete of nothing with success.
func (s varStore) delete(ctx context.Context, path string) error {
	_, err := s.api.Variables().Delete(path, (&nomad.WriteOptions{}).WithContext(ctx))
	return err
}
