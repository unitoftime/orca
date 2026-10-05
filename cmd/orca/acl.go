package main

import (
	"context"
	"fmt"
	"path"
	"time"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/internal/deploy"
)

// The cluster's token is the one credential orca holds for Nomad: made once,
// by bootstrap, and kept on every machine in a file only root can read. This
// machine never stores it. Each command reads it over SSH, so being able to
// log in to a server as root is still the whole of what running orca needs.

// mintTokenScript makes sure this machine holds a token Nomad accepts,
// asking Nomad for the cluster's first one if it has none.
//
// Nomad hands that token out once. If it has been handed out and is not here
// (the file was lost, or the machine was rebuilt around its data), Nomad
// names the index a reset has to quote, and writing it where the leader
// looks lets it hand out another. That takes root on the server, which is
// the same thing the token is worth.
var mintTokenScript = fmt.Sprintf(`set -eu
umask 077
TOKEN=%[1]s
RESET=%[2]s
mkdir -p "$(dirname "$TOKEN")"

if [ -s "$TOKEN" ] && NOMAD_TOKEN=$(cat "$TOKEN") nomad acl token self >/dev/null 2>&1; then
  echo "the cluster's token is in place"
  exit 0
fi

ERR=$(mktemp)
trap 'rm -f "$ERR" "$TOKEN.new"' EXIT
if ! OUT=$(nomad acl bootstrap -json 2>"$ERR"); then
  INDEX=$(grep -o 'reset index: [0-9]*' "$ERR" | grep -o '[0-9]*' || true)
  if [ -z "$INDEX" ]; then
    cat "$ERR" >&2
    exit 1
  fi
  echo "the cluster has a token that is not on this machine; resetting it"
  echo "$INDEX" > "$RESET"
  if ! OUT=$(nomad acl bootstrap -json 2>"$ERR"); then
    cat "$ERR" >&2
    echo "the reset is read by the leader: if this machine is not it, run bootstrap for the one that is" >&2
    exit 1
  fi
fi
printf '%%s' "$OUT" | jq -er .SecretID > "$TOKEN.new"
mv "$TOKEN.new" "$TOKEN"
echo "made the cluster's token"`,
	shQuote(tokenPath), shQuote(path.Join(dataDir, "nomad/server/acl-bootstrap-reset")))

// shareToken copies the cluster's token from one machine to others. Every
// machine pulls images with it, and any server may be the one orca talks to.
//
// On stdin, so it is in no argument list on either end.
func shareToken(ctx context.Context, from Node, to []Node) error {
	token, err := from.RunOutput(ctx, "cat "+shQuote(tokenPath))
	if err != nil {
		return err
	}
	script := fmt.Sprintf(`set -eu
umask 077
mkdir -p "$(dirname %[1]s)"
cat > %[1]s.new
mv %[1]s.new %[1]s`, shQuote(tokenPath))
	for _, n := range to {
		if _, err := n.RunStdin(ctx, script, []byte(token)); err != nil {
			return err
		}
	}
	return nil
}

// WritePolicies makes Nomad's policies the ones this build of orca defines.
func (c *Cluster) WritePolicies(ctx context.Context) error {
	api, err := c.client(ctx)
	if err != nil {
		return err
	}
	return deploy.WriteACLPolicies(ctx, api)
}

// WaitForNodes blocks until every machine has registered as a client. A
// server that has formed raft still has no capacity until its clients check
// in, so without this a multi-machine bootstrap can finish while workloads
// have nowhere to run.
func (c *Cluster) WaitForNodes(ctx context.Context, want int) error {
	api, err := c.client(ctx)
	if err != nil {
		return err
	}
	ready := 0
	for range 30 {
		nodes, _, err := api.Nodes().List((&nomad.QueryOptions{}).WithContext(ctx))
		if err == nil {
			ready = 0
			for _, n := range nodes {
				if n.Status == nomad.NodeStatusReady {
					ready++
				}
			}
			if ready >= want {
				fmt.Fprintf(getStdout(ctx), "%d of %d machines ready\n", ready, want)
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("only %d of %d machines became ready within 60s", ready, want)
}
