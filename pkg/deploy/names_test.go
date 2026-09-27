package deploy

import "testing"

func TestRegistryPathRoundTrips(t *testing.T) {
	for _, host := range []string{"ghcr.io", "docker.io", "registry.example.com:5000", "localhost:5000", "10.0.0.5:5000"} {
		p := RegistryPath(host)
		if !validVariablePath(p) {
			t.Errorf("%s: %q is not a path Nomad accepts", host, p)
		}
		got, ok := RegistryHost(p)
		if !ok || got != host {
			t.Errorf("RegistryHost(RegistryPath(%q)) = %q, %v", host, got, ok)
		}
	}
}

func TestRegistryHostRejectsOtherPaths(t *testing.T) {
	for _, p := range []string{"orca/shop/db_password", RegistryPrefix, RegistryPrefix + "/", RegistryPrefix + "/a/b"} {
		if h, ok := RegistryHost(p); ok {
			t.Errorf("RegistryHost(%q) = %q, want not a registry path", p, h)
		}
	}
}

// validVariablePath is Nomad's own rule for a variable path
// (nomad/structs/variables.go): a path it refuses is a login that can never
// be stored.
func validVariablePath(p string) bool {
	if len(p) == 0 || len(p) > 128 {
		return false
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '~', r == '/':
		default:
			return false
		}
	}
	return true
}
