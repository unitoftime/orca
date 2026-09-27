package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ClusterFile is the file that marks the root of an orca directory.
const ClusterFile = "cluster.yaml"

// FindRoot walks up from dir looking for cluster.yaml, the way git looks for
// .git. This is what lets `orca apply` work from anywhere inside the tree
// rather than only from the top of it.
func FindRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}

	for {
		if _, err := os.Stat(filepath.Join(abs, ClusterFile)); err == nil {
			return abs, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("no %s found in %s or any parent directory", ClusterFile, dir)
		}
		abs = parent
	}
}

// GroupName is the name of the group a directory defines: its path relative to
// the root, with separators turned into dashes.
//
// One rule covers every depth you might want. `blog/` is the group "blog";
// `blog/prod/` is "blog-prod". Whether a directory means an app, a project or a
// stage is your decision, expressed by how deep you nest it; orca does not
// need to know which you meant.
func GroupName(root, dir string) (string, error) {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", fmt.Errorf("the root directory is not a group")
	}
	return strings.ReplaceAll(filepath.ToSlash(rel), "/", "-"), nil
}

// Discover finds every group under root.
//
// A directory holding at least one service file (any .yaml but vars.yaml) is
// a group; the directory is the identity, so nothing inside has to repeat it
// and two groups cannot collide because two directories cannot share a path.
// Hidden directories are skipped, so a .git directory full of nothing relevant
// costs nothing.
func Discover(root string) ([]*Manifest, error) {
	var groups []*Manifest
	var errs []error

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if name := d.Name(); path != root && strings.HasPrefix(name, ".") {
			return filepath.SkipDir
		}
		if path == root {
			return nil
		}

		files, err := yamlFiles(path)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		if len(files) == 0 {
			// A directory that only holds other directories is organisation,
			// not a group.
			return nil
		}

		name, err := GroupName(root, path)
		if err != nil {
			errs = append(errs, err)
			return nil
		}

		vars, err := LoadVars(root, path)
		if err != nil {
			errs = append(errs, err)
			return nil
		}

		m, err := loadGroup(name, path, files, vars)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		groups = append(groups, m)
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}

	// A stray manifest beside cluster.yaml is almost certainly a file in the
	// wrong place, and silently ignoring it would deploy nothing while looking
	// like it worked.
	if stray, err := yamlFiles(root); err == nil {
		for _, f := range stray {
			if filepath.Base(f) != ClusterFile {
				errs = append(errs, fmt.Errorf("%s: services must live in a group directory, not beside %s", f, ClusterFile))
			}
		}
	}

	// Nesting joins the path with dashes, so blog/prod/ and blog-prod/ are both
	// the group "blog-prod". Two directories sharing a name would share its
	// jobs, secrets and DNS search domain, and an apply of that name would
	// deploy one directory while stopping the other's services.
	byName := map[string]string{}
	for _, m := range groups {
		if prev, ok := byName[m.App]; ok {
			errs = append(errs, fmt.Errorf("%s and %s are both the group %q; rename one", prev, m.Path, m.App))
			continue
		}
		byName[m.App] = m.Path
	}

	sort.Slice(groups, func(i, j int) bool { return groups[i].App < groups[j].App })
	return groups, errors.Join(errs...)
}

func yamlFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := filepath.Ext(e.Name()); ext != ".yaml" && ext != ".yml" {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") || e.Name() == VarsFile {
			continue
		}
		// Read as a service file it would fail on its first variable with
		// an error about fields; this names the actual mistake.
		if e.Name() == "vars.yml" {
			return nil, fmt.Errorf("%s: variables go in %s", filepath.Join(dir, e.Name()), VarsFile)
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// loadGroup reads every file in a group directory and merges their service
// documents into one group.
func loadGroup(name, dir string, files []string, vars Vars) (*Manifest, error) {
	m := &Manifest{App: name, Path: dir, Vars: Vars{}}
	used := map[string]bool{}
	var errs []error

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		services, err := parseServices(data, f, vars, used)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		m.Services = append(m.Services, services...)
	}
	for name := range used {
		m.Vars[name] = vars[name]
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	m.normalize()
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// ParseServices decodes a file of service documents. A file may hold one
// service or several separated by `---`, so you can split one service per file
// or keep a whole group in one, and change your mind later without orca
// caring.
//
// vars fills in ${var.NAME}; nil means none are in scope.
func ParseServices(data []byte, path string, vars Vars) ([]*Service, error) {
	return parseServices(data, path, vars, map[string]bool{})
}

func parseServices(data []byte, path string, vars Vars, used map[string]bool) ([]*Service, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))

	var out []*Service
	for i := 0; ; i++ {
		// Each document is read as a node first, so variables are filled in
		// before anything is typed. Node.Decode does not carry KnownFields,
		// so the service's own fields are checked by hand; the nested types
		// check theirs.
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err == nil {
			err = expandVars(&doc, vars, used)
		}
		if err == nil {
			err = checkServiceFields(&doc)
		}
		var s Service
		if err == nil {
			err = doc.Decode(&s)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", path, i+1, err)
		}
		// A trailing `---`, or a file that is entirely comments, decodes to
		// nothing. Skipping it beats reporting "name is required" for a
		// document the author did not write.
		if s.isEmpty() {
			continue
		}
		s.srcFile = path
		out = append(out, &s)
	}
	return out, nil
}

// ParseGroup builds a group from a single file's worth of service documents.
//
// Discover is the same operation over a directory; this is the one-file form,
// used where the caller already has the bytes.
func ParseGroup(name string, data []byte, path string) (*Manifest, error) {
	services, err := ParseServices(data, path, nil)
	if err != nil {
		return nil, err
	}

	m := &Manifest{App: name, Path: path, Services: services}
	m.normalize()
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}
