package main

import (
	"strings"
	"testing"
)

// A volume whose machine moved in the config would start empty and look
// healthy. Data found anywhere but where the service is going stops the apply.
func TestMisplacedVolumesAreRefused(t *testing.T) {
	dirs := []VolumeDir{
		{Path: "/v/shop/db", Node: "box1"},
		{Path: "/v/shop/cache", Node: "box0"},
		{Path: "/v/blog/db", Node: "box1"},
	}
	holders := map[string][]string{
		"/v/shop/db":    {"box0"}, // moved away from its data
		"/v/shop/cache": {"box0"}, // where it always was
		// blog/db is new: no data anywhere yet
	}
	err := misplacedVolumes(dirs, holders)
	if err == nil || !strings.Contains(err.Error(), "/v/shop/db holds data on box0") {
		t.Fatalf("want shop/db refused, got %v", err)
	}
	if strings.Contains(err.Error(), "cache") || strings.Contains(err.Error(), "blog") {
		t.Errorf("only the moved volume should be refused: %v", err)
	}
}

// The guard in front of a recursive delete. These inputs cannot occur in
// normal operation, which is the point: it fires on a bug, and a bug in front
// of rm -rf is the one worth catching.
func TestValidVolumePart(t *testing.T) {
	for _, ok := range []string{"blog", "db", "a-b-c"} {
		if err := validVolumePart(ok); err != nil {
			t.Errorf("validVolumePart(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "..", "../etc", "blog/db", `blog\db`, "-rf", "a/../../b"} {
		if err := validVolumePart(bad); err == nil {
			t.Errorf("validVolumePart(%q) should be refused", bad)
		}
	}
	if _, err := deleteVolumesScript([]Volume{{Group: "shop", Service: "../x"}}); err == nil {
		t.Error("a volume whose service is a path must be refused before anything is deleted")
	}
}

// A new volume is owned by the user the image runs as, and one that already
// exists is left alone.
func TestEnsureDirsCreatesOnlyMissingVolumes(t *testing.T) {
	script := ensureDirsScript([]VolumeDir{{Path: "/var/orca/volumes/services/shop/db", Owner: 70}})
	for _, want := range []string{
		"if [ ! -d '/var/orca/volumes/services/shop/db' ]; then",
		"mkdir -p '/var/orca/volumes/services/shop/db'",
		"chown 70:70 '/var/orca/volumes/services/shop/db'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
}
