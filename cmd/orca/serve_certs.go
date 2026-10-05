package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/internal/deploy"
	"golang.org/x/crypto/acme"
)

// The certificate job is orca itself, run on the machine as `orca
// serve-certs`. It is the one thing in the cluster that talks to the
// certificate authority.
//
// It holds no state of its own. What it has to do is the certificate records
// in the variable store (see deploy.CertPrefix): one with no certificate is a
// request, and one whose certificate is two thirds through its life is due
// for renewal. What it produces goes back into the same record, where Nomad
// delivers it to whichever machine runs the service. So it can be stopped,
// moved or restarted at any point and picks up where the records say.

// cmdServeCerts runs the certificate job until ctx ends.
func cmdServeCerts(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve-certs", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8081", "address to answer ownership checks on")
	nomadAddr := fs.String("nomad", fmt.Sprintf("http://127.0.0.1:%d", deploy.NomadHTTPPort), "Nomad's API")
	ingress := fs.String("ingress", "http://127.0.0.1:80", "where ingress answers on this machine")
	directory := fs.String("directory", acme.LetsEncryptURL, "the certificate authority's ACME directory")
	email := fs.String("email", "", "optional contact on the account")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// The token is the job's own identity, from its environment: see
	// deploy.ACLPolicies for what it is allowed.
	cfg := nomad.DefaultConfig()
	cfg.Address = *nomadAddr
	cfg.HttpClient = &http.Client{Timeout: 15 * time.Second}
	api, err := nomad.NewClient(cfg)
	if err != nil {
		return err
	}
	store := varStore{api: api}
	issuer := &acmeIssuer{store: store, directory: *directory, email: *email, ingress: *ingress}
	certs := &certReconciler{store: store, issue: issuer.issue, authority: *directory, state: map[string]*certAttempts{}}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           challengeHandler(store),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	go certs.run(ctx)

	fmt.Printf("serving on %s (nomad %s, authority %s)\n", *listen, *nomadAddr, *directory)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// acmeToken is what a challenge token may contain: base64url. Checked because
// the token arrives in a URL from the internet and goes into a variable path.
var acmeToken = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

// challengeHandler answers a certificate authority's ownership check: the
// token in the URL, looked up in the store. From the store and not from
// memory, so that whichever copy of this job is asked can answer for a
// challenge another one started.
func challengeHandler(store varStore) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc(deploy.ACMEChallengePath, func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.URL.Path, deploy.ACMEChallengePath)
		if !acmeToken.MatchString(token) {
			http.NotFound(w, r)
			return
		}
		v, ok, err := store.get(r.Context(), deploy.ACMETokenPrefix+"/"+token)
		if err != nil {
			http.Error(w, "the variable store did not answer", http.StatusServiceUnavailable)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, v.Items[deploy.ACMETokenKey])
	})
	return mux
}

// certReconciler makes the store's certificate records true: it issues the
// ones that have no certificate and renews the ones that are due.
type certReconciler struct {
	store varStore
	issue func(ctx context.Context, host string) (chain, key []byte, err error)

	// authority is where issue gets certificates from.
	authority string

	// state is what is remembered between passes about a record that failed:
	// only when to try again, which a restart may safely forget.
	state map[string]*certAttempts
}

type certAttempts struct {
	failed  int
	nextTry time.Time
}

const (
	certPollInterval = 5 * time.Second
	certIssueTimeout = 3 * time.Minute

	// The authority allows five failed checks an hour for a name, so a retry
	// that is itself failing must not be what uses them up. It levels off
	// under that rather than backing away for hours: the usual failure is a
	// name whose DNS has not been pointed here yet, and the certificate
	// should follow soon after it is.
	certRetryBase = 5 * time.Minute
	certRetryMax  = 15 * time.Minute

	// How long ingress is given to pass a check on, and how soon one it did
	// not pass is tried again. Long enough to sit out ingress restarting,
	// which is what an upgrade of orca does to it.
	certRouteWait  = 90 * time.Second
	certRouteRetry = 30 * time.Second
)

