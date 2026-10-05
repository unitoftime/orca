package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/internal/deploy"
)

// applyLockTTL is how long the apply lock outlives the last renewal. An apply
// that dies without releasing it holds up the next one for this long and no
// longer.
const applyLockTTL = 60 * time.Second

// applyLockRenew is how often a running apply renews it: a few chances before
// the TTL runs out, so one slow round trip does not lose it.
const applyLockRenew = applyLockTTL / 4

// LockApply takes the cluster-wide apply lock, and holds it until release is
// called or ctx ends.
//
// Two applies at once each plan against what they saw, and the one that
// finishes second undoes the first: it redeploys the jobs it planned and
// stops the services it did not know about. Checking each job's index on
// submit catches the first case but not the second, so an apply holds the
// whole cluster.
//
// The lock is one of Nomad's variable locks, which lapse unless renewed, so a
// crashed or disconnected apply blocks the next one for at most the TTL.
// Losing the lock mid-apply cancels the context it returns: carrying on
// without it is exactly the race it exists to prevent.
func (c *Cluster) LockApply(ctx context.Context) (context.Context, func(), error) {
	host, _ := os.Hostname()
	holder := strings.TrimPrefix(os.Getenv("USER")+"@"+host, "@")

	id, err := c.lockOp(ctx, "lock-acquire", nomad.Variable{
		Path:  deploy.ApplyLockPath,
		Items: map[string]string{"holder": holder, "since": time.Now().UTC().Format(time.RFC3339)},
		Lock:  &nomad.VariableLock{TTL: applyLockTTL.String(), LockDelay: "1s"},
	})
	if err != nil {
		return nil, nil, err
	}
	if id == "" {
		return nil, nil, errors.New("apply lock: nomad answered without a lock")
	}
	held := nomad.Variable{Path: deploy.ApplyLockPath, Lock: &nomad.VariableLock{ID: id}}

	ctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(applyLockRenew)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if _, err := c.lockOp(ctx, "lock-renew", held); err != nil && ctx.Err() == nil {
					cancel(fmt.Errorf("lost the apply lock, so stopped rather than race another apply: %w", err))
					return
				}
			}
		}
	}()

	release := func() {
		close(done)
		// Released on a context of its own: the apply's may be the thing
		// that just ended, and the lock should not wait out its TTL for it.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer rcancel()
		_, _ = c.lockOp(rctx, "lock-release", held)
		cancel(nil)
	}
	return ctx, release, nil
}

// lockOp runs one lock operation on the apply lock's variable and returns the
// lock's ID.
func (c *Cluster) lockOp(ctx context.Context, op string, v nomad.Variable) (string, error) {
	api, err := c.client(ctx)
	if err != nil {
		return "", fmt.Errorf("apply lock: %w", err)
	}
	var got nomad.Variable
	_, err = api.Raw().Write("/v1/var/"+v.Path+"?"+op, &v, &got, (&nomad.WriteOptions{}).WithContext(ctx))

	// A conflict is an answer, carrying the variable as its current holder
	// left it.
	var unexpected nomad.UnexpectedResponseError
	if errors.As(err, &unexpected) && unexpected.StatusCode() == http.StatusConflict {
		var held nomad.Variable
		_ = json.Unmarshal([]byte(unexpected.Body()), &held)
		if h := held.Items["holder"]; h != "" {
			return "", fmt.Errorf("another apply is running (%s, since %s); try again when it finishes", h, held.Items["since"])
		}
		return "", errors.New("another apply is running; try again when it finishes")
	}
	if err != nil {
		return "", fmt.Errorf("apply lock: %w", err)
	}
	if got.Lock == nil {
		return "", nil
	}
	return got.Lock.ID, nil
}
