package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/unitoftime/orca/internal/deploy"
	"github.com/unitoftime/orca/internal/statuspage"
)

// rememberInterval is how often the status job looks for scheduled jobs'
// finished runs to write down. Nomad keeps one for hours, so this only has to
// be far inside that.
const rememberInterval = time.Minute

// rememberRuns keeps the last run of each scheduled job on record, for as
// long as the status job runs. It is here because this is the job that is
// always up with a view of Nomad, not because the page needs it: `orca
// status` reads the same records.
//
// A failure is logged and tried again at the next interval. The page goes on
// serving either way.
func rememberRuns(ctx context.Context, nomadAddr string) {
	cfg := nomad.DefaultConfig()
	cfg.Address = nomadAddr
	client, err := nomad.NewClient(cfg)
	if err != nil {
		fmt.Printf("not remembering scheduled jobs' runs: %v\n", err)
		return
	}

	tick := time.NewTicker(rememberInterval)
	defer tick.Stop()
	for {
		attempt, cancel := context.WithTimeout(ctx, rememberInterval/2)
		if err := deploy.RememberRuns(attempt, client); err != nil && ctx.Err() == nil {
			fmt.Printf("remember scheduled jobs' runs: %v\n", err)
		}
		cancel()

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// cmdServeStatus runs the status page. It is what the status job runs on the
// machine, not something you run yourself, so it is left out of the usage
// text, and it reads no cluster.yaml: everything it needs comes from its flags
// and from the environment the job renders.
func cmdServeStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve-status", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8080", "address to serve on")
	nomadAddr := fs.String("nomad", fmt.Sprintf("http://127.0.0.1:%d", deploy.NomadHTTPPort), "Nomad's API")
	var links []statuspage.Link
	fs.Func("link", "another UI to link to, as name=url; repeatable", func(v string) error {
		name, url, ok := strings.Cut(v, "=")
		if !ok || name == "" || url == "" {
			return fmt.Errorf("want name=url, got %q", v)
		}
		links = append(links, statuspage.Link{Name: name, URL: url})
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return err
	}

	s := &statuspage.Server{
		Nomad:   *nomadAddr,
		Metrics: os.Getenv(deploy.StatusEnvMetrics),
		Logs:    os.Getenv(deploy.StatusEnvLogs),
		Links:   links,
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	go rememberRuns(ctx, *nomadAddr)

	fmt.Printf("serving on %s (nomad %s, metrics %q, logs %q)\n", *listen, s.Nomad, s.Metrics, s.Logs)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
