package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/unitoftime/orca/pkg/statuspage"
)

func topFixture(now time.Time) statuspage.Summary {
	backedUp := now.Add(-3 * time.Hour)
	cores := 0.42
	return statuspage.Summary{
		Time: now,
		Problems: []statuspage.Problem{
			{Level: statuspage.LevelCrit, Subject: "box0", What: "disk / 93% full (1.2G free)"},
			{Level: statuspage.LevelWarn, Subject: "shop/web", What: "memory at 85% of its 512M limit"},
		},
		Machines: []statuspage.Machine{{
			Name: "box0", Status: "ready", Eligible: true, Reporting: true, Level: statuspage.LevelCrit,
			Cores: 2, UptimeSeconds: 3 * 86400,
			CPU:    &statuspage.Gauge{Percent: 12, Level: statuspage.LevelOK},
			Memory: &statuspage.Usage{Used: 3 << 30, Total: 4 << 30, Percent: 75, Level: statuspage.LevelOK},
			Disks:  []statuspage.Disk{{Mount: "/", Usage: statuspage.Usage{Used: 17 << 30, Total: 18 << 30, Percent: 93, Level: statuspage.LevelCrit}}},
			Net:    &statuspage.Net{RxBytesPerSec: 2048, TxBytesPerSec: 512},
		}},
		Stores: []statuspage.Store{
			{Name: "logs", Used: 9 << 30, Cap: 10 << 30, Free: 40 << 30, Status: "accepting", Level: statuspage.LevelOK},
			{Name: "metrics", Used: 1 << 30, Free: 3 << 30, MinFree: 2 << 30, Status: "near its floor", Level: statuspage.LevelWarn,
				Note: "stops storing metrics below 2.0G free, and has 3.0G"},
		},
		Services: []statuspage.Service{
			{Group: "shop", Name: "web", Health: "running", Level: statuspage.LevelOK, Running: 2, Desired: 2, Node: "box0",
				CPUCores: &cores, Memory: &statuspage.Usage{Used: 435 << 20, Total: 512 << 20, Percent: 85, Level: statuspage.LevelWarn}},
			{Group: "shop", Name: "db", Health: "running", Level: statuspage.LevelOK, Running: 1, Desired: 1, Node: "box0",
				Backup: &statuspage.Backup{Health: "running", Level: statuspage.LevelOK, Since: &backedUp}},
			{Group: "shop", Name: "worker", Health: "failed", Level: statuspage.LevelCrit, Running: 0, Desired: 1,
				Restarts: 5, Message: "Exit Code: 3"},
		},
	}
}

func TestRenderTop(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	renderTop(&b, topFixture(now), false, now)
	out := b.String()

	for _, want := range []string{
		"NODES",
		"box0  ready  up 3d  2 cores",
		"disk  /  ",
		"↓ 2.0K/s  ↑ 512B/s",
		"0.42",
		"85% of 512M",
		"backed up 3h ago",
		"restarts 5  Exit Code: 3",
		"logs     accepting",
		"metrics  near its floor",
		"9.0G of 10G cap  40G free on disk",
		"1.0G stored  stops below 2.0G free  3.0G free on disk",
		"stops storing metrics below 2.0G free, and has 3.0G",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("no colour was asked for")
	}

	// Laid out as the page is: no verdict above the sections, and trouble
	// where it is rather than listed again.
	for _, gone := range []string{"All good", "need a look", "disk / 93% full", "✗"} {
		if strings.Contains(out, gone) {
			t.Errorf("output should not contain %q:\n%s", gone, out)
		}
	}
	if !strings.HasPrefix(out, "as of ") {
		t.Errorf("output should lead with its time:\n%s", out)
	}
}

// A source that could not be read is said at the top, since the sections it
// would have filled are blank.
func TestRenderTopSaysWhatItCouldNotRead(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	s := topFixture(now)
	s.Unavailable = map[string]string{"metrics": "connection refused"}
	var b bytes.Buffer
	renderTop(&b, s, false, now)
	if !strings.Contains(b.String(), "metrics unavailable: connection refused") {
		t.Errorf("output:\n%s", b.String())
	}
}

// Colour must not push a row out of line: a coloured cell is the same width
// on screen as an uncoloured one.
func TestRenderTopAlignsWithColour(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	renderTop(&b, topFixture(now), true, now)
	plain := ansi.ReplaceAllString(b.String(), "")

	// Every service row's replica count starts in the same column.
	col := -1
	for _, line := range strings.Split(plain, "\n") {
		m := regexp.MustCompile(`\d+/\d+`).FindStringIndex(line)
		if m == nil || !strings.HasPrefix(line, "    ") {
			continue
		}
		if col == -1 {
			col = m[0]
		} else if m[0] != col {
			t.Errorf("replica column at %d, want %d:\n%s", m[0], col, plain)
		}
	}
	if col == -1 {
		t.Fatalf("no service rows found:\n%s", plain)
	}
}

// The script runs in a remote shell, where a backtick in a message is a
// command substitution: a hint to run `orca apply orca` once tried to run it
// on the machine.
func TestStatusSummaryScriptHasNoBackticks(t *testing.T) {
	if strings.Contains(statusSummaryScript, "`") {
		t.Errorf("backtick in a remote script:\n%s", statusSummaryScript)
	}
}
