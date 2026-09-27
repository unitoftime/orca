package statuspage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A server whose summary is already collected, so a log read is resolved
// against it without a Nomad to ask.
func logServer(t *testing.T, store *httptest.Server) *httptest.Server {
	t.Helper()
	s := &Server{Nomad: "http://127.0.0.1:1"}
	if store != nil {
		s.Logs = store.URL
	}
	s.cached = &Summary{Time: time.Now(), Services: []Service{
		{Group: "shop", Name: "db", JobID: "shop-db", Backup: &Backup{JobID: "shop-db-backup", Health: "running"}},
		{Group: "shop", Name: "web", JobID: "shop-web"},
	}}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func getLogs(t *testing.T, srv *httptest.Server, query string) (int, Logs, string) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/logs?" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	json.NewDecoder(resp.Body).Decode(&raw)
	var logs Logs
	json.Unmarshal(raw, &logs)
	return resp.StatusCode, logs, string(raw)
}

func TestLogsReadsAServicesJobs(t *testing.T) {
	var asked string
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query().Get("query")
		// Newest first, as the store answers a limited read.
		w.Write([]byte(`{"_time":"2026-09-26T12:00:02.5Z","_msg":"backup done","job":"shop-db-backup/periodic-1","task":"upload"}
{"_time":"2026-09-26T12:00:01Z","_msg":"ready","job":"shop-db","task":"db","stream":"stderr"}
not a log line
`))
	}))
	defer store.Close()
	srv := logServer(t, store)

	code, logs, raw := getLogs(t, srv, "service=shop/db&q=ready&since=6h")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	// The database and its backup runs, the way `orca logs shop/db` reads it.
	want := `_time:6h (job:"shop-db" OR job:"shop-db-backup/periodic-"*) "ready"`
	if asked != want {
		t.Errorf("query = %s\nwant    %s", asked, want)
	}
	if len(logs.Lines) != 2 || logs.Lines[0].Message != "ready" || logs.Lines[1].Task != "upload" {
		t.Errorf("want oldest first, unparseable lines skipped: %+v", logs.Lines)
	}
	if logs.Lines[0].Stream != "stderr" {
		t.Errorf("stream = %q", logs.Lines[0].Stream)
	}
}

// Following asks only for what is new, and never repeats the line the page
// already has: the store's start is inclusive.
func TestLogsAfter(t *testing.T) {
	var start string
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start = r.URL.Query().Get("start")
		w.Write([]byte(`{"_time":"2026-09-26T12:00:05Z","_msg":"new","job":"shop-web"}
{"_time":"2026-09-26T12:00:01Z","_msg":"already shown","job":"shop-web"}
`))
	}))
	defer store.Close()
	srv := logServer(t, store)

	_, logs, raw := getLogs(t, srv, "service=shop/web&after=2026-09-26T12:00:01Z")
	if start != "2026-09-26T12:00:01Z" {
		t.Errorf("start = %q", start)
	}
	if len(logs.Lines) != 1 || logs.Lines[0].Message != "new" {
		t.Errorf("lines = %s", raw)
	}
}

func TestLogsRefuses(t *testing.T) {
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the store should not be asked: %s", r.URL)
	}))
	defer store.Close()
	srv := logServer(t, store)

	for query, want := range map[string]int{
		"":                                 http.StatusBadRequest,
		"service=shop/web&since=1y":        http.StatusBadRequest,
		"service=shop/web&limit=-3":        http.StatusBadRequest,
		"service=shop/web&after=yesterday": http.StatusBadRequest,
		"service=shop/nope":                http.StatusNotFound,
		`service=shop/web")%20OR%20(job:*`: http.StatusNotFound,
	} {
		if code, _, raw := getLogs(t, srv, query); code != want {
			t.Errorf("%q: status %d, want %d: %s", query, code, want, raw)
		}
	}

	if code, _, raw := getLogs(t, logServer(t, nil), "service=shop/web"); code != http.StatusServiceUnavailable || !strings.Contains(raw, "switched off") {
		t.Errorf("logs off: %d %s", code, raw)
	}
}

func TestStoresAreJudged(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	mv := &metricsView{Stores: map[string]*storeMetrics{
		"logs":    {Used: f(9.8 * gib), Cap: f(10 * gib), Free: f(40 * gib), ReadOnly: f(0)},
		"metrics": {Used: f(1 * gib), Free: f(3 * gib), MinFree: f(2 * gib), ReadOnly: f(0)},
	}}
	stores := buildStores(mv)
	if len(stores) != 2 {
		t.Fatalf("stores = %+v", stores)
	}
	logs, metrics := stores[0], stores[1]
	// At its cap the log store drops its oldest days: the cap working.
	if logs.Level != LevelOK || logs.Status != "at its cap" || !strings.Contains(logs.Note, "oldest logs are dropped") {
		t.Errorf("logs = %+v", logs)
	}
	if metrics.Level != LevelWarn || metrics.Status != "near its floor" || !strings.Contains(metrics.Note, "stops storing metrics below 2.0G") {
		t.Errorf("metrics = %+v", metrics)
	}

	mv.Stores["metrics"].ReadOnly = f(1)
	mv.Stores["logs"].ReadOnly = f(1)
	for _, st := range buildStores(mv) {
		if st.Level != LevelCrit || st.Status != "not accepting data" {
			t.Errorf("a store that stopped taking data is critical: %+v", st)
		}
	}

	// A store that is switched off reports nothing, and is not shown.
	delete(mv.Stores, "logs")
	if got := buildStores(mv); len(got) != 1 || got[0].Name != "metrics" {
		t.Errorf("stores = %+v", got)
	}
}
