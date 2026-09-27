package manifest

import (
	"fmt"
	"strings"
)

// EnvPart is one piece of an env value: literal text, or a reference to a
// secret whose value is spliced in on the machine.
type EnvPart struct {
	// Secret is the referenced secret's name; empty for literal text.
	Secret string

	// Literal is the text itself, with $$ already turned into $.
	Literal string
}

// ParseEnvValue splits an env value into literal text and ${secret.NAME}
// references.
//
// It is the one reader of this syntax. There were two — a regular expression
// here that validated, and a substring search in the job builder that
// rendered — and they disagreed about $$: "$${secret.x}" validated as a
// literal and was rendered as a reference, so the task blocked forever on a
// secret the preflight never asked for.
//
// $$ is a literal $. Any other ${...} is an error rather than literal text, so
// a near miss like ${secrets.token} is reported instead of being shipped to
// the container as that string. ${var.NAME} never reaches here: variables are
// filled in when the file is read, before any env value is.
func ParseEnvValue(v string) ([]EnvPart, error) {
	var parts []EnvPart
	var lit strings.Builder

	flush := func() {
		if lit.Len() > 0 {
			parts = append(parts, EnvPart{Literal: lit.String()})
			lit.Reset()
		}
	}

	for i := 0; i < len(v); {
		switch {
		case strings.HasPrefix(v[i:], "$$"):
			lit.WriteByte('$')
			i += 2
		case strings.HasPrefix(v[i:], "${"):
			end := strings.IndexByte(v[i:], '}')
			if end < 0 {
				return nil, fmt.Errorf("%s is not closed; write ${secret.NAME} or ${var.NAME}, or $${ for a literal", v[i:])
			}
			ref := v[i : i+end+1]
			name, ok := strings.CutPrefix(ref[2:len(ref)-1], "secret.")
			if !ok || !secretName.MatchString(name) {
				return nil, fmt.Errorf("%s is not a secret reference; write ${secret.NAME} or ${var.NAME}, or $${ for a literal", ref)
			}
			flush()
			parts = append(parts, EnvPart{Secret: name})
			i += end + 1
		default:
			lit.WriteByte(v[i])
			i++
		}
	}
	flush()
	return parts, nil
}

// HasSecret reports whether any part is a secret reference.
func HasSecret(parts []EnvPart) bool {
	for _, p := range parts {
		if p.Secret != "" {
			return true
		}
	}
	return false
}

// LiteralValue joins parts that hold no secret back into one string.
func LiteralValue(parts []EnvPart) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Literal)
	}
	return b.String()
}

// secretRefs lists the secret names an env value references. A value that
// does not parse references nothing; validation reports it.
func secretRefs(v string) []string {
	parts, err := ParseEnvValue(v)
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range parts {
		if p.Secret != "" {
			out = append(out, p.Secret)
		}
	}
	return out
}
