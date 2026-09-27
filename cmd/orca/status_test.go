package main

import (
	"strings"
	"testing"

	"github.com/unitoftime/orca/pkg/deploy"
)

// A job waited on that never shows up is a failure, not a pass.
func TestUnhealthyIncludesJobsNeverSeen(t *testing.T) {
	want := map[string]bool{"shop-app": true, "shop-db": true}
	settled := map[string]deploy.ServiceStatus{
		"shop-app": {JobID: "shop-app", App: "shop", Service: "app", Health: deploy.HealthOK},
	}
	bad := unhealthy(want, settled)
	if len(bad) != 1 || !strings.Contains(bad[0], "shop-db (not found") {
		t.Errorf("bad = %v, want shop-db reported as missing", bad)
	}
}
