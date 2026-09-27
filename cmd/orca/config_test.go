package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `
nodes:
  - host: root@203.0.113.10
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// A node with no name gets one derived from its address, so a minimal
	// config never has to invent node names.
	if got := cfg.Nodes[0].Name; got != "203-0-113-10" {
		t.Errorf("derived name = %q, want %q", got, "203-0-113-10")
	}
	if got := cfg.Nodes[0].Role; got != RoleServer {
		t.Errorf("first node role = %q, want %q", got, RoleServer)
	}
}

func TestLoadConfigRejectsUnknownKeys(t *testing.T) {
	// A typo must fail loudly rather than silently taking a default — the whole
	// reason the decoder runs in strict mode.
	_, err := LoadConfig(writeConfig(t, `
datadir: /srv/orca
nodes:
  - host: root@203.0.113.10
`))
	if err == nil {
		t.Fatal("expected an error for the unknown key 'datadir'")
	}
	if !strings.Contains(err.Error(), "datadir") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "no nodes",
			body:    "monitoring:\n  domain: example.com\n",
			wantErr: "no nodes defined",
		},
		{
			name:    "missing host",
			body:    "nodes:\n  - name: box0\n",
			wantErr: "host is required",
		},
		{
			name:    "duplicate name",
			body:    "nodes:\n  - {host: root@10.0.0.1, name: box}\n  - {host: root@10.0.0.2, name: box, private_ip: 10.0.0.2}\n",
			wantErr: "duplicate node name",
		},
		{
			name: "even server count",
			body: `nodes:
  - {host: root@10.0.0.1, name: a, private_ip: 10.0.0.1, role: server}
  - {host: root@10.0.0.2, name: b, private_ip: 10.0.0.2, role: server}
`,
			wantErr: "raft needs an odd count",
		},
		{
			name: "multi-node without private_ip",
			body: `nodes:
  - {host: root@10.0.0.1, name: a, private_ip: 10.0.0.1}
  - {host: root@10.0.0.2, name: b, role: client}
`,
			wantErr: "private_ip is required",
		},
		{
			name:    "no server",
			body:    "nodes:\n  - {host: root@10.0.0.1, role: client}\n",
			wantErr: `no node has role "server"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestSingleNodeOmitsIP(t *testing.T) {
	// The single-machine case must work with nothing but an SSH address: no
	// hand-measured IPs, no interface names.
	cfg, err := LoadConfig(writeConfig(t, "nodes:\n  - host: root@203.0.113.10\n"))
	if err != nil {
		t.Fatalf("a one-node config with no ip should be valid: %v", err)
	}
	if len(cfg.Servers()) != 1 {
		t.Errorf("Servers() = %d, want 1", len(cfg.Servers()))
	}
}

const threeMachines = `
nodes:
  - host: root@203.0.113.10
    name: box0
    private_ip: 10.0.0.1
  - host: root@203.0.113.11
    name: box1
    private_ip: 10.0.0.2
  - host: root@203.0.113.12
    name: box2
    private_ip: 10.0.0.3
`

// Ingress and monitoring stay on the first server unless told otherwise, and
// each can be told separately.
func TestIngressAndMonitoringPlacement(t *testing.T) {
	tests := []struct {
		name                string
		extra               string
		ingress, monitoring string
	}{
		{"default", "", "box0", "box0"},
		{"monitoring moved", "monitoring:\n  node: box2\n", "box0", "box2"},
		{"both moved", "ingress:\n  node: box1\nmonitoring:\n  domain: example.com\n  node: box2\n  logs:\n    retention: 7d\n", "box1", "box2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeConfig(t, threeMachines+tt.extra))
			if err != nil {
				t.Fatal(err)
			}
			in, _ := cfg.IngressNode()
			mon, _ := cfg.MonitoringNode()
			if in.Name != tt.ingress || mon.Name != tt.monitoring {
				t.Errorf("ingress on %s, monitoring on %s; want %s and %s", in.Name, mon.Name, tt.ingress, tt.monitoring)
			}
			// Moving monitoring moves the status page's binary with it.
			opts := platformOptions(cfg, "", nil)
			if opts.IngressNode != tt.ingress || opts.MonitoringNode != tt.monitoring {
				t.Errorf("platform options: ingress %s, monitoring %s", opts.IngressNode, opts.MonitoringNode)
			}
		})
	}
}

func TestPlacementRejectsAnUnknownNode(t *testing.T) {
	for _, extra := range []string{"ingress:\n  node: box9\n", "monitoring:\n  node: box9\n"} {
		_, err := LoadConfig(writeConfig(t, threeMachines+extra))
		if err == nil || !strings.Contains(err.Error(), `"box9"`) {
			t.Errorf("%q: want an error naming box9, got %v", extra, err)
		}
	}
}
