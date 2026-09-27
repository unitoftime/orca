// Package registry resolves a mutable image tag to the immutable digest it
// currently points at.
package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Resolver turns an image reference into a digest-pinned one.
//
// This exists because a tag is a moving target. Nomad will not restart a job
// whose spec has not changed, so deploying `myapp:latest` a second time is
// silently a no-op even though latest has moved — the bug everyone hits when
// they deploy by mutable tag. Pinning the digest at apply time makes the job
// spec change exactly when the image really changed, and makes every deploy
// reproducible and auditable after the fact.
type Resolver interface {
	// Resolve returns ref rewritten as repo@sha256:..., or ref unchanged if it
	// already names a digest.
	Resolve(ctx context.Context, ref string) (Pinned, error)
}

// Pinned is an image reference resolved to a digest.
type Pinned struct {
	// Ref is repo@sha256:..., which is what gets submitted.
	Ref string

	// Registry is the host the image is pulled from, as Host names it.
	Registry string

	// Private reports that the registry would not serve the image
	// anonymously, and your own credentials were needed to resolve it. The
	// machine that pulls it needs credentials too, which the resolving side
	// is the only one in a position to notice before anything is submitted.
	//
	// Always false for a reference that already names a digest: it is never
	// looked up, so there is nothing to learn it from.
	Private bool
}

// IsDigest reports whether a reference already pins a digest.
func IsDigest(ref string) bool { return strings.Contains(ref, "@sha256:") }

// Remote resolves against the real registry.
//
// Anonymously first, and with the credentials docker uses
// (~/.docker/config.json and its credential helpers) only when the registry
// refuses. The fallback is what resolves a private image with no
// orca-specific configuration; trying without first is what finds out that
// it is private, which decides whether the cluster needs credentials of its
// own to pull it. It costs a second round trip only for a private image.
type Remote struct{}

func (Remote) Resolve(ctx context.Context, ref string) (Pinned, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return Pinned{}, fmt.Errorf("image %q: %w", ref, err)
	}
	host := canonical(parsed.Context().Registry)

	if IsDigest(ref) {
		return Pinned{Ref: ref, Registry: host}, nil
	}

	// Head, not Get: the manifest itself is not needed, only what it hashes to.
	desc, err := remote.Head(parsed,
		remote.WithContext(ctx),
		remote.WithAuth(authn.Anonymous),
	)
	if err == nil {
		return Pinned{Ref: parsed.Context().Name() + "@" + desc.Digest.String(), Registry: host}, nil
	}
	if !refused(err) {
		return Pinned{}, fmt.Errorf("resolve %q: %w", ref, err)
	}

	desc, err = remote.Head(parsed,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
	)
	if err != nil {
		return Pinned{}, fmt.Errorf("resolve %q: %w", ref, err)
	}
	return Pinned{Ref: parsed.Context().Name() + "@" + desc.Digest.String(), Registry: host, Private: true}, nil
}

// refused reports whether a registry answered as it does to a request it
// will not serve without credentials. 404 is included because some registries
// answer that for a private repository rather than admit it exists; a
// genuinely missing image fails the credentialed retry too, and reports that.
//
// Anything else — a timeout, a refused connection — is not retried, so a
// network blip is never mistaken for a private image.
func refused(err error) bool {
	var terr *transport.Error
	if !errors.As(err, &terr) {
		return false
	}
	switch terr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

// Fake resolves from a table, for tests and for any path that must not touch
// the network.
type Fake map[string]string

func (f Fake) Resolve(_ context.Context, ref string) (Pinned, error) {
	if IsDigest(ref) {
		return Pinned{Ref: ref}, nil
	}
	if got, ok := f[ref]; ok {
		return Pinned{Ref: got}, nil
	}
	return Pinned{}, fmt.Errorf("resolve %q: not in the fake registry", ref)
}
