package statuspage

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/unitoftime/orca/internal/deploy"
)

// Summary is everything the page and `orca top` show, judged once, here, so
// that the two cannot disagree about what counts as a problem.
type Summary struct {
	Time time.Time `json:"time"`

	// Problems is every warning and failure, worst first. Empty is the
	// "All good" the page leads with.
	Problems []Problem `json:"problems"`

	Machines []Machine `json:"machines"`
	Services []Service `json:"services"`

	// Stores are orca's own log and metric stores: how much they hold, and
	// whether they are still accepting data.
	Stores []Store `json:"stores,omitempty"`

	// Unavailable names each source that could not be read, and why. A
	// section with no data then says "could not ask" rather than looking like
	// a quiet cluster.
	Unavailable map[string]string `json:"unavailable,omitempty"`
}

// Level is how much something needs a look.
type Level string

const (
	LevelOK   Level = "ok"
	LevelWarn Level = "warn"
	LevelCrit Level = "crit"
)

func (l Level) rank() int {
	switch l {
	case LevelCrit:
		return 2
	case LevelWarn:
		return 1
	}
	return 0
}

// Problem is one thing that needs a look.
type Problem struct {
	Level   Level  `json:"level"`
	Subject string `json:"subject"` // a machine's name, or group/service
	What    string `json:"what"`
}

// Machine is one node: whether it is up, and how full it is.
type Machine struct {
	Name string `json:"name"`

	// Status is Nomad's view: ready, down, initializing — or "unknown" when
	// Nomad could not be asked.
	Status   string `json:"status"`
	Draining bool   `json:"draining,omitempty"`
	Eligible bool   `json:"eligible"`

	// Reporting is whether its metrics are arriving. A machine Nomad calls
	// ready but that reports nothing has a broken exporter, and every number
	// below is missing rather than zero.
	Reporting bool `json:"reporting"`

	Level Level `json:"level"`

	// RebootRequired is an installed update that does nothing until the
	// machine restarts: a kernel's, say. Nothing restarts it on its own.
	RebootRequired bool `json:"rebootRequired,omitempty"`

	UptimeSeconds float64  `json:"uptimeSeconds,omitempty"`
	Cores         int      `json:"cores,omitempty"`
	Load1         *float64 `json:"load1,omitempty"`
	CPU           *Gauge   `json:"cpu,omitempty"`
	Memory        *Usage   `json:"memory,omitempty"`
	Swap          *Usage   `json:"swap,omitempty"`
	Disks         []Disk   `json:"disks,omitempty"`
	Net           *Net     `json:"net,omitempty"`

	// The last hour, one point a minute, for a sparkline.
	CPUHistory    []Point `json:"cpuHistory,omitempty"`
	MemoryHistory []Point `json:"memoryHistory,omitempty"`

	// ClaimedCPU (in MHz) and ClaimedMemory (in bytes) are what the services
	// placed on it have claimed, against what Nomad has to hand out. Nomad
	// places by these, not by what is in use, so a machine whose memory is
	// all claimed takes nothing new however idle it is, and a blue/green
	// deploy there has no room for its second copy.
	ClaimedCPU    *Usage `json:"claimedCpu,omitempty"`
	ClaimedMemory *Usage `json:"claimedMemory,omitempty"`

	// Placed is every service running on it, most memory claimed first.
	Placed []Placed `json:"placed,omitempty"`
}

// Placed is one service's copies on one machine.
type Placed struct {
	Group    string `json:"group"`
	Name     string `json:"name"`
	Platform bool   `json:"platform,omitempty"`
	Copies   int    `json:"copies"`

	// What its copies here claimed: CPU in vCPU, as a manifest declares it,
	// and memory in bytes.
	CPU    float64 `json:"cpu"`
	Memory int64   `json:"memory"`

	// MemoryUsed is what they use now, when the metric store knows.
	MemoryUsed *int64 `json:"memoryUsed,omitempty"`
}

// Gauge is a percentage and what it means.
type Gauge struct {
	Percent float64 `json:"percent"`
	Level   Level   `json:"level"`
}

// Usage is an amount of something with a size.
type Usage struct {
	Used    int64   `json:"used"`
	Total   int64   `json:"total"`
	Percent float64 `json:"percent"`
	Level   Level   `json:"level"`
}

