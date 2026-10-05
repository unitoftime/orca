package statuspage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/unitoftime/orca/internal/logsql"
)

// LogLine is one line as the log drawer shows it.
type LogLine struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
	Job     string    `json:"job,omitempty"`
	Task    string    `json:"task,omitempty"`
	Stream  string    `json:"stream,omitempty"`
}

// Logs is a read of one service's logs, oldest first.
type Logs struct {
	Lines []LogLine `json:"lines"`

	// Truncated says there were more lines than the limit, and only the
	// newest were kept.
	Truncated bool `json:"truncated"`
}

// The windows the drawer offers. A fixed list rather than any duration: it is
// the whole of what the page can ask the log store for.
var logWindows = map[string]bool{"15m": true, "1h": true, "6h": true, "24h": true, "7d": true}

const (
	defaultLogLimit = 200
	maxLogLimit     = 1000
	logTimeout      = 15 * time.Second
)

// errNotFound is a request for a service the summary does not have.
var errNotFound = errors.New("not found")

type logRequest struct {
	service string // group/name
	grep    string
	since   string
	limit   int
	after   time.Time
}

func parseLogRequest(q url.Values) (logRequest, error) {
	r := logRequest{service: q.Get("service"), grep: strings.TrimSpace(q.Get("q")), since: q.Get("since"), limit: defaultLogLimit}
	if r.service == "" {
		return r, fmt.Errorf("service is required, as group/name")
	}
	if r.since == "" {
		r.since = "1h"
	}
	if !logWindows[r.since] {
		return r, fmt.Errorf("since must be one of 15m, 1h, 6h, 24h, 7d")
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return r, fmt.Errorf("limit must be a positive number")
		}
		r.limit = min(n, maxLogLimit)
	}
	if v := q.Get("after"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return r, fmt.Errorf("after must be an RFC 3339 time")
		}
		r.after = t
	}
	return r, nil
}

// jobsFor maps a service to the jobs whose logs are its logs, from the same
// summary the page is showing: its own job, and a database's backup runs.
// Only services the summary has can be asked for, so the page can never name
// a job, or write a query, of its own.
func jobsFor(sum Summary, service string) ([]logsql.Job, error) {
	group, name, _ := strings.Cut(service, "/")
	for _, s := range sum.Services {
		if s.Group != group || s.Name != name {
			continue
		}
		jobs := []logsql.Job{{ID: s.JobID, Periodic: s.Periodic}}
		if s.Backup != nil && s.Backup.JobID != "" {
			jobs = append(jobs, logsql.Job{ID: s.Backup.JobID, Periodic: true})
		}
		return jobs, nil
	}
	return nil, fmt.Errorf("%w: no service %s", errNotFound, service)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	fail := func(code int, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
	}
	if s.Logs == "" {
		fail(http.StatusServiceUnavailable, errors.New("logs are switched off in cluster.yaml (monitoring.logs)"))
		return
	}
	req, err := parseLogRequest(r.URL.Query())
	if err != nil {
		fail(http.StatusBadRequest, err)
		return
	}
	jobs, err := jobsFor(s.Summary(r.Context()), req.service)
	if err != nil {
		fail(http.StatusNotFound, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), logTimeout)
	defer cancel()
	logs, err := s.queryLogs(ctx, jobs, req)
	if err != nil {
		fail(http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(logs)
}

// queryLogs asks the log store, and puts what it says oldest first: it
// answers a limited read with the newest lines, newest first.
func (s *Server) queryLogs(ctx context.Context, jobs []logsql.Job, req logRequest) (Logs, error) {
	history, _ := logsql.Queries(jobs, req.grep, req.since)

	params := url.Values{"query": {history}, "limit": {strconv.Itoa(req.limit)}}
	if !req.after.IsZero() {
		// Following: only what arrived since the last line the page has.
		params.Set("start", req.after.Format(time.RFC3339Nano))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Logs+"/select/logsql/query?"+params.Encode(), nil)
	if err != nil {
		return Logs{}, err
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return Logs{}, fmt.Errorf("the log store: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Logs{}, fmt.Errorf("the log store: %s", resp.Status)
	}

	out := Logs{Lines: []LogLine{}}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var l logsql.Line
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, l.Time)
		if err != nil {
			continue
		}
		// start is inclusive, and the page already has the line it passed.
		if !req.after.IsZero() && !t.After(req.after) {
			continue
		}
		out.Lines = append(out.Lines, LogLine{Time: t, Message: l.Message, Job: l.Job, Task: l.Task, Stream: l.Stream})
	}
	if err := sc.Err(); err != nil {
		return Logs{}, fmt.Errorf("the log store: %w", err)
	}

	sort.SliceStable(out.Lines, func(i, j int) bool { return out.Lines[i].Time.Before(out.Lines[j].Time) })
	out.Truncated = len(out.Lines) >= req.limit
	return out, nil
}
