package deploy

import (
	"errors"
	"fmt"
	"strings"

	nomad "github.com/hashicorp/nomad/api"
)

// PinImages rewrites every task image in the jobs to the digest resolve
// returns for it.
//
// One rule pins every image orca submits: a service's own image, the
// platform's, the init task a template adds and the backup's S3 client. Any
// image left as a tag lets a tag moving upstream change what runs without
// apply noticing, and two machines could run two different builds of "the
// same" component. An image already naming a digest, which is every one orca
// chooses itself, is left as it is. resolve is injected so this stays offline.
func PinImages(jobs []*nomad.Job, resolve func(ref string) (string, error)) error {
	cache := map[string]string{}
	pin := func(ref string) (string, error) {
		if ref == "" || strings.Contains(ref, "@sha256:") {
			return ref, nil
		}
		if p, ok := cache[ref]; ok {
			return p, nil
		}
		p, err := resolve(ref)
		if err != nil {
			return "", err
		}
		cache[ref] = p
		return p, nil
	}

	var errs []error
	for _, j := range jobs {
		for _, g := range j.TaskGroups {
			for _, t := range g.Tasks {
				ref, _ := t.Config["image"].(string)
				p, err := pin(ref)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: task %s: %w", *j.ID, t.Name, err))
					continue
				}
				t.Config["image"] = p
			}
		}
		if img := j.Meta[MetaImage]; img != "" {
			if p, err := pin(img); err == nil {
				j.Meta[MetaImage] = p
			}
		}
	}
	return errors.Join(errs...)
}
