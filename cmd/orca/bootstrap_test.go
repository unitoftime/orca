package main

import (
	"strings"
	"testing"
)

// TestTemplatesFullySubstituted is the guard on the {{KEY}} rendering scheme:
// a placeholder nobody supplies renders literally onto the host and fails at
// 3am inside a shell script instead of here. Every template every phase stages
// must come out the other side with no placeholders left.
func TestTemplatesFullySubstituted(t *testing.T) {
	cfg := Config{
		Nodes: []NodeConfig{{Host: "root@203.0.113.10", Name: "box0", Role: roleServer}},
	}
	vars, err := nodeVars(cfg, cfg.Nodes[0])
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range hostPhases {
		for _, name := range append([]string{p.script}, p.extraFiles...) {
			out, err := renderTemplate("templates/host/"+name, vars)
			if err != nil {
				t.Fatalf("render %s: %v", name, err)
			}
			if idx := strings.Index(out, "{{"); idx >= 0 {
				end := min(idx+40, len(out))
				t.Errorf("%s: unsubstituted placeholder near %q", name, out[idx:end])
			}
		}
	}
}

func TestNomadServerStanzaSingleNode(t *testing.T) {
	cfg := Config{Nodes: []NodeConfig{{Host: "root@203.0.113.10", Name: "box0", Role: roleServer}}}

	got := nomadServerStanza(cfg, cfg.Nodes[0])

	if !strings.Contains(got, "bootstrap_expect = 1") {
		t.Errorf("want bootstrap_expect = 1, got:\n%s", got)
	}
	// With no peers there is nothing to join; an empty retry_join list would
	// make Nomad log join failures forever on a perfectly healthy cluster.
	if strings.Contains(got, "retry_join") {
		t.Errorf("single node should emit no retry_join, got:\n%s", got)
	}
}

func TestNomadServerStanzaThreeNodes(t *testing.T) {
	cfg := Config{Nodes: []NodeConfig{
		{Host: "root@10.0.0.1", Name: "a", PrivateIP: "10.0.0.1", Role: roleServer},
		{Host: "root@10.0.0.2", Name: "b", PrivateIP: "10.0.0.2", Role: roleServer},
		{Host: "root@10.0.0.3", Name: "c", PrivateIP: "10.0.0.3", Role: roleServer},
	}}

	got := nomadServerStanza(cfg, cfg.Nodes[0])

	if !strings.Contains(got, "bootstrap_expect = 3") {
		t.Errorf("want bootstrap_expect = 3, got:\n%s", got)
	}
	// A node must not list itself as a peer to join.
	if strings.Contains(got, `"10.0.0.1"`) {
		t.Errorf("node should not retry_join itself, got:\n%s", got)
	}
	for _, peer := range []string{`"10.0.0.2"`, `"10.0.0.3"`} {
		if !strings.Contains(got, peer) {
			t.Errorf("missing peer %s, got:\n%s", peer, got)
		}
	}
}

func TestNomadServerStanzaClientNode(t *testing.T) {
	cfg := Config{Nodes: []NodeConfig{
		{Host: "root@10.0.0.1", Name: "a", PrivateIP: "10.0.0.1", Role: roleServer},
		{Host: "root@10.0.0.2", Name: "b", PrivateIP: "10.0.0.2", Role: roleClient},
	}}

	if got := nomadServerStanza(cfg, cfg.Nodes[1]); strings.Contains(got, "enabled") {
		t.Errorf("client node should emit no server stanza, got:\n%s", got)
	}
}

// A client-only machine has no local server to find, so it is told where the
// servers are. Without it the machine never joins.
func TestClientNodeJoinsTheServers(t *testing.T) {
	cfg := Config{Nodes: []NodeConfig{
		{Host: "root@10.0.0.1", Name: "a", PrivateIP: "10.0.0.1", Role: roleServer},
		{Host: "root@10.0.0.2", Name: "b", PrivateIP: "10.0.0.2", Role: roleClient},
	}}
	vars, err := nodeVars(cfg, cfg.Nodes[1])
	if err != nil {
		t.Fatal(err)
	}
	hcl, err := renderTemplate("templates/host/nomad.hcl", vars)
	if err != nil {
		t.Fatal(err)
	}
	client := hcl[strings.Index(hcl, "client {"):]
	if !strings.Contains(client, "server_join") || !strings.Contains(client, `retry_join     = ["10.0.0.1"]`) {
		t.Errorf("client node must join the servers:\n%s", client)
	}
	if got := nomadClientJoin(cfg, cfg.Nodes[0]); strings.Contains(got, "server_join") {
		t.Errorf("a server's client joins locally, got %q", got)
	}
}

func TestBindIPEmptyOnSingleNode(t *testing.T) {
	// Empty BIND_IP is the signal nomad.sh uses to bind loopback. If this ever
	// starts resolving to a real address by accident, a single-box install
	// silently exposes the Nomad API.
	cfg := Config{
		Nodes: []NodeConfig{{Host: "root@203.0.113.10", Name: "box0", Role: roleServer}},
	}
	vars, err := nodeVars(cfg, cfg.Nodes[0])
	if err != nil {
		t.Fatal(err)
	}
	if vars["BIND_IP"] != "" {
		t.Errorf("BIND_IP = %q, want empty for a single-node cluster", vars["BIND_IP"])
	}
}
