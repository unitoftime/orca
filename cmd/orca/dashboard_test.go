package main

import (
	"context"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestResolveAuthHashNoPassword(t *testing.T) {
	got, err := resolveAuthHash(context.Background(), nil, "")
	if err != nil || got != "" {
		t.Errorf("no password should produce no hash, got %q, %v", got, err)
	}
}

func TestResolveAuthHashGeneratesUsableHash(t *testing.T) {
	got, err := resolveAuthHash(context.Background(), nil, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(got), []byte("s3cret")); err != nil {
		t.Errorf("hash does not verify against the password: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(got), []byte("wrong")); err == nil {
		t.Error("hash verified against the wrong password")
	}
}

// bcrypt salts randomly, so two hashes of the same password differ. If orca
// hashed afresh on every apply the ingress job spec would change every time and
// redeploy forever. This documents why the deployed hash is reused rather than
// regenerated.
func TestBcryptIsNotDeterministic(t *testing.T) {
	a, _ := bcrypt.GenerateFromPassword([]byte("same"), bcrypt.MinCost)
	b, _ := bcrypt.GenerateFromPassword([]byte("same"), bcrypt.MinCost)
	if string(a) == string(b) {
		t.Skip("bcrypt produced identical hashes; the reuse logic would be unnecessary")
	}
	// Both must still verify, which is what makes reuse safe.
	for _, h := range [][]byte{a, b} {
		if err := bcrypt.CompareHashAndPassword(h, []byte("same")); err != nil {
			t.Errorf("hash does not verify: %v", err)
		}
	}
}
