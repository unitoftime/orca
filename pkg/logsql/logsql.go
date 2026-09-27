// Package logsql builds the log store's queries, for `orca logs` and for the
// status page's log drawer alike, so a service's logs are the same lines
// wherever they are read.
package logsql

import (
	"fmt"
	"strings"
)

// Job is one job whose logs a target covers.
type Job struct {
	ID string

	// Periodic matches the runs of a periodic job. Each run is a child job
	// named <id>/periodic-<time>, and that is the name its log lines carry.
	Periodic bool
}

// Filter is the LogsQL that selects this job's lines.
func (j Job) Filter() string {
	if j.Periodic {
		return fmt.Sprintf("job:%q*", j.ID+"/periodic-")
	}
	return fmt.Sprintf("job:%q", j.ID)
}

// Queries turns jobs and a search into the two queries a log read needs:
// history, bounded by since, and tail, which is not.
//
// They differ only in that filter because VictoriaLogs' live tailing endpoint
// refuses a query carrying `_time` — and refuses it by streaming nothing at
// all, with no error and a 200. Sending one query to both endpoints is
// therefore a follow that hangs forever against a service that is logging
// steadily, which is what it did.
func Queries(jobs []Job, grep, since string) (history, tail string) {
	var filters []string
	if len(jobs) > 0 {
		quoted := make([]string, 0, len(jobs))
		for _, j := range jobs {
			quoted = append(quoted, j.Filter())
		}
		filters = append(filters, "("+strings.Join(quoted, " OR ")+")")
	}
	if grep != "" {
		filters = append(filters, fmt.Sprintf("%q", grep))
	}

	tail = strings.Join(filters, " ")
	history = tail
	if since != "" {
		history = strings.TrimSpace("_time:" + since + " " + tail)
	}

	// An empty query matches nothing rather than everything, so the
	// unfiltered read has to say "everything" out loud.
	if history == "" {
		history = "*"
	}
	if tail == "" {
		tail = "*"
	}
	return history, tail
}

// Line is one entry as the log store returns it. The field names are the ones
// Vector attaches, which is why a line arrives knowing which job and task
// produced it rather than only a container id.
type Line struct {
	Time    string `json:"_time"`
	Message string `json:"_msg"`
	Job     string `json:"job"`
	Task    string `json:"task"`
	Stream  string `json:"stream"`
}
