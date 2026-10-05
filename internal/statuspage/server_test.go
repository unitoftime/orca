package statuspage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// The page is one file. Anything it loaded from elsewhere would be a request
// to a third party from behind the admin password, and a page that breaks
// when the machines cannot reach the internet.
func TestPageLoadsNothingFromElsewhere(t *testing.T) {
	srv := httptest.NewServer((&Server{Nomad: "http://127.0.0.1:1"}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type = %q", ct)
	}
	page := string(indexHTML)
	if m := regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?(https?:)?//`).FindString(page); m != "" {
		t.Errorf("the page loads something from elsewhere: %q", m)
	}
	if !strings.Contains(page, `fetch("api/summary"`) {
		t.Error("the page should read the summary relative to where it is served")
	}
	// Names come from the cluster and are only ever set as text.
	if strings.Contains(page, "innerHTML") {
		t.Error("the page must not build markup from strings")
	}
}

// With nothing reachable, the summary still answers, saying what it could not
// ask, rather than failing the page.
func TestSummaryWhenNothingAnswers(t *testing.T) {
	srv := httptest.NewServer((&Server{Nomad: "http://127.0.0.1:1"}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s", resp.Status)
	}
	var s Summary
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	if s.Unavailable["nomad"] == "" || s.Unavailable["metrics"] == "" {
		t.Errorf("unavailable = %v", s.Unavailable)
	}
	if len(s.Problems) != 1 || s.Problems[0].Subject != "nomad" {
		t.Errorf("an unreachable scheduler is the problem; metrics being off is not: %+v", s.Problems)
	}
}

// The other UIs are the page's to link to, as they were passed; none is an
// empty list, not null.
func TestLinks(t *testing.T) {
	get := func(s *Server) []Link {
		t.Helper()
		srv := httptest.NewServer(s.Handler())
		defer srv.Close()
		resp, err := http.Get(srv.URL + "/api/links")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var links []Link
		if err := json.NewDecoder(resp.Body).Decode(&links); err != nil {
			t.Fatal(err)
		}
		if links == nil {
			t.Error("links should be a list, not null")
		}
		return links
	}

	if got := get(&Server{}); len(got) != 0 {
		t.Errorf("no links passed, got %v", got)
	}
	want := []Link{{Name: "metrics", URL: "https://metrics.example.com"}, {Name: "logs", URL: "https://logs.example.com"}}
	if got := get(&Server{Links: want}); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("links = %v, want %v", got, want)
	}
}