// Disk is one filesystem.
type Disk struct {
	Mount  string `json:"mount"`
	Device string `json:"device"`
	FSType string `json:"fstype"`
	Usage
	Inodes *Gauge `json:"inodes,omitempty"`
}

// Net is traffic on the machine's own interfaces, not its containers'.
type Net struct {
	RxBytesPerSec float64 `json:"rxBytesPerSec"`
	TxBytesPerSec float64 `json:"txBytesPerSec"`
}

// Point is one sample: unix seconds and a value.
type Point struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

// Service is one service's health and what it is using.
type Service struct {
	Group string `json:"group"`
	Name  string `json:"name"`
	JobID string `json:"jobId"`

	// Platform marks orca's own services.
	Platform bool `json:"platform,omitempty"`

	// Periodic marks a job that runs on a schedule and is shown by its last
	// run: a backup whose database is not a row of its own.
	Periodic bool `json:"periodic,omitempty"`

	// Health is orca status's verdict, verbatim: running, pending, failed,
	// unplaced, stopped, scheduled.
	Health   string     `json:"health"`
	Level    Level      `json:"level"`
	Running  int        `json:"running"`
	Desired  int        `json:"desired"`
	Restarts int        `json:"restarts"`
	Node     string     `json:"node,omitempty"`
	Since    *time.Time `json:"since,omitempty"`
	Message  string     `json:"message,omitempty"`

	// CPUCores is CPU in use, in cores. Not judged: a service's cpu is a
	// scheduling weight, not a cap, so it can use several times what it
	// declared and be fine. The machine's CPU is what says the box is full.
	CPUCores *float64 `json:"cpuCores,omitempty"`

	// Memory against its limit, which is a hard one: at 100% the kernel
	// kills it.
	Memory *Usage `json:"memory,omitempty"`

	// Backup is a database's backup job, by its last run.
	Backup *Backup `json:"backup,omitempty"`
}

// Backup is the last run of a database's backups.
type Backup struct {
	JobID   string     `json:"jobId"`
	Health  string     `json:"health"`
	Level   Level      `json:"level"`
	Since   *time.Time `json:"since,omitempty"`
	Message string     `json:"message,omitempty"`
}

// Store is one of orca's own stores.
type Store struct {
	// Name is "logs" or "metrics".
	Name string `json:"name"`

	// Used is what it holds on disk.
	Used int64 `json:"used"`

	// Cap is the log store's hard ceiling. At it, the oldest days are
	// dropped: the cap working, not a fault.
	Cap int64 `json:"cap,omitempty"`

	// Free is the free space on the disk it writes to.
	Free int64 `json:"free"`

	// MinFree is the metric store's floor: below it, it stops storing.
	MinFree int64 `json:"minFree,omitempty"`

	ReadOnly bool `json:"readOnly,omitempty"`

	// Status is the store's state in a few words (accepting, at its cap,
	// near its floor, not accepting data), shown as it is by the page and by
	// orca top.
	Status string `json:"status"`
	Level  Level  `json:"level"`
	Note   string `json:"note,omitempty"`
}

// Thresholds. Fixed, not configurable: the page is for "is anything about to
// fall over", and these are the same numbers anyone would pick.
const (
	// Memory, disks, inodes, and a service against its memory limit.
	fullWarn = 80.0
	fullCrit = 90.0

	// CPU runs hot for good reasons, so only near-saturation is a problem.
	cpuWarn = 80.0
	cpuCrit = 95.0

	// A large disk at 90% can still have plenty left, and a small one at 80%
	// may have almost nothing. Below this much free it is critical whatever
	// the percentage, on any disk big enough for that to be meaningful,
	// which an EFI partition is not.
	diskFreeCrit      = 1 << 30
	diskFreeFloorSize = 4 << 30
)

func percentLevel(p, warn, crit float64) Level {
	switch {
	case p >= crit:
		return LevelCrit
	case p >= warn:
		return LevelWarn
	}
	return LevelOK
}

