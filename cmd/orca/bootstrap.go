package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/unitoftime/orca/pkg/deploy"
)

// phase is one host-setup script, plus any extra templates staged beside it.
type phase struct {
	name       string
	script     string   // template name under templates/host/
	extraFiles []string // config files the script installs
}

// hostPhases take a fresh box to a running Nomad node. Each script is
// idempotent, so re-running bootstrap on a live node converges it rather than
// rebuilding it. That is how a version bump in versions.go rolls out.
var hostPhases = []phase{
	{name: "prep", script: "prep.sh"},
	{name: "docker", script: "docker.sh"},
	{name: "nomad", script: "nomad.sh", extraFiles: []string{"nomad.hcl", "docker-credential-orca"}},
}

const remoteStageDir = "/tmp/orca-bootstrap"

// cmdBootstrap brings one node (or every node, if ref is empty) to a ready
// state: packages, docker, nomad, running and joined.
func cmdBootstrap(ctx context.Context, cfg Config, ref string, sshCopyID bool) error {
	nodes := cfg.Nodes
	if ref != "" {
		n, ok := cfg.FindNode(ref)
		if !ok {
			return fmt.Errorf("no node matching %q in config", ref)
		}
		nodes = []NodeConfig{n}
	}

	for _, nc := range nodes {
		fmt.Fprintf(os.Stderr, "\n=== bootstrap %s (%s, %s) ===\n", nc.Name, nc.Host, nc.Role)
		if err := bootstrapNode(ctx, cfg, nc, sshCopyID); err != nil {
			return fmt.Errorf("bootstrap %s: %w", nc.Host, err)
		}
	}

	// The leader check happens once, after every machine is up, rather than at
	// the end of each one. bootstrap_expect is the server count, so on a
	// three-server cluster the first machine cannot elect a leader until the
	// other two exist, so waiting for one there would fail every multi-machine
	// bootstrap on its first node.
	servers := cfg.Servers()
	if len(servers) == 0 {
		return fmt.Errorf("no server node to wait on")
	}
	fmt.Fprintf(os.Stderr, "\n=== cluster ===\n")
	var list StepList
	list.Add("Wait for Nomad to elect a leader", func(ctx context.Context) error {
		return waitForNomad(ctx, Node{Host: servers[0].Host})
	})
	if len(cfg.Nodes) > 1 {
		list.Add("Wait for every machine to join", func(ctx context.Context) error {
			return waitForNodes(ctx, Node{Host: servers[0].Host}, len(cfg.Nodes))
		})
	}
	// Made once, here, so the dashboards are behind a password from the first
	// apply without your choosing one. Run again, it keeps the one there.
	list.Add("Generate the dashboard password", func(ctx context.Context) error {
		_, err := ensureAdminPassword(ctx, NewCluster(Node{Host: servers[0].Host}))
		return err
	})
	return list.Run(ctx)
}

func bootstrapNode(ctx context.Context, cfg Config, nc NodeConfig, sshCopyID bool) error {
	node := Node{Host: nc.Host}

	vars, err := nodeVars(cfg, nc)
	if err != nil {
		return err
	}

	// Render every script this node runs into a staging directory, then ship
	// the directory in one rsync. Rendering locally keeps the remote side dumb:
	// the box only ever executes concrete scripts with no variables to resolve.
	tmpDir, err := os.MkdirTemp("", "orca-bootstrap-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	files := map[string]bool{}
	for _, p := range hostPhases {
		files[p.script] = true
		for _, f := range p.extraFiles {
			files[f] = true
		}
	}
	for name := range files {
		rendered, err := RenderTemplate("templates/host/"+name, vars)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(tmpDir, name), []byte(rendered), 0o755); err != nil {
			return fmt.Errorf("stage %s: %w", name, err)
		}
	}

	var list StepList

	if sshCopyID {
		list.Add("Authorize SSH key on remote host", func(ctx context.Context) error {
			c := exec.CommandContext(ctx, "ssh-copy-id", node.Host)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		})
	}

	list.AddRun(node, "Install rsync", "command -v rsync >/dev/null || (apt-get update && apt-get install -y rsync)")
	list.AddRun(node, "Create staging directory", "mkdir -p "+remoteStageDir)
	list.AddUploadDir(node, "Upload setup scripts", tmpDir+"/", remoteStageDir+"/", "--delete")
	list.AddRun(node, "Make scripts executable", "chmod +x "+remoteStageDir+"/*.sh")

	for _, p := range hostPhases {
		list.AddRun(node, "Run "+p.script, fmt.Sprintf("cd %s && ./%s", remoteStageDir, p.script))
	}

	return list.Run(ctx)
}

