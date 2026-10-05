package registry

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostCanonical(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io":                   "ghcr.io",
		"GHCR.io":                   "ghcr.io",
		"https://ghcr.io/":          "ghcr.io",
		"docker.io":                 DockerHub,
		"index.docker.io":           DockerHub,
		"registry.example.com:5000": "registry.example.com:5000",
		"localhost:5000":            "localhost:5000",
	} {
		got, err := Host(in)
		if err != nil {
			t.Errorf("Host(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Host(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostRefusesWhatIsNotAHost(t *testing.T) {
	for _, in := range []string{"", "ghcr.io/you/app", "you@ghcr.io", "my_registry.local"} {
		if got, err := Host(in); err == nil {
			t.Errorf("Host(%q) = %q, want an error", in, got)
		}
	}
}

// An image's registry and the registry a person logs in to have to come out
// as the same string, or credentials are stored under a name no pull asks for.
func TestResolveNamesRegistryLikeHost(t *testing.T) {
	for ref, want := range map[string]string{
		"busybox@sha256:" + strings.Repeat("a", 64):                    DockerHub,
		"ghcr.io/you/app@sha256:" + strings.Repeat("a", 64):            "ghcr.io",
		"localhost:5000/private/app@sha256:" + strings.Repeat("a", 64): "localhost:5000",
	} {
		p, err := Remote{}.Resolve(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if p.Registry != want {
			t.Errorf("%s: registry %q, want %q", ref, p.Registry, want)
		}
		if p.Private {
			t.Errorf("%s: a digest is never looked up, so cannot be known private", ref)
		}
	}
}

// fakeRegistry serves one image, anonymously or only with basic auth.
func fakeRegistry(t *testing.T, private bool) *httptest.Server {
	t.Helper()
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if private {
			if u, p, ok := r.BasicAuth(); !ok || u != "you" || p != "token" {
				w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		switch r.URL.Path {
		case "/v2/":
			w.WriteHeader(http.StatusOK)
		case "/v2/app/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", digest)
			w.Header().Set("Content-Length", "2")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolvePublicIsNotPrivate(t *testing.T) {
	srv := fakeRegistry(t, false)
	host := strings.TrimPrefix(srv.URL, "http://")

	p, err := Remote{}.Resolve(context.Background(), host+"/app:latest")
	if err != nil {
		t.Fatal(err)
	}
	if p.Private {
		t.Error("an image served anonymously was reported private")
	}
	if !IsDigest(p.Ref) {
		t.Errorf("not pinned: %s", p.Ref)
	}
}

// With no local credentials for it, a private image fails to resolve at all;
// the anonymous attempt changes nothing for it.
func TestResolvePrivateWithoutLocalCredentialsFails(t *testing.T) {
	srv := fakeRegistry(t, true)
	host := strings.TrimPrefix(srv.URL, "http://")
	t.Setenv("DOCKER_CONFIG", t.TempDir())

	if _, err := (Remote{}).Resolve(context.Background(), host+"/app:latest"); err == nil {
		t.Fatal("resolved a private image with no credentials anywhere")
	}
}

func TestResolvePrivateWithLocalCredentials(t *testing.T) {
	srv := fakeRegistry(t, true)
	host := strings.TrimPrefix(srv.URL, "http://")
	writeDockerConfig(t, host, "you", "token")

	p, err := Remote{}.Resolve(context.Background(), host+"/app:latest")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Private {
		t.Error("an image that needed credentials was not reported private")
	}
	if p.Registry != host {
		t.Errorf("registry %q, want %q", p.Registry, host)
	}
}

func TestVerify(t *testing.T) {
	srv := fakeRegistry(t, true)
	host := strings.TrimPrefix(srv.URL, "http://")
	ctx := context.Background()

	if err := Verify(ctx, host, "you", "token"); err != nil {
		t.Errorf("good credentials: %v", err)
	}
	if err := Verify(ctx, host, "you", "wrong"); err == nil {
		t.Error("bad credentials were accepted")
	}
}

// writeDockerConfig points the docker keychain at a config holding one login,
// so a test never reads, or depends on, the credentials of whoever runs it.
func writeDockerConfig(t *testing.T, host, user, pass string) {
	t.Helper()
	dir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	cfg := `{"auths":{"` + host + `":{"auth":"` + auth + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
}
