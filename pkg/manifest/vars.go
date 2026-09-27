package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// VarsFile holds a directory's variables. It is the one .yaml file in a group
// directory that is not a service file.
//
// Variables exist so that a value repeated across a group's files is written
// once — the commit three services are built from, above all, so moving them
// to the next build is one edit rather than one per file:
//
//	# blog/prod/vars.yaml
//	commit: 4f2a9c1
//
//	# blog/prod/server.yaml
//	image: ghcr.io/you/blog-server:${var.commit}
//
// A vars.yaml applies to its own directory and every directory below it, and
// the nearest one wins, so values shared by every group sit at the root beside
// cluster.yaml and a group overrides what it needs to.
const VarsFile = "vars.yaml"

// Vars are the variables in scope for a group, by name.
type Vars map[string]string

// varName is what a variable may be called: the same shape as a secret name,
// so ${var.NAME} and ${secret.NAME} read alike.
var varName = secretName

// serviceLikeKeys are fields no variable file has any reason to hold. A
// vars.yaml carrying one is a service file that happens to have the reserved
// name, and reading it as variables would silently drop the service — which the
// next apply would then stop.
var serviceLikeKeys = []string{"image", "template", "target"}

// ParseVars reads one vars.yaml: a flat mapping of names to single values.
//
// Deliberately flat and deliberately scalar. A variable stands in for one
// value; anything that would need a list or a block is a structural
// difference, and those are written out, not templated.
func ParseVars(data []byte, path string) (Vars, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return Vars{}, nil
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: holds more than one document; variables are one mapping of name: value", path)
	}
	if len(doc.Content) == 0 {
		return Vars{}, nil
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return Vars{}, nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: must be a mapping of name: value", path)
	}

	for i := 0; i+1 < len(root.Content); i += 2 {
		for _, k := range serviceLikeKeys {
			if root.Content[i].Value == k {
				return nil, fmt.Errorf("%s: has %q, so it looks like a service; %s is reserved for variables, so rename the file",
					path, k, VarsFile)
			}
		}
	}

	out := Vars{}
	seen := map[string]bool{}
	var errs []error
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, val := root.Content[i], root.Content[i+1]
		name := key.Value
		dup := seen[name]
		seen[name] = true
		switch {
		case !varName.MatchString(name):
			errs = append(errs, fmt.Errorf("%s: line %d: %q is not a variable name (letters, digits, _ and -)", path, key.Line, name))
		case dup:
			errs = append(errs, fmt.Errorf("%s: line %d: %s is defined twice", path, key.Line, name))
		case val.Kind != yaml.ScalarNode || val.Tag == "!!null":
			errs = append(errs, fmt.Errorf("%s: line %d: %s must be a single value, not a list, a mapping or nothing", path, key.Line, name))
		case strings.Contains(val.Value, "$"):
			// A value is spliced in before env values are read, so a $ in it
			// would be read again: ${secret.x} would become a secret reference
			// that no one wrote as one.
			errs = append(errs, fmt.Errorf("%s: line %d: %s may not contain $", path, key.Line, name))
		default:
			out[name] = val.Value
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadVars collects the variables in scope for dir: every vars.yaml from root
// down to dir, the nearer overriding the farther.
func LoadVars(root, dir string) (Vars, error) {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return nil, err
	}

	dirs := []string{root}
	if rel != "." {
		cur := root
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			dirs = append(dirs, cur)
		}
	}

	out := Vars{}
	for _, d := range dirs {
		path := filepath.Join(d, VarsFile)
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		vars, err := ParseVars(data, path)
		if err != nil {
			return nil, err
		}
		for k, v := range vars {
			out[k] = v
		}
	}
	return out, nil
}

// expandVars fills in every ${var.NAME} in a service document's values, and
// reports which variables it used.
//
// It works on the parsed document rather than the text, which is what keeps
// variables to values: a key is never expanded (and a reference in one is an
// error), a comment is never touched, and a value can hold any characters
// without having to be escaped for wherever in the YAML it lands. Line numbers
// are the file's own, so an error after expansion still points at the line
// you wrote.
func expandVars(n *yaml.Node, vars Vars, used map[string]bool) error {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if err := expandVars(c, vars, used); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if k := n.Content[i]; strings.Contains(k.Value, "${var.") {
				return fmt.Errorf("line %d: %s is a key; variables go in values", k.Line, k.Value)
			}
			if err := expandVars(n.Content[i+1], vars, used); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		out, changed, err := expandString(n.Value, vars, used)
		if err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		if changed {
			n.Value = out
			// An unquoted value is typed by what it holds, so
			// `replicas: ${var.n}` is a number once n is filled in. A quoted
			// one stays a string, as it would have if written out.
			if n.Style == 0 {
				n.Tag = ""
			}
		}
	}
	return nil
}

// expandString fills in the ${var.NAME} references in one value.
//
// $$ is passed through untouched, so $${var.x} is not a reference — the same
// escape env values use, and env values are read after this, where $$ becomes
// $. Every other ${...} is left alone: ${secret.NAME} is resolved on the
// machine, and anything else is for whatever reads that value to accept or
// refuse.
func expandString(s string, vars Vars, used map[string]bool) (string, bool, error) {
	if !strings.Contains(s, "${var") {
		return s, false, nil
	}

	var b strings.Builder
	changed := false
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "$$"):
			b.WriteString("$$")
			i += 2
		case strings.HasPrefix(s[i:], "${vars."):
			return "", false, fmt.Errorf("%s: a variable is ${var.NAME}", s[i:])
		case strings.HasPrefix(s[i:], "${var."):
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return "", false, fmt.Errorf("%s is not closed", s[i:])
			}
			name := s[i+len("${var.") : i+end]
			if !varName.MatchString(name) {
				return "", false, fmt.Errorf("${var.%s}: %q is not a variable name", name, name)
			}
			v, ok := vars[name]
			if !ok {
				return "", false, undefinedVar(name, vars)
			}
			b.WriteString(v)
			used[name] = true
			changed = true
			i += end + 1
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String(), changed, nil
}

func undefinedVar(name string, vars Vars) error {
	if len(vars) == 0 {
		return fmt.Errorf("${var.%s} is not defined, and no %s is in scope; add one to this directory or a parent", name, VarsFile)
	}
	names := make([]string, 0, len(vars))
	for n := range vars {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Errorf("${var.%s} is not defined; defined here: %s", name, strings.Join(names, ", "))
}

// serviceFields are the keys a service document may have.
//
// Checked by hand because a service is decoded from a yaml.Node — which is
// what lets variables be filled in first — and Node.Decode does not carry the
// decoder's KnownFields setting. The nested types already check their own
// fields for the same reason; this is the one level that relied on the
// decoder.
var serviceFields = func() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(Service{})
	for i := 0; i < t.NumField(); i++ {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if tag != "" && tag != "-" {
			out[tag] = true
		}
	}
	return out
}()

// checkServiceFields refuses a key a service does not have, worded as the
// decoder words it so a typo reads the same whether or not the file uses
// variables.
func checkServiceFields(doc *yaml.Node) error {
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	m := doc.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i]; !serviceFields[k.Value] {
			return fmt.Errorf("line %d: field %s not found in type manifest.Service", k.Line, k.Value)
		}
	}
	return nil
}
