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

	"github.com/unitoftime/orca/internal/deploy"
	"github.com/unitoftime/orca/internal/statuspage"
)

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

	fmt.Printf("serving on %s (nomad %s, metrics %q, logs %q)\n", *listen, s.Nomad, s.Metrics, s.Logs)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
