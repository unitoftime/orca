package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	nomad "github.com/hashicorp/nomad/api"
)

// Hash is the content hash of a job spec, stamped into the job's own meta and
// compared against the deployed job's to decide whether anything changed.
//
// This is what makes a no-op apply quiet and fast, the requirement that lets
// `orca apply` run from CI on every commit. Nomad would also treat an identical
// submission as a no-op, but only after a round trip per job; comparing hashes
// answers the question for every app in one read.
//
// The hash covers the whole rendered job, so anything that would alter what
// runs (a resolved image digest, a resource change, a new env var, a template
// edit) changes it, with no separate list of significant fields to keep in
// sync.
func Hash(job *nomad.Job) string {
	// The hash field itself is excluded, or stamping it would change what it is
	// the hash of.
	var saved string
	if job.Meta != nil {
		saved = job.Meta[MetaHash]
		delete(job.Meta, MetaHash)
	}

	// encoding/json sorts map keys, so this is stable across runs.
	data, err := json.Marshal(job)
	if err != nil {
		// A Nomad job is plain data; failing to marshal one means a
		// programming error, not a condition a caller can handle.
		panic("deploy: marshal job for hashing: " + err.Error())
	}

	if saved != "" {
		job.Meta[MetaHash] = saved
	}

	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// PinImages rewrites every task image in the jobs to the digest resolve
// returns for it, and re-stamps the hash of any job it changed.
//
// One rule pins every image orca submits: a service's own image, the
// platform's, the init task a template adds and the backup's S3 client. Any
// image left as a tag lets a tag moving upstream change what runs without
// apply noticing, and two machines could run two different builds of "the
// same" component. resolve is injected so this stays offline.
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
		changed := false
		for _, g := range j.TaskGroups {
			for _, t := range g.Tasks {
				ref, _ := t.Config["image"].(string)
				p, err := pin(ref)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: task %s: %w", *j.ID, t.Name, err))
					continue
				}
				if p != ref {
					t.Config["image"] = p
					changed = true
				}
			}
		}
		if !changed {
			continue
		}
		if img := j.Meta[MetaImage]; img != "" {
			if p, err := pin(img); err == nil {
				j.Meta[MetaImage] = p
			}
		}
		j.Meta[MetaHash] = Hash(j)
	}
	return errors.Join(errs...)
}
