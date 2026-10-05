package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/unitoftime/orca/internal/deploy"
	"github.com/unitoftime/orca/internal/logsql"
	"github.com/unitoftime/orca/internal/manifest"
)

// LogLine is one entry as the log store returns it.
type LogLine = logsql.Line

// cmdLogs queries the log store.
//
// It reads from VictoriaLogs rather than from Docker, so logs outlive the
// container that wrote them: a service that crashed, was redeployed, or moved
// to another machine still has its history. It also goes over SSH rather than
// through ingress, so reading logs does not depend on the front door being up,
// and a broken front door is exactly when you want them.
func cmdLogs(ctx context.Context, cfg Config, in invocation) error {
	opts, err := logOptionsFrom(in)
	if err != nil {
		return err
	}

	cluster, err := clusterFor(ctx, cfg)
	if err != nil {
		return err
	}

	history, tail, err := buildLogQuery(cfg, opts)
	if err != nil {
		return err
	}

	if opts.follow {
		// The history first, then the stream. Following a service you just
		// deployed is the common case, and the interesting lines (the ones
		// explaining why it is not running) were written before you asked.
		//
		// The two reads overlap by however long the first took, so the last
		// line or two would otherwise arrive twice.
		seen := &boundary{}
		if err := cluster.QueryLogs(ctx, history, opts.limit, seen.print); err != nil {
			return err
		}
		return cluster.TailLogs(ctx, tail, seen.printNew)
	}
	return cluster.QueryLogs(ctx, history, opts.limit, printLogLine)
}

// boundary suppresses the lines a live tail repeats from the history printed
// just before it.
//
// A repeat is dropped once and then forgotten, so a service that really does
// log the same message again still shows it; only the overlap is removed.
type boundary struct {
	printed map[string]bool
}

func (b *boundary) key(raw []byte) string {
	var l LogLine
	if err := json.Unmarshal(raw, &l); err != nil {
		return ""
	}
	return l.Time + "\x00" + l.Message
}

func (b *boundary) print(raw []byte) {
	if k := b.key(raw); k != "" {
		if b.printed == nil {
			b.printed = map[string]bool{}
		}
		b.printed[k] = true
	}
	printLogLine(raw)
}

func (b *boundary) printNew(raw []byte) {
	if k := b.key(raw); k != "" && b.printed[k] {
		delete(b.printed, k)
		return
	}
	printLogLine(raw)
}

type logOptions struct {
	target string
	since  string
	grep   string
	limit  int
	follow bool
}

// logOptionsFrom reads what `orca logs` was asked for.
func logOptionsFrom(in invocation) (logOptions, error) {
	opts := logOptions{since: in.Value(flagSince), follow: in.Has(flagFollow), target: in.Arg(0)}
	if !logsql.ValidDuration(opts.since) {
		return opts, fmt.Errorf("--since %q is not a duration; write e.g. 30m, 24h, 7d or 1h30m", opts.since)
	}
	n, err := strconv.Atoi(in.Value(flagLines))
	if err != nil || n < 1 {
		return opts, fmt.Errorf("-n %q is not a positive number", in.Value(flagLines))
	}
	opts.limit = n
	if len(in.args) > 1 {
		// The common case is looking for a word, not writing a query language.
		opts.grep = strings.Join(in.args[1:], " ")
	}
	return opts, nil
}

// buildLogQuery turns a target and a search into the two LogsQL queries a log
// read needs: history, bounded by --since, and tail, which is not (see
// logsql.Queries for why they differ).
//
// The target is resolved against the manifests so a mistyped service name
// fails here, naming what does exist, rather than silently returning no
// lines, which is indistinguishable from a service that simply said nothing.
func buildLogQuery(cfg Config, opts logOptions) (history, tail string, err error) {
	var jobs []logJob
	if opts.target != "" {
		if jobs, err = resolveLogTarget(cfg, opts.target); err != nil {
			return "", "", err
		}
	}
	history, tail = logsql.Queries(jobs, opts.grep, opts.since)
	return history, tail, nil
}

// logJob is one job whose logs a target covers.
type logJob = logsql.Job

// resolveLogTarget maps "group", "group/service" or a bare service name onto
// the jobs the log store knows.
//
// The jobs come from deploy.ServiceJobIDs, the same list that decides which
// job names a service owns, so a service's backup runs are among its logs. A
// failing nightly backup must not be the one job whose output `orca logs`
// cannot show, since it is exactly the one nobody is watching.
func resolveLogTarget(cfg Config, target string) ([]logJob, error) {
	groups, err := loadGroups(cfg)
	if err != nil {
		return nil, err
	}

	type svc struct {
		group, name string
		jobs        []logJob
	}
	var all []svc
	// The platform is not a directory on disk, so it is added by hand. vector
	// is included though it registers no address: it produces logs like
	// anything else, and being unable to ask for them would be surprising.
	for _, p := range deploy.OrcaServices {
		all = append(all, svc{manifest.ReservedGroup, p, []logJob{{ID: deploy.JobID(manifest.ReservedGroup, p)}}})
	}
	for _, m := range groups {
		for _, s := range m.Services {
			var jobs []logJob
			for _, id := range deploy.ServiceJobIDs(m.Group, s) {
				if id == deploy.BackupJobID(m.Group, s.Name) {
					if s.Backup != nil {
						jobs = append(jobs, logJob{ID: id, Periodic: true})
					}
					continue
				}
				jobs = append(jobs, logJob{ID: id})
			}
			if len(jobs) > 0 {
				all = append(all, svc{m.Group, s.Name, jobs})
			}
		}
	}

	group, name, qualified := strings.Cut(target, "/")

	var jobs []logJob
	for _, s := range all {
		switch {
		case qualified && s.group == group && s.name == name,
			!qualified && (s.group == target || s.name == target):
			jobs = append(jobs, s.jobs...)
		}
	}

	if len(jobs) == 0 {
		names := make([]string, 0, len(all))
		for _, s := range all {
			names = append(names, s.group+"/"+s.name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("no group or service named %q; known: %s", target, strings.Join(names, ", "))
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Filter() < jobs[j].Filter() })
	return jobs, nil
}

func printLogLine(raw []byte) {
	var l LogLine
	if err := json.Unmarshal(raw, &l); err != nil {
		// Not a line orca recognizes; show it rather than swallow it.
		fmt.Println(string(raw))
		return
	}

	stamp := l.Time
	if t, err := time.Parse(time.RFC3339Nano, l.Time); err == nil {
		stamp = t.Local().Format("15:04:05")
	}

	name := l.Job
	if l.Task != "" && l.Task != l.Job {
		name = l.Task
	}

	fmt.Printf("%s  %-14s %s\n", stamp, name, l.Message)
}

// logsQueryURL builds the store's query URL with the LogsQL properly encoded.
func logsQueryURL(path, query string, limit int) string {
	v := url.Values{}
	v.Set("query", query)
	if limit > 0 {
		v.Set("limit", fmt.Sprint(limit))
	}
	return path + "?" + v.Encode()
}