// nodeVars builds the {{KEY}} substitutions for one node's scripts.
func nodeVars(cfg Config, nc NodeConfig) (map[string]string, error) {
	return map[string]string{
		"NOMAD_VERSION":       Versions.Nomad,
		"DOCKER_VERSION":      Versions.Docker,
		"CNI_VERSION":         Versions.CNIPlugins,
		"NOMAD_SHA256":        Versions.NomadSHA256,
		"DOCKER_SHA256":       Versions.DockerSHA256,
		"CNI_SHA256":          Versions.CNIPluginsSHA256,
		"DATACENTER":          Datacenter,
		"NODE_NAME":           nc.Name,
		"DATA_DIR":            DataDir,
		"BIND_IP":             nc.PrivateIP, // empty on a single machine: binds loopback
		"ROLE":                string(nc.Role),
		"SERVER_COUNT":        fmt.Sprint(len(cfg.Servers())),
		"NOMAD_SERVER_STANZA": nomadServerStanza(cfg, nc),
		"NOMAD_CLIENT_JOIN":   nomadClientJoin(cfg, nc),
		"NOMAD_ADDR":          NomadAddr,
		"REGISTRY_PREFIX":     deploy.RegistryPrefix,
	}, nil
}

// nomadServerStanza renders the server block for this node, or nothing if the
// node is a pure client. retry_join is omitted on a single-node cluster because
// there are no peers to join: the one server bootstraps itself.
func nomadServerStanza(cfg Config, nc NodeConfig) string {
	if nc.Role != RoleServer {
		return "# client-only node: no server stanza"
	}

	servers := cfg.Servers()

	var b strings.Builder
	b.WriteString("server {\n")
	b.WriteString("  enabled          = true\n")
	fmt.Fprintf(&b, "  bootstrap_expect = %d\n", len(servers))

	var peers []string
	for _, s := range servers {
		if s.Host == nc.Host || s.PrivateIP == "" {
			continue
		}
		peers = append(peers, fmt.Sprintf("%q", s.PrivateIP))
	}
	if len(peers) > 0 {
		b.WriteString("\n  server_join {\n")
		fmt.Fprintf(&b, "    retry_join = [%s]\n", strings.Join(peers, ", "))
		b.WriteString("    retry_max      = 0\n")
		b.WriteString("    retry_interval = \"15s\"\n")
		b.WriteString("  }\n")
	}
	b.WriteString("}")
	return b.String()
}

// nomadClientJoin tells a client-only machine where the servers are.
//
// A machine that runs a server finds it locally. One that does not has no
// other way to learn where the cluster is: without this, a client node would
// start, never join, and bootstrap's wait for every machine would time out.
func nomadClientJoin(cfg Config, nc NodeConfig) string {
	if nc.Role == RoleServer {
		return "  # runs a server: its client joins it locally"
	}
	var servers []string
	for _, s := range cfg.Servers() {
		servers = append(servers, fmt.Sprintf("%q", s.PrivateIP))
	}
	return fmt.Sprintf(`  server_join {
    retry_join     = [%s]
    retry_max      = 0
    retry_interval = "15s"
  }`, strings.Join(servers, ", "))
}

// waitForNodes blocks until every machine has registered as a client. A server
// that has formed raft still has no capacity until its clients check in, so
// without this a multi-machine bootstrap can finish while workloads have
// nowhere to run.
func waitForNodes(ctx context.Context, node Node, want int) error {
	script := fmt.Sprintf(`for i in $(seq 1 30); do
  N=$(curl -sf --max-time 5 %s/v1/nodes 2>/dev/null | jq '[.[] | select(.Status == "ready")] | length' 2>/dev/null || echo 0)
  if [ "$N" -ge %d ]; then
    echo "$N of %d machines ready"
    exit 0
  fi
  sleep 2
done
echo "only $N of %d machines became ready within 60s" >&2
exit 1`, NomadAddr, want, want, want)
	return node.Run(ctx, script)
}

// waitForNomad blocks until the agent answers and reports a leader. Without the
// leader check, bootstrap can "succeed" against an agent that is up but has not
// formed raft yet, and the very next command fails confusingly.
func waitForNomad(ctx context.Context, node Node) error {
	// curl, not `nomad operator api`: that subcommand infers a write method when
	// its stdin is not a terminal, which over SSH it never is, and the API then
	// rejects the request as "Invalid method".
	const script = `for i in $(seq 1 30); do
  LEADER=$(curl -sf --max-time 5 ` + NomadAddr + `/v1/status/leader 2>/dev/null || true)
  case "$LEADER" in
    *:*) echo "nomad leader: $LEADER"; exit 0 ;;
  esac
  sleep 2
done
echo "nomad did not report a leader within 60s" >&2
systemctl --no-pager --lines=30 status nomad >&2 || true
journalctl -u nomad --no-pager -n 20 >&2 || true
exit 1`

	return node.Run(ctx, script)
}
