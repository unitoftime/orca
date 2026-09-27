// Package statuspage is the status page orca runs on the cluster: every
// machine and every service at a glance, and the API `orca top` reads over
// SSH.
//
// It runs as `orca serve-status`, from the same binary as the CLI, so the page
// and the terminal are one implementation and cannot disagree.
package statuspage

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	nomad "github.com/hashicorp/nomad/api"
)

//go:embed index.html
var indexHTML []byte

// Server answers the page and its API. Every address is a base URL; an empty
// store address means that store is switched off, and the page says so rather
// than showing an empty section.
type Server struct {
	Nomad   string
	Metrics string
	Logs    string

	// Links are the cluster's other web UIs, shown in the page's header.
	Links []Link

	once    sync.Once
	nomad   *nomad.Client
	metrics *metricsClient
	initErr error

	// The last summary, reused for a few seconds: every open page asks every
	// fifteen, and the answer does not change faster than the metric store
	// scrapes. Held under mu while it is rebuilt, so a burst of requests
	// costs one collection.
	mu     sync.Mutex
	cached *Summary
}

// Link is another web UI the page links to: Nomad's, and the stores' own.
type Link struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// cacheFor is how long a summary is served before it is collected again.
const cacheFor = 5 * time.Second

// collectTimeout bounds one collection. A store that hangs is reported
// unavailable rather than hanging the page with it.
const collectTimeout = 10 * time.Second

// errMetricsOff is what the page shows when metrics are switched off.
var errMetricsOff = errors.New("metrics are switched off in cluster.yaml (monitoring.metrics)")

// Handler routes the page and its API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	// Healthy means serving. It deliberately does not ask Nomad or the
	// stores: the page reports their being down, and a check that failed with
	// them would restart the one thing that can tell you so.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /api/links", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(append([]Link{}, s.Links...))
	})
	mux.HandleFunc("GET /api/summary", func(w http.ResponseWriter, r *http.Request) {
		sum := s.Summary(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(sum)
	})
	return mux
}

// Summary collects, or returns the one collected moments ago.
func (s *Server) Summary(ctx context.Context) Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil && time.Since(s.cached.Time) < cacheFor {
		return *s.cached
	}
	sum := s.collect(ctx)
	s.cached = &sum
	return sum
}

func (s *Server) init() error {
	s.once.Do(func() {
		cfg := nomad.DefaultConfig()
		cfg.Address = s.Nomad
		s.nomad, s.initErr = nomad.NewClient(cfg)
		if s.Metrics != "" {
			s.metrics = &metricsClient{base: s.Metrics, http: &http.Client{Timeout: collectTimeout}}
		}
	})
	return s.initErr
}

func (s *Server) collect(ctx context.Context) Summary {
	now := time.Now()
	if err := s.init(); err != nil {
		return build(now, nil, err, nil, err)
	}
	// Not the request's context: a collection is shared by every request
	// waiting on it, and one closed tab should not cancel it for the rest.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), collectTimeout)
	defer cancel()

	var (
		nv         *nomadView
		mv         *metricsView
		nErr, mErr error
		wg         sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		nv, nErr = fetchNomad(ctx, s.nomad)
	}()
	go func() {
		defer wg.Done()
		if s.metrics == nil {
			mErr = errMetricsOff
			return
		}
		mv, mErr = fetchMetrics(ctx, s.metrics, now)
	}()
	wg.Wait()
	return build(now, nv, nErr, mv, mErr)
}
