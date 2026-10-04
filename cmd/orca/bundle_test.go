package main

import (
	"reflect"
	"testing"

	"github.com/unitoftime/orca/pkg/deploy"
)

// An exported file is what a cluster is rebuilt from, so what comes back out
// of one has to be exactly what the cluster held: every kind of variable, and
// a value's every byte.
func TestBundleSurvivesExportAndImport(t *testing.T) {
	held := variables{
		deploy.SecretPath("shop", "db_password"): {deploy.SecretItemKey: "hunter2"},
		deploy.SecretPath("shop", "gh_key"):      {deploy.SecretItemKey: "-----BEGIN KEY-----\n a \"b\" $c \\ # d: e\n-----END KEY-----"},
		deploy.RegistryPath("ghcr.io"):           registryItems("you", "token"),
		deploy.RegistryPath("localhost:5000"):    registryItems("you", "token"),
		deploy.AdminPasswordPath:                 {deploy.AdminPasswordKey: "0123456789abcdef"},
	}

	file, err := encodeBundle(bundleOf(held), "a passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if !sealed(file) {
		t.Fatal("a bundle written with a passphrase must be encrypted")
	}
	if _, err := unseal(file, "another passphrase"); err == nil {
		t.Error("the wrong passphrase must not open it")
	}
	doc, err := unseal(file, "a passphrase")
	if err != nil {
		t.Fatal(err)
	}
	back, err := parseBundle(doc)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.variables(); !reflect.DeepEqual(got, held) {
		t.Errorf("came back as\n%v\nwant\n%v", got, held)
	}
}