func worse(a, b Level) Level {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// nomadView is what Nomad said, already projected to orca's types.
type nomadView struct {
	Jobs        map[string]deploy.JobState
	Allocs      []deploy.AllocState
	Deployments []deploy.DeploymentState
	Nodes       []nodeInfo

	// Placement explains each unplaced job, by job ID.
	Placement map[string]string
}

type nodeInfo struct {
	Name     string
	Status   string
	Eligible bool
	Draining bool

	// What it has to hand out to allocations, zero when Nomad did not say.
	CPUMHz   int64
	MemoryMB int64
}

// metricsView is what the metric store said.
type metricsView struct {
	Machines map[string]*machineMetrics // by node name

	// Per allocation, summed over its tasks.
	AllocCPUPercent map[string]float64
	AllocMemUsed    map[string]float64
	AllocMemLimit   map[string]float64

	Stores map[string]*storeMetrics // "logs", "metrics"
}

type machineMetrics struct {
	Up                     bool
	RebootRequired         bool
	Cores                  int
	CPUPercent             *float64
	Load1                  *float64
	MemTotal, MemAvailable *float64
	SwapTotal, SwapFree    *float64
	Uptime                 *float64
	Rx, Tx                 *float64
	Disks                  []Disk
	CPUHistory, MemoryHist []Point
}

// build judges everything. A nil view is a source that could not be read;
// its error goes in Unavailable, and whatever the other source knows is still
// shown.
func build(now time.Time, nv *nomadView, nomadErr error, mv *metricsView, metricsErr error) Summary {
	s := Summary{Time: now, Problems: []Problem{}, Machines: []Machine{}, Services: []Service{}}
	if nomadErr != nil {
		s.unavailable("nomad", nomadErr)
	}
	if metricsErr != nil {
		s.unavailable("metrics", metricsErr)
	}

	s.Machines = buildMachines(nv, mv)
	if nv != nil {
		s.Services = buildServices(nv, mv)
	}
	if mv != nil {
		s.Stores = buildStores(mv)
	}

	// Not being able to ask is a problem of its own: every section it would
	// have filled is blank, and blank must not read as fine. Metrics switched
	// off on purpose is not.
	if nomadErr != nil {
		s.Problems = append(s.Problems, Problem{Level: LevelCrit, Subject: "nomad", What: "cannot read the scheduler: " + nomadErr.Error()})
	}
	if metricsErr != nil && !errors.Is(metricsErr, errMetricsOff) {
		s.Problems = append(s.Problems, Problem{Level: LevelWarn, Subject: "metrics", What: "cannot read the metric store: " + metricsErr.Error()})
	}

	for _, m := range s.Machines {
		s.Problems = append(s.Problems, machineProblems(m, mv != nil)...)
	}
	for _, svc := range s.Services {
		s.Problems = append(s.Problems, serviceProblems(svc)...)
	}
	for _, st := range s.Stores {
		if st.Level != LevelOK {
			s.Problems = append(s.Problems, Problem{Level: st.Level, Subject: st.Name + " store", What: st.Note})
		}
	}
	sort.SliceStable(s.Problems, func(i, j int) bool {
		return s.Problems[i].Level.rank() > s.Problems[j].Level.rank()
	})
	return s
}

func (s *Summary) unavailable(what string, err error) {
	if s.Unavailable == nil {
		s.Unavailable = map[string]string{}
	}
	s.Unavailable[what] = err.Error()
}

func buildMachines(nv *nomadView, mv *metricsView) []Machine {
	byName := map[string]*Machine{}
	get := func(name string) *Machine {
		if m, ok := byName[name]; ok {
			return m
		}
		m := &Machine{Name: name, Status: "unknown", Eligible: true, Level: LevelOK}
		byName[name] = m
		return m
	}

	if nv != nil {
		for _, n := range nv.Nodes {
			m := get(n.Name)
			m.Status, m.Eligible, m.Draining = n.Status, n.Eligible, n.Draining
		}
		place(byName, nv, mv)
	}
	if mv != nil {
		for name, mm := range mv.Machines {
			fillMachine(get(name), mm)
		}
	}

	out := make([]Machine, 0, len(byName))
	for _, m := range byName {
		m.Level = machineLevel(*m, mv != nil)
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// place fills in what runs on each machine and how much of it is claimed.
//
// Allocations are counted while Nomad wants them running, starting ones
// included, since they hold their claim from the moment they are placed.
func place(byName map[string]*Machine, nv *nomadView, mv *metricsView) {
	type key struct{ node, job string }
	placed := map[key]*Placed{}
	type claim struct{ mhz, mb int64 }
	claimed := map[string]claim{}

	for _, a := range nv.Allocs {
		if a.DesiredStatus != "run" || (a.ClientStatus != "running" && a.ClientStatus != "pending") {
			continue
		}
		m, ok := byName[a.NodeName]
		if !ok {
			continue
		}
		c := claimed[a.NodeName]
		claimed[a.NodeName] = claim{c.mhz + a.CPUMHz, c.mb + a.MemoryMB}

		// A backup run belongs to its periodic job, which is the one that
		// carries the group and service.
		jobID, _, _ := strings.Cut(a.JobID, "/periodic-")
		k := key{m.Name, jobID}
		p := placed[k]
		if p == nil {
			job := nv.Jobs[jobID]
			p = &Placed{Group: job.App, Name: job.Service, Platform: job.App == deploy.OrcaApp}
			if p.Name == "" {
				p.Name = jobID
			}
			placed[k] = p
		}
		p.Copies++
		p.CPU += float64(a.CPUMHz) / deploy.MHzPerVCPU
		p.Memory += a.MemoryMB << 20
		if mv != nil {
			if used, ok := mv.AllocMemUsed[a.ID]; ok {
				u := int64(used)
				if p.MemoryUsed != nil {
					u += *p.MemoryUsed
				}
				p.MemoryUsed = &u
			}
		}
	}

	for k, p := range placed {
		byName[k.node].Placed = append(byName[k.node].Placed, *p)
	}
	for _, m := range byName {
		sort.Slice(m.Placed, func(i, j int) bool {
			if m.Placed[i].Memory != m.Placed[j].Memory {
				return m.Placed[i].Memory > m.Placed[j].Memory
			}
			return m.Placed[i].Group+"/"+m.Placed[i].Name < m.Placed[j].Group+"/"+m.Placed[j].Name
		})
	}

	for _, n := range nv.Nodes {
		m, c := byName[n.Name], claimed[n.Name]
		if n.CPUMHz > 0 {
			m.ClaimedCPU = usage(float64(c.mhz), float64(n.CPUMHz))
		}
		if n.MemoryMB > 0 {
			m.ClaimedMemory = usage(float64(c.mb<<20), float64(n.MemoryMB<<20))
		}
	}
}

func fillMachine(m *Machine, mm *machineMetrics) {
	m.Reporting = mm.Up
	m.RebootRequired = mm.RebootRequired
	m.Cores = mm.Cores
	m.Load1 = mm.Load1
	if mm.Uptime != nil {
		m.UptimeSeconds = *mm.Uptime
	}
	if mm.CPUPercent != nil {
		p := *mm.CPUPercent
		m.CPU = &Gauge{Percent: p, Level: percentLevel(p, cpuWarn, cpuCrit)}
	}
	if mm.MemTotal != nil && mm.MemAvailable != nil && *mm.MemTotal > 0 {
		m.Memory = usage(*mm.MemTotal-*mm.MemAvailable, *mm.MemTotal)
	}
	if mm.SwapTotal != nil && mm.SwapFree != nil && *mm.SwapTotal > 0 {
		// Reported, never judged: swap in use on its own is not a problem,
		// and memory is already judged by what is available.
		m.Swap = usage(*mm.SwapTotal-*mm.SwapFree, *mm.SwapTotal)
		m.Swap.Level = LevelOK
	}
	m.Disks = mm.Disks
	if mm.Rx != nil || mm.Tx != nil {
		m.Net = &Net{}
		if mm.Rx != nil {
			m.Net.RxBytesPerSec = *mm.Rx
		}
		if mm.Tx != nil {
			m.Net.TxBytesPerSec = *mm.Tx
		}
	}
	m.CPUHistory, m.MemoryHistory = mm.CPUHistory, mm.MemoryHist
}

func usage(used, total float64) *Usage {
	p := 100 * used / total
	return &Usage{Used: int64(used), Total: int64(total), Percent: p, Level: percentLevel(p, fullWarn, fullCrit)}
}

// diskUsage judges a filesystem by the percentage, and by what is left.
func diskUsage(size, avail float64) Usage {
	u := *usage(size-avail, size)
	if avail < diskFreeCrit && size >= diskFreeFloorSize {
		u.Level = LevelCrit
	}
	return u
}

func machineLevel(m Machine, askedMetrics bool) Level {
	var worst Level = LevelOK
	for _, p := range machineProblems(m, askedMetrics) {
		worst = worse(worst, p.Level)
	}
	return worst
}

func machineProblems(m Machine, askedMetrics bool) []Problem {
	var out []Problem
	add := func(l Level, format string, args ...any) {
		out = append(out, Problem{Level: l, Subject: m.Name, What: fmt.Sprintf(format, args...)})
	}

	switch m.Status {
	case "down", "disconnected":
		add(LevelCrit, "%s: Nomad has lost contact with it", m.Status)
	case "initializing":
		add(LevelWarn, "still joining the cluster")
	}
	if askedMetrics && !m.Reporting && m.Status != "down" {
		add(LevelWarn, "not reporting metrics")
	}
	if m.RebootRequired {
		add(LevelWarn, "an installed update is waiting for a reboot")
	}
	if m.CPU != nil && m.CPU.Level != LevelOK {
		add(m.CPU.Level, "CPU at %.0f%%", m.CPU.Percent)
	}
	if m.Memory != nil && m.Memory.Level != LevelOK {
		add(m.Memory.Level, "memory %.0f%% used (%s free)", m.Memory.Percent, BytesHuman(m.Memory.Total-m.Memory.Used))
	}
	for _, d := range m.Disks {
		if d.Level != LevelOK {
			add(d.Level, "disk %s %.0f%% full (%s free)", d.Mount, d.Percent, BytesHuman(d.Total-d.Used))
		}
		if d.Inodes != nil && d.Inodes.Level != LevelOK {
			add(d.Inodes.Level, "disk %s has used %.0f%% of its inodes", d.Mount, d.Inodes.Percent)
		}
	}
	return out
}

func buildServices(nv *nomadView, mv *metricsView) []Service {
	statuses := deploy.Summarize(nv.Jobs, nv.Allocs, nv.Deployments)

	// The allocations running now, per job: usage is summed over these and
	// no others, so a replica that was just replaced is not counted twice
	// while its last samples age out of the metric store.
	current := map[string][]string{}
	for _, a := range nv.Allocs {
		if a.DesiredStatus == "run" && a.ClientStatus == "running" {
			current[a.JobID] = append(current[a.JobID], a.ID)
		}
	}

	var out []Service
	index := map[string]int{}
	var backups []deploy.ServiceStatus
	for _, st := range statuses {
		if nv.Jobs[st.JobID].Periodic {
			backups = append(backups, st)
			continue
		}
		if st.Health == deploy.HealthUnplaced {
			if why := nv.Placement[st.JobID]; why != "" {
				st.Message = why
			}
		}
		svc := Service{
			Group:    st.App,
			Name:     st.Service,
			JobID:    st.JobID,
			Platform: st.App == deploy.OrcaApp,
			Health:   string(st.Health),
			Level:    healthLevel(st.Health),
			Running:  st.Running,
			Desired:  st.Desired,
			Restarts: st.Restarts,
			Node:     st.Node,
			Message:  st.Message,
		}
		if !st.Since.IsZero() {
			t := st.Since
			svc.Since = &t
		}
		if mv != nil {
			svc.CPUCores, svc.Memory = allocUsage(mv, current[st.JobID])
		}
		index[st.JobID] = len(out)
		out = append(out, svc)
	}

	// A database's backup job is shown on the database's row: "is the
	// database backed up" is a question about the database. A periodic job
	// that belongs to no row is shown on its own.
	for _, b := range backups {
		bk := &Backup{JobID: b.JobID, Health: string(b.Health), Level: healthLevel(b.Health), Message: b.Message}
		if !b.Since.IsZero() {
			t := b.Since
			bk.Since = &t
		}
		parent := deploy.JobID(b.App, strings.TrimSuffix(b.Service, "-backup"))
		if i, ok := index[parent]; ok && b.JobID == deploy.BackupJobID(b.App, strings.TrimSuffix(b.Service, "-backup")) {
			out[i].Backup = bk
			continue
		}
		out = append(out, Service{
			Group: b.App, Name: b.Service, JobID: b.JobID, Platform: b.App == deploy.OrcaApp, Periodic: true,
			Health: bk.Health, Level: bk.Level, Running: b.Running, Desired: b.Desired,
			Restarts: b.Restarts, Since: bk.Since, Message: b.Message,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		// Your services before orca's own, then by name.
		if out[i].Platform != out[j].Platform {
			return !out[i].Platform
		}
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// allocUsage sums what a service's current allocations use. Nil when none of
// them has reported yet: unknown, which is not the same as zero.
func allocUsage(mv *metricsView, allocIDs []string) (*float64, *Usage) {
	var cpu, used, limit float64
	var sawCPU, sawMem bool
	for _, id := range allocIDs {
		if v, ok := mv.AllocCPUPercent[id]; ok {
			cpu += v
			sawCPU = true
		}
		u, okU := mv.AllocMemUsed[id]
		l, okL := mv.AllocMemLimit[id]
		if okU && okL {
			used += u
			limit += l
			sawMem = true
		}
	}
	var cores *float64
	if sawCPU {
		c := cpu / 100
		cores = &c
	}
	var mem *Usage
	if sawMem && limit > 0 {
		mem = usage(used, limit)
	}
	return cores, mem
}

// storeMetrics is what a store says about its own disk. Nil fields were not
// reported.
type storeMetrics struct {
	Used, Cap, Free, MinFree, ReadOnly *float64
}

func buildStores(mv *metricsView) []Store {
	var out []Store
	for _, name := range []string{"logs", "metrics"} {
		sm := mv.Stores[name]
		if sm == nil || sm.Used == nil {
			continue
		}
		st := Store{Name: name, Used: int64(*sm.Used), Level: LevelOK}
		if sm.Cap != nil {
			st.Cap = int64(*sm.Cap)
		}
		if sm.Free != nil {
			st.Free = int64(*sm.Free)
		}
		if sm.MinFree != nil {
			st.MinFree = int64(*sm.MinFree)
		}
		st.ReadOnly = sm.ReadOnly != nil && *sm.ReadOnly > 0
		judgeStore(&st)
		out = append(out, st)
	}
	return out
}

func judgeStore(st *Store) {
	st.Status = "accepting"
	switch {
	case st.ReadOnly && st.Name == "logs":
		st.Status, st.Level = "not accepting data", LevelCrit
		st.Note = "has stopped accepting logs: its disk is full"
	case st.ReadOnly:
		st.Status, st.Level = "not accepting data", LevelCrit
		st.Note = fmt.Sprintf("has stopped storing metrics: under %s free on its disk", BytesHuman(st.MinFree))
	case st.Name == "metrics" && st.MinFree > 0 && st.Free < 2*st.MinFree:
		st.Status, st.Level = "near its floor", LevelWarn
		st.Note = fmt.Sprintf("stops storing metrics below %s free, and has %s", BytesHuman(st.MinFree), BytesHuman(st.Free))
	case st.Name == "logs" && st.Cap > 0 && float64(st.Used) >= 0.95*float64(st.Cap):
		// The cap working, not a fault: said, not flagged.
		st.Status = "at its cap"
		st.Note = "the oldest logs are dropped to make room"
	}
}

func healthLevel(h deploy.Health) Level {
	switch h {
	case deploy.HealthFailed, deploy.HealthUnplaced:
		return LevelCrit
	case deploy.HealthPending:
		return LevelWarn
	}
	return LevelOK
}

func serviceProblems(s Service) []Problem {
	var out []Problem
	subject := s.Group + "/" + s.Name
	if s.Level != LevelOK {
		what := s.Health
		if s.Message != "" {
			what += ": " + s.Message
		}
		out = append(out, Problem{Level: s.Level, Subject: subject, What: what})
	}
	if s.Memory != nil && s.Memory.Level != LevelOK {
		out = append(out, Problem{Level: s.Memory.Level, Subject: subject,
			What: fmt.Sprintf("memory at %.0f%% of its %s limit", s.Memory.Percent, BytesHuman(s.Memory.Total))})
	}
	if s.Backup != nil && s.Backup.Level != LevelOK {
		what := "backup " + s.Backup.Health
		if s.Backup.Message != "" {
			what += ": " + s.Backup.Message
		}
		out = append(out, Problem{Level: s.Backup.Level, Subject: subject, What: what})
	}
	return out
}

// BytesHuman is a size the way df -h writes it.
func BytesHuman(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	if v >= 10 {
		return fmt.Sprintf("%.0f%c", v, "KMGTPE"[exp])
	}
	return fmt.Sprintf("%.1f%c", v, "KMGTPE"[exp])
}
