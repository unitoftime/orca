package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/unitoftime/orca/pkg/deploy"
	"github.com/unitoftime/orca/pkg/statuspage"
	"golang.org/x/term"
)

// topInterval is how often -w asks again. The metric store scrapes every 30
// seconds and the status page reuses a summary for five, so asking faster
// would only redraw the same numbers.
const topInterval = 5 * time.Second

// cmdTop shows what the status page shows: each node, store and service, with
// its status, how full it is, and what it is using.
//
// It reads the status page's own summary over SSH rather than working the
// numbers out itself, so the terminal and the page are one judgement and
// cannot disagree.
func cmdTop(ctx context.Context, cfg Config, in invocation) error {
	watch, asJSON := in.Has(flagWatch), in.Has(flagJSON)
	if watch && asJSON {
		return fmt.Errorf("-w and --json do not go together: --json is for reading once")
	}
	if !cfg.Monitoring.Status.Enabled {
		return fmt.Errorf("orca top reads the status page, which is switched off in cluster.yaml (monitoring.status: false)")
	}

	cluster, err := clusterFor(ctx, cfg)
	if err != nil {
		return err
	}

	color := useColor()
	for {
		sum, err := cluster.StatusSummary(ctx)
		if err != nil {
			// Ctrl-C mid-read is how -w is stopped, not a failure.
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(sum)
		}
		if watch {
			// Home and clear, so each refresh replaces the last.
			fmt.Print("\033[H\033[2J")
		}
		renderTop(os.Stdout, sum, color, time.Now())
		if !watch {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(topInterval):
		}
	}
}

// useColor is whether stdout is a terminal that has not asked for none.
func useColor() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// palette colours a level; without colour, the level is spelled out where it
// matters instead.
type palette bool

func (p palette) level(l statuspage.Level, s string) string {
	if !p {
		return s
	}
	switch l {
	case statuspage.LevelCrit:
		return "\033[31m" + s + "\033[0m"
	case statuspage.LevelWarn:
		return "\033[33m" + s + "\033[0m"
	}
	return s
}

func (p palette) dim(s string) string {
	if !p {
		return s
	}
	return "\033[2m" + s + "\033[0m"
}

func (p palette) bold(s string) string {
	if !p {
		return s
	}
	return "\033[1m" + s + "\033[0m"
}

// renderTop writes the summary for a terminal.
// It is laid out as the page is: nodes, stores and services, each with its
// status. There is no verdict above them; trouble shows where it is: a red
// bar, a failed service and why, a store near its floor.
func renderTop(w io.Writer, s statuspage.Summary, color bool, now time.Time) {
	p := palette(color)

	asOf := "as of " + s.Time.Local().Format("15:04:05")
	if age := now.Sub(s.Time).Round(time.Second); age > 2*topInterval {
		asOf = fmt.Sprintf("as of %s ago", age)
	}
	fmt.Fprintln(w, p.dim(asOf))
	// A source that could not be read leaves its sections blank, and blank
	// must not read as quiet.
	for _, what := range sortedKeys(s.Unavailable) {
		level := statuspage.LevelWarn
		if what == "nomad" {
			level = statuspage.LevelCrit
		}
		fmt.Fprintln(w, p.level(level, fmt.Sprintf("%s unavailable: %s", what, s.Unavailable[what])))
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, p.bold("NODES"))
	if len(s.Machines) == 0 {
		fmt.Fprintln(w, "  none known")
	}
	for _, m := range s.Machines {
		renderMachine(w, p, m)
	}

	if len(s.Stores) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, p.bold("STORES"))
		renderStores(w, p, s.Stores)
	}

	fmt.Fprintln(w)
	renderServices(w, p, s.Services, now)
}

