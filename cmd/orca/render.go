package main

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed templates
var templates embed.FS

// renderTemplate reads an embedded template and replaces all {{KEY}}
// placeholders with values from vars. Returns the rendered string.
func renderTemplate(name string, vars map[string]string) (string, error) {
	data, err := templates.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("read template %s: %w", name, err)
	}

	result := string(data)
	for k, v := range vars {
		result = strings.ReplaceAll(result, "{{"+k+"}}", v)
	}
	return result, nil
}
