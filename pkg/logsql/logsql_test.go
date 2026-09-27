package logsql

import "testing"

func TestQueries(t *testing.T) {
	jobs := []Job{{ID: "shop-db"}, {ID: "shop-db-backup", Periodic: true}}
	history, tail := Queries(jobs, `say "hi"`, "1h")
	if want := `_time:1h (job:"shop-db" OR job:"shop-db-backup/periodic-"*) "say \"hi\""`; history != want {
		t.Errorf("history = %s\nwant      %s", history, want)
	}
	// The tailing endpoint streams nothing for a query with _time in it.
	if want := `(job:"shop-db" OR job:"shop-db-backup/periodic-"*) "say \"hi\""`; tail != want {
		t.Errorf("tail = %s\nwant   %s", tail, want)
	}

	// Nothing to filter on is everything, said out loud.
	if h, tl := Queries(nil, "", ""); h != "*" || tl != "*" {
		t.Errorf("empty = %q, %q", h, tl)
	}
}