// renderStores shows orca's own log and metric stores. The log store has a
// cap, so it is a bar; the metric store has a free-space floor instead, so it
// is words.
func renderStores(w io.Writer, p palette, stores []statuspage.Store) {
	var t table
	for _, st := range stores {
		status := p.level(st.Level, st.Status)
		if st.Cap > 0 {
			pc := 100 * float64(st.Used) / float64(st.Cap)
			t.row(st.Name, status, bar(p, pc, st.Level), p.level(st.Level, pct(pc)),
				fmt.Sprintf("%s of %s cap  %s free on disk", statuspage.BytesHuman(st.Used), statuspage.BytesHuman(st.Cap), statuspage.BytesHuman(st.Free)))
		} else {
			text := statuspage.BytesHuman(st.Used) + " stored"
			if st.MinFree > 0 {
				text += "  stops below " + statuspage.BytesHuman(st.MinFree) + " free"
			}
			// In the last column, under the log store's sizes, so a long
			// line does not widen the bar's column.
			t.row(st.Name, status, "", "", text+"  "+statuspage.BytesHuman(st.Free)+" free on disk")
		}
		if st.Note != "" {
			t.row("", "", "", "", p.level(st.Level, st.Note))
		}
	}
	t.write(w, "  ")
}

const barWidth = 20

// bar is a filled gauge: at a glance, not a number to read.
func bar(p palette, pct float64, l statuspage.Level) string {
	pct = math.Max(0, math.Min(100, pct))
	filled := int(math.Round(pct / 100 * barWidth))
	return p.level(l, strings.Repeat("█", filled)) + p.dim(strings.Repeat("░", barWidth-filled))
}

func renderMachine(w io.Writer, p palette, m statuspage.Machine) {
	head := []string{p.bold(m.Name), p.level(m.Level, m.Status)}
	if m.Draining {
		head = append(head, "draining")
	} else if !m.Eligible {
		head = append(head, "not taking new work")
	}
	if m.UptimeSeconds > 0 {
		head = append(head, "up "+deploy.HumanDuration(time.Duration(m.UptimeSeconds)*time.Second))
	}
	if m.Cores > 0 {
		head = append(head, fmt.Sprintf("%d cores", m.Cores))
	}
	if !m.Reporting && m.Status != "down" {
		head = append(head, p.level(statuspage.LevelWarn, "no metrics"))
	}
	if m.RebootRequired {
		head = append(head, p.level(statuspage.LevelWarn, "reboot required (orca reboot "+m.Name+")"))
	}
	fmt.Fprintf(w, "  %s\n", strings.Join(head, "  "))

	var t table
	if m.CPU != nil {
		extra := ""
		if m.Load1 != nil {
			extra = fmt.Sprintf("load %.2f", *m.Load1)
		}
		t.row("cpu", "", bar(p, m.CPU.Percent, m.CPU.Level), p.level(m.CPU.Level, pct(m.CPU.Percent)), extra)
	}
	if m.Memory != nil {
		t.row("mem", "", bar(p, m.Memory.Percent, m.Memory.Level), p.level(m.Memory.Level, pct(m.Memory.Percent)), ofTotal(m.Memory))
	}
	if m.ClaimedCPU != nil {
		t.row("cpu", "claimed", bar(p, m.ClaimedCPU.Percent, m.ClaimedCPU.Level), p.level(m.ClaimedCPU.Level, pct(m.ClaimedCPU.Percent)), "")
	}
	if m.ClaimedMemory != nil {
		t.row("mem", "claimed", bar(p, m.ClaimedMemory.Percent, m.ClaimedMemory.Level), p.level(m.ClaimedMemory.Level, pct(m.ClaimedMemory.Percent)), ofTotal(m.ClaimedMemory))
	}
	if m.Swap != nil && m.Swap.Used > 0 {
		t.row("swap", "", bar(p, m.Swap.Percent, statuspage.LevelOK), pct(m.Swap.Percent), ofTotal(m.Swap))
	}
	for i, d := range m.Disks {
		label := ""
		if i == 0 {
			label = "disk"
		}
		extra := ofTotal(&d.Usage)
		if d.Inodes != nil && d.Inodes.Level != statuspage.LevelOK {
			extra += "  " + p.level(d.Inodes.Level, "inodes "+pct(d.Inodes.Percent))
		}
		t.row(label, d.Mount, bar(p, d.Percent, d.Level), p.level(d.Level, pct(d.Percent)), extra)
	}
	if m.Net != nil {
		t.row("net", "", fmt.Sprintf("↓ %s/s  ↑ %s/s",
			statuspage.BytesHuman(int64(m.Net.RxBytesPerSec)), statuspage.BytesHuman(int64(m.Net.TxBytesPerSec))))
	}
	t.write(w, "    ")

	// What fills it, by claim: Nomad places by claims, so this is what
	// decides whether the next service fits.
	if len(m.Placed) > 0 {
		var pt table
		for _, pl := range m.Placed {
			name := pl.Name
			if pl.Group != "" {
				name = pl.Group + "/" + name
			}
			if pl.Copies > 1 {
				name += fmt.Sprintf(" ×%d", pl.Copies)
			}
			if pl.Platform {
				name = p.dim(name)
			}
			used := "-"
			if pl.MemoryUsed != nil {
				used = statuspage.BytesHuman(*pl.MemoryUsed) + " in use"
			}
			pt.row(name, fmt.Sprintf("%.1f vCPU", pl.CPU), statuspage.BytesHuman(pl.Memory)+" claimed", p.dim(used))
		}
		pt.write(w, "    ")
	}
}

