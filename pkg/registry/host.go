package registry

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// DockerHub is the name Docker Hub goes by. It is the one Nomad hands a
// credential helper when it pulls from Hub, so it is the one credentials are
// stored under: go-containerregistry calls the same registry index.docker.io,
// and a lookup under that name would find nothing.
const DockerHub = "docker.io"

// Host is the canonical name of a registry as a person typed it: lowercased,
// with any scheme or trailing slash dropped, and Docker Hub under the name
// Nomad asks for it by. It is what credentials are stored under, so every
// spelling of one registry has to come out the same.
func Host(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimSuffix(s, "/")

	// A path means an image or a repository was given rather than a registry;
	// credentials belong to the whole host. "_" and "~" cannot appear in a
	// host name, and are what the stored path uses in place of "." and ":".
	if s == "" || strings.ContainsAny(s, "/_~@ ") {
		return "", fmt.Errorf("registry %q should be a host name, optionally with a port: ghcr.io, docker.io, registry.example.com:5000", s)
	}
	reg, err := name.NewRegistry(s)
	if err != nil {
		return "", fmt.Errorf("registry %q: %w", s, err)
	}
	return canonical(reg), nil
}

func canonical(reg name.Registry) string {
	h := strings.ToLower(reg.RegistryStr())
	if h == name.DefaultRegistry {
		return DockerHub
	}
	return h
}

// Verify checks that a registry accepts a username and token, the way
// `docker login` does: authenticate, then ask for the API root.
//
// It proves the credentials are good, not that they can read any particular
// image. A registry grants per-repository, and which repositories the
// cluster will pull is not known here. What it catches is the typo and the
// expired token, at the moment someone is there to fix them rather than at a
// pull on the machine.
func Verify(ctx context.Context, host, username, password string) error {
	regName := host
	if host == DockerHub {
		regName = name.DefaultRegistry
	}
	reg, err := name.NewRegistry(regName)
	if err != nil {
		return fmt.Errorf("registry %q: %w", host, err)
	}

	auth := authn.FromConfig(authn.AuthConfig{Username: username, Password: password})
	rt, err := transport.NewWithContext(ctx, reg, auth, remote.DefaultTransport, nil)
	if err != nil {
		if refused(err) {
			return fmt.Errorf("%s refused these credentials: %w", host, err)
		}
		return fmt.Errorf("reach %s: %w", host, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reg.Scheme()+"://"+reg.RegistryStr()+"/v2/", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		return fmt.Errorf("reach %s: %w", host, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s refused these credentials (HTTP %d)", host, resp.StatusCode)
	default:
		return fmt.Errorf("%s answered HTTP %d to a login check", host, resp.StatusCode)
	}
}
