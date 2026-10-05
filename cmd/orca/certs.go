package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/unitoftime/orca/internal/deploy"
	"github.com/unitoftime/orca/internal/manifest"
)

// Every certificate the cluster serves: the ones ingress presents for
// hostname ports and the dashboards, and the ones services that serve TLS
// themselves are given (`tls:` in a manifest).
//
// The CLI's whole part is asking: it creates a certificate's record, waits
// for the certificate job to fill it in, and removes the record of a name
// nothing declares any more. Issuing and renewing happen on the cluster; see
// serve_certs.go.

// certRequest is one certificate something asks for.
type certRequest struct {
	Host  string
	Owner secretRef // the group and service asking

	// Required is a certificate its service cannot start without: one it
	// serves itself. The rest are ingress's, and a service behind ingress
	// runs the same whether or not its name can be proven yet, which is
	// what lets one be brought up before its DNS is moved.
	Required bool
}

// ingressOwner is who the dashboards' certificates are asked for by.
var ingressOwner = secretRef{deploy.OrcaApp, "traefik"}

// wantedCerts is every certificate these groups' services ask for, and with
// ownJobs, the ones orca's dashboards are published under.
func wantedCerts(cfg Config, groups []*manifest.Manifest, ownJobs bool) []certRequest {
	// Without HTTPS, ingress presents no certificate and no name can be
	// proven. A `tls:` is still returned, to be refused by name.
	https := cfg.Ingress.TLS()

	var out []certRequest
	for _, m := range groups {
		for _, s := range m.Services {
			owner := secretRef{m.App, s.Name}
			if s.TLS != "" {
				out = append(out, certRequest{Host: s.TLS, Owner: owner, Required: true})
			}
			for _, p := range s.Ports {
				if p.Kind == manifest.PortDomain && https {
					out = append(out, certRequest{Host: p.Domain, Owner: owner})
				}
			}
		}
	}
	if d := cfg.Monitoring.Domain; ownJobs && https && d != "" {
		for name, on := range map[string]bool{
			"status": cfg.Monitoring.Status.Enabled, "logs": cfg.Monitoring.Logs.Enabled, "metrics": cfg.Monitoring.Metrics.Enabled,
		} {
			if on {
				out = append(out, certRequest{Host: name + "." + d, Owner: ingressOwner})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// certPlan is what an apply does to the certificate records.
type certPlan struct {
	Request []certRequest // to ask for now
	Waiting []string      // asked for before and still not issued, with why
	Remove  []string      // hostnames nothing in scope declares any more
}

func (p certPlan) String() string {
	var b strings.Builder
	for _, r := range p.Request {
		fmt.Fprintf(&b, "  cert    %s for %s: will be requested\n", r.Host, r.Owner)
	}
	for _, w := range p.Waiting {
		fmt.Fprintf(&b, "  cert    %s\n", w)
	}
	for _, host := range p.Remove {
		fmt.Fprintf(&b, "  cert    %s: no longer asked for, will be removed\n", host)
	}
	return b.String()
}

// planCerts compares what is asked for with the records the cluster holds.
// inScope says whether a group is this apply's to decide for, so a narrowed
// apply leaves every other group's certificates alone.
func planCerts(want []certRequest, have variables, inScope func(group string) bool) certPlan {
	var p certPlan
	wanted := map[string]bool{}
	for _, w := range want {
		wanted[deploy.CertPath(w.Host)] = true
		switch rec := have[deploy.CertPath(w.Host)]; {
		case rec[deploy.CertChainKey] != "":
		case rec == nil || w.Required:
			// Asked for again when its service cannot start without it:
			// that is what makes the job try now, and the apply is stuck
			// until it does.
			p.Request = append(p.Request, w)
		default:
			// One ingress would present is left to the job's own retries.
			// A name whose DNS has not been moved yet fails every time it is
			// tried, the authority allows a name few failures an hour, and
			// asking again on every apply would spend them all before the
			// DNS is right.
			state := "being issued"
			if e := rec[deploy.CertErrorKey]; e != "" {
				state = "being retried; the last attempt failed: " + e
			}
			p.Waiting = append(p.Waiting, w.Host+": "+state)
		}
	}
	for path, rec := range have {
		group, _, _ := strings.Cut(rec[deploy.CertOwnerKey], "/")
		if !wanted[path] && inScope(group) {
			p.Remove = append(p.Remove, rec[deploy.CertNameKey])
		}
	}
	sort.Strings(p.Remove)
	return p
}

// checkCertsPossible refuses a certificate request the cluster cannot serve,
// before anything is changed.
func checkCertsPossible(cfg Config, p certPlan, current map[string]deploy.JobState, platformInScope bool) error {
	if len(p.Request) == 0 {
		return nil
	}
	first := p.Request[0]
	if !cfg.Ingress.TLS() {
		return fmt.Errorf("%s asks for a certificate (tls: %s), which needs ingress with HTTPS on: "+
			"the certificate authority checks the name on port 80, and ingress is what answers there", first.Owner, first.Host)
	}
	// The certificate job is one of orca's own, so only an apply that
	// includes them deploys it. A cluster last applied by an orca from before
	// there was one has none, and an ingress that does not know to serve
	// what it issues.
	if _, running := current[deploy.JobID(deploy.OrcaApp, "certs")]; !running && !platformInScope {
		return fmt.Errorf("%s needs a certificate, and the cluster's certificate job is not running yet; "+
			"run `orca apply` without a group once to deploy it", first.Host)
	}
	return nil
}

const (
	certWaitTimeout = 4 * time.Minute
	certWaitPoll    = 2 * time.Second
)

// requestCerts asks the certificate job for each certificate and waits for
// all of them, so that a service is never submitted to wait on a certificate
// that is not coming. A failure is the authority's own reason: an error for a
// certificate its service needs to start, and a warning for one ingress would
// present, which the job goes on trying for.
func requestCerts(ctx context.Context, cluster *Cluster, reqs []certRequest) error {
	// The record is the request. Written over any that is there: every one
	// here has no certificate, and replacing one that failed is what tells
	// the job to try again now rather than when its backoff says.
	records := variables{}
	pending := map[string]certRequest{}
	for _, r := range reqs {
		records[deploy.CertPath(r.Host)] = map[string]string{deploy.CertNameKey: r.Host, deploy.CertOwnerKey: r.Owner.String()}
		pending[deploy.CertPath(r.Host)] = r
	}
	if err := cluster.PutVariables(ctx, records); err != nil {
		return err
	}
	fmt.Printf("  cert    waiting for %d certificate(s) ...\n", len(reqs))

	var errs, warns []error
	fail := func(r certRequest, err error) {
		if r.Required {
			errs = append(errs, err)
		} else {
			warns = append(warns, err)
		}
	}
	for deadline := time.Now().Add(certWaitTimeout); len(pending) > 0 && time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(certWaitPoll):
		}
		have, err := cluster.Certificates(ctx)
		if err != nil {
			return err
		}
		for path, r := range pending {
			switch rec := have[path]; {
			case rec[deploy.CertChainKey] != "":
				fmt.Printf("  cert    %s ... ok\n", r.Host)
			case rec[deploy.CertErrorKey] != "":
				fmt.Printf("  cert    %s ... FAILED\n", r.Host)
				fail(r, fmt.Errorf("no certificate for %s: %s", r.Host, rec[deploy.CertErrorKey]))
			default:
				continue
			}
			delete(pending, path)
		}
	}
	for _, r := range pending {
		fail(r, fmt.Errorf("no certificate for %s yet: the certificate job has not answered; "+
			"`orca status orca` shows whether it is running and `orca logs orca/certs` what it is doing", r.Host))
	}

	const hint = "a name has to resolve to the ingress machine, with port 80 reachable from the internet"
	if len(warns) > 0 {
		fmt.Fprintf(os.Stderr, "\nwarning: %v\n\nthe certificate job keeps trying, and `orca status` shows when it has them; "+
			"until then those names are served with a certificate browsers reject. %s\n\n", errors.Join(warns...), hint)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w\n\nnothing that needs them was deployed; %s", errors.Join(errs...), hint)
	}
	return nil
}

// removeCerts deletes the records of certificates nothing asks for, which is
// what stops them being renewed. Not fatal: a record left behind costs a
// renewal, and the next apply tries again.
func removeCerts(ctx context.Context, cluster *Cluster, hosts []string) {
	for _, host := range hosts {
		if err := cluster.deleteVariable(ctx, deploy.CertPath(host)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove the certificate for %s: %v\n", host, err)
			continue
		}
		fmt.Printf("  cert    %s removed\n", host)
	}
}

// printCertificates shows each certificate a service asked for, for the
// groups in want (nil: every group): how long it is good for, or why there is
// none. A renewal that keeps failing shows here weeks before the certificate
// it would replace runs out.
func printCertificates(ctx context.Context, cluster *Cluster, want map[string]bool) {
	certs, err := cluster.Certificates(ctx)
	if err != nil {
		fmt.Printf("\ncould not read the certificates: %v\n", err)
		return
	}
	var lines []string
	for _, rec := range certs {
		group, _, _ := strings.Cut(rec[deploy.CertOwnerKey], "/")
		if want != nil && !want[group] {
			continue
		}
		state := "waiting to be issued"
		if leaf, err := leafCertificate(rec[deploy.CertChainKey]); err == nil {
			state = fmt.Sprintf("expires in %dd", int(time.Until(leaf.NotAfter).Hours()/24))
			if e := rec[deploy.CertErrorKey]; e != "" {
				state += "; renewal being retried, the last attempt failed: " + e
			}
		} else if e := rec[deploy.CertErrorKey]; e != "" {
			state = "being retried; the last attempt failed: " + e
		}
		lines = append(lines, fmt.Sprintf("  %-32s %-24s %s", rec[deploy.CertNameKey], rec[deploy.CertOwnerKey], state))
	}
	if len(lines) == 0 {
		return
	}
	sort.Strings(lines)
	fmt.Println("\ncertificates:")
	fmt.Println(strings.Join(lines, "\n"))
}
