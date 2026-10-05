package manifest

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Volume is a service's persistent disk. It is authored either as a bare size,
// when a template already knows where to mount it:
//
//	volume: 20G
//
// or as a size and a mount point, for a service orca knows nothing about:
//
//	volume:
//	  size: 20G
//	  mount: /data
//
// A volume is node-pinned: the service can only be scheduled where its data is.
// That is invisible on one machine and load-bearing on three.
type Volume struct {
	Size  Size   `yaml:"size"`
	Mount string `yaml:"mount,omitempty"`
}

func (v *Volume) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var size Size
		if err := size.UnmarshalYAML(node); err != nil {
			return fmt.Errorf("volume: %w", err)
		}
		v.Size = size
		return nil
	}

	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("volume must be a size like 20G, or a mapping with size and mount")
	}

	// node.Decode does not inherit the parent decoder's KnownFields setting, so
	// strictness has to be enforced by hand here or a typo inside `volume:`
	// silently takes a default, which is exactly what strict decoding exists
	// to prevent.
	if err := checkKnownFields(node, "volume", "size", "mount"); err != nil {
		return err
	}

	type plain Volume // avoid recursing into this method
	var out plain
	if err := node.Decode(&out); err != nil {
		return err
	}
	*v = Volume(out)
	return nil
}

func (v Volume) MarshalYAML() (any, error) {
	if v.Mount == "" {
		return v.Size.String(), nil
	}
	return map[string]string{"size": v.Size.String(), "mount": v.Mount}, nil
}

// checkKnownFields rejects mapping keys outside the allowed set, reproducing
// the strict decoding that yaml.Node.Decode drops.
func checkKnownFields(node *yaml.Node, what string, allowed ...string) error {
	ok := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		ok[a] = true
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !ok[key] {
			return fmt.Errorf("%s: unknown field %q (known fields: %s)", what, key, strings.Join(allowed, ", "))
		}
	}
	return nil
}
