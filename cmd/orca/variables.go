package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/internal/deploy"
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

// store is the cluster's variable store.
func (c *Cluster) store(ctx context.Context) (varStore, error) {
	api, err := c.client(ctx)
	return varStore{api: api}, err
}

// variablePaths lists the variables under a prefix, by path.
//
// It fails closed. An unreachable Nomad must not read as "nothing is set",
// because the caller that decides whether to generate a database password
// acts on exactly that answer.
func (c *Cluster) variablePaths(ctx context.Context, prefix string) ([]string, error) {
	store, err := c.store(ctx)
	if err != nil {
		return nil, err
	}
	return store.list(ctx, prefix)
}

// SecretPaths lists the variable paths orca owns, as a set.
func (c *Cluster) SecretPaths(ctx context.Context) (map[string]bool, error) {
	paths, err := c.variablePaths(ctx, deploy.SecretPrefix+"/")
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return set, nil
}

// PutSecret writes one secret into Nomad's variable store.
func (c *Cluster) PutSecret(ctx context.Context, group, name, value string) error {
	if err := c.putVariable(ctx, deploy.SecretPath(group, name), map[string]string{deploy.SecretItemKey: value}); err != nil {
		return fmt.Errorf("set secret %s/%s: %w", group, name, err)
	}
	return nil
}

// putVariable writes a variable, replacing any there.
func (c *Cluster) putVariable(ctx context.Context, path string, items map[string]string) error {
	store, err := c.store(ctx)
	if err != nil {
		return err
	}
	return store.put(ctx, path, items)
}

// CreateSecret writes a secret only if it does not exist yet, and reports
// whether it did, which is what a generated password depends on: made once,
// never replaced.
func (c *Cluster) CreateSecret(ctx context.Context, group, name, value string) (bool, error) {
	created, err := c.createVariable(ctx, deploy.SecretPath(group, name), deploy.SecretItemKey, value)
	if err != nil {
		return false, fmt.Errorf("create secret %s/%s: %w", group, name, err)
	}
	return created, nil
}

// createVariable writes a one-item variable only if it does not exist yet, and
// reports whether it did.
func (c *Cluster) createVariable(ctx context.Context, path, key, value string) (bool, error) {
	store, err := c.store(ctx)
	if err != nil {
		return false, err
	}
	return store.putChecked(ctx, nomad.Variable{Path: path, Items: map[string]string{key: value}})
}

// variablePrefixes are everything orca keeps in the store that is a secret of
// some kind: what a manifest references, the registry logins, and the
// dashboard password. The apply lock is the one thing left out.
var variablePrefixes = []string{deploy.SecretPrefix + "/", deploy.RegistryPrefix + "/", deploy.AdminPasswordPath, deploy.CertPrefix + "/"}

// Variables reads every secret the cluster holds, values included. This is
// the only call that pulls plaintext off the machine wholesale, and exporting
// or editing the cluster's secrets is the only reason to make it.
func (c *Cluster) Variables(ctx context.Context) (variables, error) {
	vars, err := c.readVariables(ctx, variablePrefixes)
	if err != nil {
		return nil, fmt.Errorf("read the cluster's secrets: %w", err)
	}
	return vars, nil
}

// Certificates reads every certificate record, without its private key: what
// there is to know about a certificate (who asked, when it expires, why it
// failed) is all in the rest.
func (c *Cluster) Certificates(ctx context.Context) (variables, error) {
	vars, err := c.readVariables(ctx, []string{deploy.CertPrefix + "/"})
	if err != nil {
		return nil, fmt.Errorf("read the cluster's certificates: %w", err)
	}
	for _, items := range vars {
		delete(items, deploy.CertKeyKey)
	}
	return vars, nil
}

func (c *Cluster) readVariables(ctx context.Context, prefixes []string) (variables, error) {
	api, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	return deploy.ReadVariables(ctx, api, prefixes)
}

