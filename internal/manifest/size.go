package manifest

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Size is a byte quantity written with an explicit unit: "512M", "20G".
//
// A bare number is rejected rather than guessed at: memory in MB and a volume
// in GB would otherwise be one field apart. Requiring the unit means a value
// means the same thing everywhere it appears.
//
// Units are binary (1K = 1024), matching how every tool that will consume these
// numbers reports them back to you.
type Size int64

const (
	Kilobyte Size = 1 << 10
	Megabyte Size = 1 << 20
	Gigabyte Size = 1 << 30
	Terabyte Size = 1 << 40
)

var sizeUnits = []struct {
	suffix string
	scale  Size
}{
	{"T", Terabyte},
	{"G", Gigabyte},
	{"M", Megabyte},
	{"K", Kilobyte},
}

// ParseSize reads a size with a mandatory unit suffix. "10GB" and "10GiB" are
// accepted as spellings of "10G"; everything is binary regardless.
func ParseSize(s string) (Size, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, fmt.Errorf("size is empty; use a unit, e.g. 512M or 20G")
	}

	// Tolerate the common spellings of the same thing before matching the unit.
	norm := strings.ToUpper(raw)
	norm = strings.TrimSuffix(norm, "IB")
	norm = strings.TrimSuffix(norm, "B")

	for _, u := range sizeUnits {
		numPart, ok := strings.CutSuffix(norm, u.suffix)
		if !ok {
			continue
		}
		numPart = strings.TrimSpace(numPart)
		if numPart == "" {
			return 0, fmt.Errorf("size %q has a unit but no number", raw)
		}
		n, err := strconv.ParseFloat(numPart, 64)
		if err != nil {
			return 0, fmt.Errorf("size %q: %q is not a number", raw, numPart)
		}
		if n < 0 {
			return 0, fmt.Errorf("size %q is negative", raw)
		}
		return Size(n * float64(u.scale)), nil
	}

	// A bare number is the likely mistake, so name the fix instead of just
	// reporting a parse failure.
	if _, err := strconv.ParseFloat(norm, 64); err == nil {
		return 0, fmt.Errorf("size %q has no unit; write %sM for megabytes or %sG for gigabytes", raw, norm, norm)
	}
	return 0, fmt.Errorf("size %q is not a size; use a number and a unit, e.g. 512M or 20G", raw)
}

// Megabytes returns the size in whole MB, rounded down. Nomad sizes memory and
// disk in MB, so this is the conversion every jobspec needs.
func (s Size) Megabytes() int { return int(s / Megabyte) }

// String renders the size in the largest unit that divides it exactly, so a
// value read from a manifest round-trips to the same text it was written as.
func (s Size) String() string {
	if s == 0 {
		return "0"
	}
	for _, u := range sizeUnits {
		if s%u.scale == 0 {
			return strconv.FormatInt(int64(s/u.scale), 10) + u.suffix
		}
	}
	return strconv.FormatInt(int64(s), 10)
}

func (s *Size) UnmarshalYAML(node *yaml.Node) error {
	// Decoding into a string first means an unquoted 512M (a YAML string) and a
	// quoted "512M" behave identically, while a bare 512 arrives as "512" and
	// gets the explanatory error from ParseSize rather than a type error.
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("size must be a value with a unit, e.g. 512M or 20G")
	}
	v, err := ParseSize(raw)
	if err != nil {
		return err
	}
	*s = v
	return nil
}
