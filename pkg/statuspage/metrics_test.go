package statuspage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The Prometheus API sends values as strings, so NaN and infinities survive
// JSON; a NaN is a missing number, not a zero.
func TestMetricsClientParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/query":
			if r.URL.Query().Get("query") != qUp {
				t.Errorf("query = %q", r.URL.Query().Get("query"))
			}
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"node":"box0"},"value":[1790461033,"1"]},
				{"metric":{"node":"box1"},"value":[1790461033,"NaN"]}]}}`))
		case "/api/v1/query_range":
			w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
				{"metric":{"node":"box0"},"values":[[1790461000,"12.5"],[1790461060,"13"]]}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &metricsClient{base: srv.URL, http: srv.Client()}

	got, err := c.query(context.Background(), qUp)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].labels["node"] != "box0" || got[0].value != 1 {
		t.Errorf("query = %+v", got)
	}

	rng, err := c.queryRange(context.Background(), qCPU, time.Unix(1790461060, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(rng) != 1 || len(rng[0].points) != 2 || rng[0].points[1] != (Point{T: 1790461060, V: 13}) {
		t.Errorf("range = %+v", rng)
	}
}

func TestMetricsClientReportsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"error","error":"parse error"}`))
	}))
	defer srv.Close()
	c := &metricsClient{base: srv.URL, http: srv.Client()}
	if _, err := c.query(context.Background(), "bad("); err == nil {
		t.Error("a failed query should be an error, not an empty result")
	}
}