// PutVariables writes a set of variables, replacing any there.
//
// It stops at the first failure and names it: what was written before it
// stays written, and running the same import again writes the rest.
func (c *Cluster) PutVariables(ctx context.Context, vars variables) error {
	for _, path := range slices.Sorted(maps.Keys(vars)) {
		if err := c.putVariable(ctx, path, vars[path]); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// readVariable returns one item of a variable, and whether the variable
// exists. Missing is an answer, not an error; a Nomad that cannot be asked is.
func (c *Cluster) readVariable(ctx context.Context, path, key string) (string, bool, error) {
	store, err := c.store(ctx)
	if err != nil {
		return "", false, err
	}
	v, ok, err := store.get(ctx, path)
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	return v.Items[key], ok, nil
}

// AdminPassword is the dashboards' generated password, and whether one has
// been generated.
func (c *Cluster) AdminPassword(ctx context.Context) (string, bool, error) {
	v, ok, err := c.readVariable(ctx, deploy.AdminPasswordPath, deploy.AdminPasswordKey)
	if err != nil {
		return "", false, fmt.Errorf("read the dashboard password: %w", err)
	}
	return v, ok, nil
}

// PutAdminPassword replaces the dashboards' password.
func (c *Cluster) PutAdminPassword(ctx context.Context, value string) error {
	if err := c.putVariable(ctx, deploy.AdminPasswordPath, map[string]string{deploy.AdminPasswordKey: value}); err != nil {
		return fmt.Errorf("store the dashboard password: %w", err)
	}
	return nil
}

// CreateAdminPassword stores the dashboards' password unless one is already
// stored, and reports whether it did.
func (c *Cluster) CreateAdminPassword(ctx context.Context, value string) (bool, error) {
	created, err := c.createVariable(ctx, deploy.AdminPasswordPath, deploy.AdminPasswordKey, value)
	if err != nil {
		return false, fmt.Errorf("store the dashboard password: %w", err)
	}
	return created, nil
}

// deleteVariable removes one variable.
func (c *Cluster) deleteVariable(ctx context.Context, path string) error {
	store, err := c.store(ctx)
	if err != nil {
		return err
	}
	return store.delete(ctx, path)
}

// DeleteSecret removes one secret.
func (c *Cluster) DeleteSecret(ctx context.Context, group, name string) error {
	if err := c.deleteVariable(ctx, deploy.SecretPath(group, name)); err != nil {
		return fmt.Errorf("remove secret %s/%s: %w", group, name, err)
	}
	return nil
}

// RegistryHosts lists the registries the cluster holds credentials for.
//
// The host is in the path, so finding out which registries are logged in to
// never pulls a token off the machine.
func (c *Cluster) RegistryHosts(ctx context.Context) (map[string]bool, error) {
	paths, err := c.variablePaths(ctx, deploy.RegistryPrefix+"/")
	if err != nil {
		return nil, fmt.Errorf("list registry credentials: %w", err)
	}
	hosts := map[string]bool{}
	for _, p := range paths {
		if host, ok := deploy.RegistryHost(p); ok {
			hosts[host] = true
		}
	}
	return hosts, nil
}

// The items of a registry's variable. The credential helper on each machine
// reads them by these names.
const (
	registryUsernameKey = "username"
	registryPasswordKey = "password"
)

func registryItems(username, password string) map[string]string {
	return map[string]string{registryUsernameKey: username, registryPasswordKey: password}
}

// PutRegistry stores the credentials for one registry, replacing any there.
func (c *Cluster) PutRegistry(ctx context.Context, host, username, password string) error {
	if err := c.putVariable(ctx, deploy.RegistryPath(host), registryItems(username, password)); err != nil {
		return fmt.Errorf("store credentials for %s: %w", host, err)
	}
	return nil
}

// DeleteRegistry removes one registry's credentials.
func (c *Cluster) DeleteRegistry(ctx context.Context, host string) error {
	if err := c.deleteVariable(ctx, deploy.RegistryPath(host)); err != nil {
		return fmt.Errorf("remove credentials for %s: %w", host, err)
	}
	return nil
}