func renderServices(w io.Writer, p palette, services []statuspage.Service, now time.Time) {
	fmt.Fprintln(w, p.bold("SERVICES"))
	if len(services) == 0 {
		fmt.Fprintln(w, "  nothing deployed")
		return
	}
	var t table
	t.row("", p.dim("health"), "", p.dim("node"), p.dim("cpu"), p.dim("memory"))
	group := ""
	for _, s := range services {
		if s.Group != group {
			t.row(p.bold(s.Group))
			group = s.Group
		}

		cpu := "-"
		if s.CPUCores != nil {
			cpu = fmt.Sprintf("%.2f", *s.CPUCores)
		}
		mem := "-"
		if s.Memory != nil {
			mem = p.level(s.Memory.Level, pct(s.Memory.Percent)) + " of " + statuspage.BytesHuman(s.Memory.Total)
		}

		var notes []string
		if s.Restarts > 0 {
			notes = append(notes, fmt.Sprintf("restarts %d", s.Restarts))
		}
		if s.Backup != nil {
			b := "backup " + s.Backup.Health
			if s.Backup.Since != nil && s.Backup.Health == "running" {
				b = "backed up " + deploy.HumanDuration(now.Sub(*s.Backup.Since)) + " ago"
			}
			notes = append(notes, p.level(s.Backup.Level, b))
		}
		if s.Level != statuspage.LevelOK && s.Message != "" {
			notes = append(notes, s.Message)
		}

		t.row("  "+s.Name, p.level(s.Level, s.Health), fmt.Sprintf("%d/%d", s.Running, s.Desired),
			s.Node, cpu, mem, strings.Join(notes, "  "))
	}
	t.write(w, "  ")
	fmt.Fprintln(w, p.dim("  cpu is in cores; memory is against each service's limit"))
}

func pct(v float64) string { return fmt.Sprintf("%3.0f%%", v) }

func ofTotal(u *statuspage.Usage) string {
	return statuspage.BytesHuman(u.Used) + " / " + statuspage.BytesHuman(u.Total)
}

// table lines up columns by what is visible. text/tabwriter counts colour
// escapes as width, so a coloured cell would push its row out of line with
// the uncoloured ones.
type table struct{ rows [][]string }

func (t *table) row(cells ...string) { t.rows = append(t.rows, cells) }

func (t *table) write(w io.Writer, indent string) {
	var widths []int
	for _, r := range t.rows {
		// A row with one cell is a heading and does not size the columns.
		if len(r) == 1 {
			continue
		}
		for i, c := range r {
			if i >= len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], visibleWidth(c))
		}
	}
	for _, r := range t.rows {
		var b strings.Builder
		b.WriteString(indent)
		for i, c := range r {
			b.WriteString(c)
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-visibleWidth(c)+2))
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

func visibleWidth(s string) int {
	return utf8.RuneCountInString(ansi.ReplaceAllString(s, ""))
}