func (c *certReconciler) run(ctx context.Context) {
	tick := time.NewTicker(certPollInterval)
	defer tick.Stop()
	for {
		if err := c.reconcile(ctx, time.Now()); err != nil && ctx.Err() == nil {
			fmt.Printf("reading certificate records: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// reconcile is one pass over every record.
func (c *certReconciler) reconcile(ctx context.Context, now time.Time) error {
	records, err := c.store.list(ctx, deploy.CertPrefix+"/")
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, path := range records {
		seen[path] = true
		rec, ok, err := c.store.get(ctx, path)
		if err != nil || !ok {
			continue
		}

		// A record with no error on it is either fresh or was asked for
		// again, and both mean now: apply is waiting on it.
		st := c.state[path]
		if rec.Items[deploy.CertErrorKey] == "" {
			st = nil
			delete(c.state, path)
		} else if st == nil {
			// Failed before a restart. Not known how often, so start over,
			// and soon if the failure was one the authority never saw.
			st = &certAttempts{failed: 1, nextTry: now.Add(certRetryBase)}
			if strings.Contains(rec.Items[deploy.CertErrorKey], errNotRouted.Error()) {
				st = &certAttempts{nextTry: now.Add(certRouteRetry)}
			}
			c.state[path] = st
		}
		// One with no authority recorded was put there by an import, and is
		// taken as it is.
		from := rec.Items[deploy.CertAuthorityKey]
		due := certDue(rec.Items[deploy.CertChainKey], now) || (from != "" && from != c.authority)
		if !due || (st != nil && now.Before(st.nextTry)) {
			continue
		}
		c.attempt(ctx, rec, now)
	}
	for path := range c.state {
		if !seen[path] {
			delete(c.state, path)
		}
	}
	return nil
}

// attempt issues one record's certificate and writes the outcome back to it.
func (c *certReconciler) attempt(ctx context.Context, rec nomad.Variable, now time.Time) {
	host := rec.Items[deploy.CertNameKey]
	fmt.Printf("%s: requesting a certificate\n", host)

	issueCtx, cancel := context.WithTimeout(ctx, certIssueTimeout)
	chain, key, err := c.issue(issueCtx, host)
	cancel()

	update := func(items map[string]string) {
		if err != nil {
			items[deploy.CertErrorKey] = err.Error()
			return
		}
		items[deploy.CertChainKey], items[deploy.CertKeyKey] = string(chain), string(key)
		items[deploy.CertAuthorityKey] = c.authority
		delete(items, deploy.CertErrorKey)
	}
	if err != nil {
		st := c.state[rec.Path]
		if st == nil {
			st = &certAttempts{}
			c.state[rec.Path] = st
		}
		if errors.Is(err, errNotRouted) {
			// The authority was never asked, so nothing of its allowance
			// was spent: ingress is usually just restarting.
			st.nextTry = now.Add(certRouteRetry)
		} else {
			st.failed++
			st.nextTry = now.Add(min(certRetryBase<<min(st.failed-1, 8), certRetryMax))
		}
		fmt.Printf("%s: failed, trying again at %s: %v\n", host, st.nextTry.Format(time.RFC3339), err)
	} else {
		delete(c.state, rec.Path)
		fmt.Printf("%s: certificate issued\n", host)
	}

	// Written only if the record is still the one that was read. If apply
	// replaced it meanwhile the outcome still belongs on it, so it is read
	// again and written once more; a record that is gone is left gone.
	for range 2 {
		update(rec.Items)
		written, werr := c.store.putChecked(ctx, rec)
		if werr != nil {
			fmt.Printf("%s: storing the outcome: %v\n", host, werr)
			return
		}
		if written {
			return
		}
		var ok bool
		if rec, ok, werr = c.store.get(ctx, rec.Path); werr != nil || !ok {
			return
		}
	}
}

// certDue reports whether a certificate needs issuing: there is none, it does
// not parse, or it is two thirds of the way from issue to expiry. That leaves
// a third of its life for a failing renewal to be noticed and fixed while the
// old certificate still works.
func certDue(chain string, now time.Time) bool {
	leaf, err := leafCertificate(chain)
	if err != nil {
		return true
	}
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	return now.After(leaf.NotBefore.Add(life * 2 / 3))
}

// leafCertificate is the first certificate of a PEM chain: the one for the
// hostname itself.
func leafCertificate(chain string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(chain))
	if block == nil {
		return nil, errors.New("no certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

// acmeIssuer gets certificates from an ACME certificate authority, proving
// each name over HTTP.
type acmeIssuer struct {
	store     varStore
	directory string
	email     string
	ingress   string

	client *acme.Client // registered; nil until first used
}

// issue gets a certificate for one hostname.
//
// The authority hands out a token and fetches its answer from port 80 of
// whatever the hostname resolves to. Ingress holds that port and forwards the
// fetch here, where challengeHandler answers it from the store.
func (a *acmeIssuer) issue(ctx context.Context, host string) (chain, key []byte, err error) {
	client, err := a.account(ctx)
	if err != nil {
		return nil, nil, err
	}

	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(host))
	if err != nil {
		return nil, nil, fmt.Errorf("order: %w", err)
	}
	for _, url := range order.AuthzURLs {
		if err := a.authorize(ctx, client, host, url); err != nil {
			return nil, nil, err
		}
	}
	if order, err = client.WaitOrder(ctx, order.URI); err != nil {
		return nil, nil, fmt.Errorf("order: %w", err)
	}

	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{host}}, certKey)
	if err != nil {
		return nil, nil, err
	}
	ders, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, nil, fmt.Errorf("issue: %w", err)
	}

	var out bytes.Buffer
	for _, der := range ders {
		pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	keyPEM, err := encodeECKey(certKey)
	if err != nil {
		return nil, nil, err
	}
	return out.Bytes(), keyPEM, nil
}

// authorize proves one name to the authority.
func (a *acmeIssuer) authorize(ctx context.Context, client *acme.Client, host, url string) error {
	authz, err := client.GetAuthorization(ctx, url)
	if err != nil {
		return fmt.Errorf("authorization: %w", err)
	}
	if authz.Status == acme.StatusValid {
		return nil
	}
	var challenge *acme.Challenge
	for _, c := range authz.Challenges {
		if c.Type == "http-01" {
			challenge = c
		}
	}
	if challenge == nil {
		return fmt.Errorf("the authority offers no HTTP check for %s", host)
	}
	answer, err := client.HTTP01ChallengeResponse(challenge.Token)
	if err != nil {
		return err
	}

	tokenPath := deploy.ACMETokenPrefix + "/" + challenge.Token
	if err := a.store.put(ctx, tokenPath, map[string]string{deploy.ACMETokenKey: answer}); err != nil {
		return err
	}
	// Not ctx: the token is removed even when the attempt was canceled.
	defer a.store.delete(context.WithoutCancel(ctx), tokenPath)

	// Checked from here before the authority is asked to. A failed check of
	// theirs counts against a small hourly allowance, and theirs cannot say
	// which hop was missing; this one can.
	if err := a.checkRoute(ctx, host, challenge.Token, answer); err != nil {
		return err
	}
	if _, err := client.Accept(ctx, challenge); err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	if _, err := client.WaitAuthorization(ctx, authz.URI); err != nil {
		return err
	}
	return nil
}

// checkRoute fetches the answer the way the authority will, through ingress
// on this machine, until it comes back right. Ingress learns of a new name
// from the store a moment after its record is created, hence the waiting.
func (a *acmeIssuer) checkRoute(ctx context.Context, host, token, answer string) error {
	client := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	last := "no answer"
	for deadline := time.Now().Add(certRouteWait); time.Now().Before(deadline); {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.ingress+deploy.ACMEChallengePath+token, nil)
		if err != nil {
			return err
		}
		req.Host = host
		if resp, err := client.Do(req); err != nil {
			last = err.Error()
		} else {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && string(body) == answer {
				return nil
			}
			last = "HTTP " + resp.Status
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("%w for %s (%s)", errNotRouted, host, last)
}

// errNotRouted is a check that never reached the authority, because ingress
// did not pass it on from this machine in the first place.
var errNotRouted = errors.New("ingress is not passing the ownership check on to the certificate job")

// account is the client for the cluster's account with the authority,
// registered on first use. Its key is made once and kept in the store, so
// every certificate the cluster ever asks for comes from one account.
func (a *acmeIssuer) account(ctx context.Context) (*acme.Client, error) {
	if a.client != nil {
		return a.client, nil
	}

	v, ok, err := a.store.get(ctx, deploy.ACMEAccountPath)
	if err != nil {
		return nil, err
	}
	if !ok {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		keyPEM, err := encodeECKey(key)
		if err != nil {
			return nil, err
		}
		// Created, never replaced: another copy may have made one a moment
		// ago, and that one is then the account's.
		v = nomad.Variable{Path: deploy.ACMEAccountPath, Items: map[string]string{deploy.ACMEAccountKey: string(keyPEM)}}
		if _, err := a.store.putChecked(ctx, v); err != nil {
			return nil, err
		}
		if v, _, err = a.store.get(ctx, deploy.ACMEAccountPath); err != nil {
			return nil, err
		}
	}
	block, _ := pem.Decode([]byte(v.Items[deploy.ACMEAccountKey]))
	if block == nil {
		return nil, fmt.Errorf("the account key in %s is not PEM", deploy.ACMEAccountPath)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the account key in %s: %w", deploy.ACMEAccountPath, err)
	}

	version, _ := buildVersion()
	client := &acme.Client{Key: key, DirectoryURL: a.directory, UserAgent: "orca/" + version}
	account := &acme.Account{}
	if a.email != "" {
		account.Contact = []string{"mailto:" + a.email}
	}
	if _, err := client.Register(ctx, account, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, fmt.Errorf("register with %s: %w", a.directory, err)
	}
	a.client = client
	return client, nil
}

func encodeECKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}
