// Command pinimages resolves every image in internal/images to the digest its tag
// points at now, and rewrites the file with them. `make pin-images` runs it.
package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/unitoftime/orca/internal/registry"
)

const path = "internal/images/images.go"

// imageConst is one image constant: `Name = "repo:tag"`, with or without a
// digest after the tag.
var imageConst = regexp.MustCompile(`(?m)^(\s*\w+\s*=\s*")([^"@]+)(@sha256:[0-9a-f]{64})?(")`)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pin-images:", err)
		os.Exit(1)
	}
}

func run() error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var errs []string
	out := imageConst.ReplaceAllStringFunc(string(src), func(line string) string {
		m := imageConst.FindStringSubmatch(line)
		tag, old := m[2], strings.TrimPrefix(m[3], "@")
		p, err := registry.Remote{}.Resolve(ctx, tag)
		if err != nil {
			errs = append(errs, err.Error())
			return line
		}
		_, digest, _ := strings.Cut(p.Ref, "@")
		if digest != old {
			fmt.Printf("%s: %s\n", tag, digest)
		}
		return m[1] + tag + "@" + digest + m[4]
	})
	if len(errs) > 0 {
		return fmt.Errorf("nothing written:\n  %s", strings.Join(errs, "\n  "))
	}
	return os.WriteFile(path, []byte(out), 0o644)
}
