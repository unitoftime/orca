package main

import (
	"strings"
	"testing"
)

// Flags must be recognized wherever they appear. Stopping at the first
// positional argument would fold the rest of the line into the search, so
// `orca logs worker tick -n 3` would search for the literal "tick -n 3" and
// silently find nothing, which is indistinguishable from a service that said
// nothing.
// parseLogArgs reads `orca logs <args>` the way main does.
func parseLogArgs(args []string) (logOptions, error) {
	line, err := parseCommandLine(append([]string{"logs"}, args...))
	if err != nil {
		return logOptions{}, err
	}
	return logOptionsFrom(line.in)
}

func TestParseLogArgsFlagsAfterPositional(t *testing.T) {
	opts, err := parseLogArgs([]string{"worker", "tick", "-n", "3"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.target != "worker" {
		t.Errorf("target = %q, want worker", opts.target)
	}
	if opts.grep != "tick" {
		t.Errorf("grep = %q, want tick", opts.grep)
	}
	if opts.limit != 3 {
		t.Errorf("limit = %d, want 3", opts.limit)
	}
}

func TestParseLogArgs(t *testing.T) {
	opts, err := parseLogArgs([]string{"-f", "blog/api", "--since", "30m", "connection", "refused"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.follow || opts.target != "blog/api" || opts.since != "30m" {
		t.Errorf("opts = %+v", opts)
	}
	// Several words are one search, because looking for a phrase is the
	// common case and writing a query language is not.
	if opts.grep != "connection refused" {
		t.Errorf("grep = %q, want %q", opts.grep, "connection refused")
	}
}

func TestParseLogArgsDefaults(t *testing.T) {
	opts, err := parseLogArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.since == "" || opts.limit < 1 {
		t.Errorf("defaults should bound the query, got %+v", opts)
	}
}

func TestParseLogArgsErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--since"},
		{"-n"},
		{"-n", "zero"},
		{"-n", "0"},
		{"--nope"},
	} {
		if _, err := parseLogArgs(args); err == nil {
			t.Errorf("parseLogArgs(%v) should be an error", args)
		}
	}
}

func TestLogsQueryURLEncodes(t *testing.T) {
	got := logsQueryURL("/select/logsql/query", `_time:1h job:"demo-worker" "a b"`, 50)
	if !strings.HasPrefix(got, "/select/logsql/query?") {
		t.Fatalf("url = %q", got)
	}
	// Quotes and spaces must survive as encoded characters, or the store sees
	// a different query than the one that was built.
	if strings.Contains(got, `"`) || strings.Contains(got, " ") {
		t.Errorf("query was not encoded: %q", got)
	}
	if !strings.Contains(got, "limit=50") {
		t.Errorf("limit missing: %q", got)
	}
}

func TestLogsQueryURLOmitsLimitWhenTailing(t *testing.T) {
	if got := logsQueryURL("/select/logsql/tail", "*", 0); strings.Contains(got, "limit") {
		t.Errorf("a tail has no limit: %q", got)
	}
}

// logCluster is a cluster directory with one group holding one service, which
// is all buildLogQuery needs to resolve a target.
func logCluster(t *testing.T) Config {
	t.Helper()
	root := writeTree(t, map[string]string{
		"cluster.yaml":     "nodes:\n  - host: root@203.0.113.10\n",
		"demo/worker.yaml": "{name: worker, image: alpine:3.20}",
	})
	return Config{Root: root, Nodes: []NodeConfig{{Host: "root@203.0.113.10", Name: "box0", Role: roleServer}}}
}

// VictoriaLogs' live tailing endpoint refuses a query carrying _time, and
// refuses it by streaming nothing at all with a 200, so `orca logs -f` on a
// service logging every 20 seconds would print nothing, forever. Verified
// against a real store: the same query tails fine with the filter removed.
func TestTailQueryHasNoTimeFilter(t *testing.T) {
	cfg := logCluster(t)

	opts, err := parseLogArgs([]string{"worker", "-f"})
	if err != nil {
		t.Fatal(err)
	}
	history, tail, err := buildLogQuery(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(tail, "_time") {
		t.Errorf("tail query must not carry a time filter, got %q", tail)
	}
	// The default since still bounds the history, or following a service would
	// replay everything the store has kept.
	if !strings.Contains(history, "_time:1h") {
		t.Errorf("history query should be bounded by --since, got %q", history)
	}
	// Everything else has to survive in both, or -f would follow the wrong
	// service.
	for _, q := range []string{history, tail} {
		if !strings.Contains(q, `job:"demo-worker"`) {
			t.Errorf("query %q lost its target", q)
		}
	}
}

// A backed-up database's logs include its backup runs, which are periodic
// children named <id>/periodic-<time>. Naming the database has to reach them
// too, or a failing backup is the one job whose output cannot be shown.
func TestLogTargetIncludesBackupRuns(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cluster.yaml":         "nodes:\n  - host: root@203.0.113.10\n",
		"shop/db.yaml":         "{name: db, template: postgres:17, volume: 1G, backup: {to: storage/offsite}}",
		"storage/offsite.yaml": "{name: offsite, target: s3, endpoint: https://x, bucket: b}",
	})
	cfg := Config{Root: root, Nodes: []NodeConfig{{Host: "root@203.0.113.10", Name: "box0", Role: roleServer}}}

	_, tail, err := buildLogQuery(cfg, logOptions{target: "shop/db"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`job:"shop-db"`, `job:"shop-db-backup/periodic-"*`} {
		if !strings.Contains(tail, want) {
			t.Errorf("query %q is missing %s", tail, want)
		}
	}
	if _, _, err := buildLogQuery(cfg, logOptions{target: "storage/offsite"}); err == nil {
		t.Error("a target runs nothing and has no logs; naming it should say what does")
	}
}

func TestLogQueryKeepsSearchInBothForms(t *testing.T) {
	cfg := logCluster(t)

	opts, err := parseLogArgs([]string{"worker", "tick", "--since", "30m"})
	if err != nil {
		t.Fatal(err)
	}
	history, tail, err := buildLogQuery(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(history, "_time:30m") {
		t.Errorf("history = %q, want the requested window", history)
	}
	for _, q := range []string{history, tail} {
		if !strings.Contains(q, `"tick"`) {
			t.Errorf("query %q lost the search term", q)
		}
	}
}

// An empty LogsQL query matches nothing rather than everything, so an
// unfiltered tail has to say "everything" out loud.
func TestUnfilteredTailIsNotEmpty(t *testing.T) {
	cfg := logCluster(t)

	opts, err := parseLogArgs([]string{"-f"})
	if err != nil {
		t.Fatal(err)
	}
	opts.since = "" // no target, no search, no window
	history, tail, err := buildLogQuery(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if history != "*" || tail != "*" {
		t.Errorf("history = %q, tail = %q, want * for both", history, tail)
	}
}

func TestLogTargetTypoNamesWhatExists(t *testing.T) {
	cfg := logCluster(t)

	opts, err := parseLogArgs([]string{"wroker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := buildLogQuery(cfg, opts); err == nil {
		t.Fatal("a mistyped target should fail rather than return no lines")
	} else if !strings.Contains(err.Error(), "demo/worker") {
		t.Errorf("the error should name what exists, got %v", err)
	}
}

// History and the live tail overlap by however long the history read took, so
// the last line arrives twice. Both are correct; printed together they look
// like the service logged the same thing twice.
func TestBoundaryDropsTheOverlap(t *testing.T) {
	line := func(ts, msg string) []byte {
		return []byte(`{"_time":"` + ts + `","_msg":"` + msg + `","job":"demo-worker","task":"worker"}`)
	}

	var b boundary
	var printed []string
	capture := func(raw []byte) { printed = append(printed, string(raw)) }

	// Stand in for printLogLine so the test asserts on what would be shown.
	record := func(raw []byte) {
		if k := b.key(raw); k != "" {
			if b.printed == nil {
				b.printed = map[string]bool{}
			}
			b.printed[k] = true
		}
		capture(raw)
	}
	recordNew := func(raw []byte) {
		if k := b.key(raw); k != "" && b.printed[k] {
			delete(b.printed, k)
			return
		}
		capture(raw)
	}

	record(line("2026-09-25T18:09:36Z", "tick a"))
	record(line("2026-09-25T18:09:56Z", "tick b"))

	// The tail replays the last history line, then streams a new one.
	recordNew(line("2026-09-25T18:09:56Z", "tick b"))
	recordNew(line("2026-09-25T18:10:16Z", "tick c"))

	if len(printed) != 3 {
		t.Fatalf("printed %d lines, want 3 with the overlap dropped:\n%s",
			len(printed), strings.Join(printed, "\n"))
	}

	// A genuine repeat later still shows: the suppression is spent, not sticky.
	recordNew(line("2026-09-25T18:09:56Z", "tick b"))
	if len(printed) != 4 {
		t.Error("a line repeated after the boundary should still be printed")
	}
}
