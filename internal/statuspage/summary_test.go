package statuspage

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/unitoftime/orca/internal/deploy"
)

var now = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

func f(v float64) *float64 { return &v }

const gib = 1 << 30

func machineByName(t *testing.T, s Summary, name string) Machine {
	t.Helper()
	for _, m := range s.Machines {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no machine %q in %+v", name, s.Machines)
	return Machine{}
}

func serviceByName(t *testing.T, s Summary, group, name string) Service {
	t.Helper()
	for _, svc := range s.Services {
		if svc.Group == group && svc.Name == name {
			return svc
		}
	}
	t.Fatalf("no service %s/%s in %+v", group, name, s.Services)
	return Service{}
}

func hasProblem(s Summary, subject, contains string) *Problem {
	for i, p := range s.Problems {
		if p.Subject == subject && strings.Contains(p.What, contains) {
			return &s.Problems[i]
		}
	}
	return nil
}

func TestMachinesAreJudged(t *testing.T) {
	results := map[string][]sample{
		qUp: {
			{labels: map[string]string{"node": "box0"}, value: 1},
			{labels: map[string]string{"node": "box1"}, value: 0},
		},
		qCPU:      {{labels: map[string]string{"node": "box0"}, value: 97}},
		qMemTotal: {{labels: map[string]string{"node": "box0"}, value: 8 * gib}},
		qMemAvail: {{labels: map[string]string{"node": "box0"}, value: 1.25 * gib}},
		qFSSize: {
			{labels: map[string]string{"node": "box0", "mountpoint": "/", "device": "/dev/vda1", "fstype": "ext4"}, value: 100 * gib},
			// The same device bind-mounted elsewhere is the same disk.
			{labels: map[string]string{"node": "box0", "mountpoint": "/srv/data", "device": "/dev/vda1", "fstype": "ext4"}, value: 100 * gib},
			// Nearly empty by percentage, but a big disk with under a GiB left.
			{labels: map[string]string{"node": "box0", "mountpoint": "/big", "device": "/dev/vdb", "fstype": "xfs"}, value: 2000 * gib},
			// A tiny EFI partition is not held to the free-space floor.
			{labels: map[string]string{"node": "box0", "mountpoint": "/boot/efi", "device": "/dev/vda15", "fstype": "vfat"}, value: 0.1 * gib},
		},
		qFSAvail: {
			{labels: map[string]string{"node": "box0", "mountpoint": "/"}, value: 15 * gib},
			{labels: map[string]string{"node": "box0", "mountpoint": "/srv/data"}, value: 15 * gib},
			{labels: map[string]string{"node": "box0", "mountpoint": "/big"}, value: 0.5 * gib},
			{labels: map[string]string{"node": "box0", "mountpoint": "/boot/efi"}, value: 0.09 * gib},
		},
		qFSFiles:     {{labels: map[string]string{"node": "box0", "mountpoint": "/"}, value: 1000}},
		qFSFilesFree: {{labels: map[string]string{"node": "box0", "mountpoint": "/"}, value: 50}},
	}
	mv := assembleMetrics(results, nil)
	nv := &nomadView{
		Jobs: map[string]deploy.JobState{},
		Nodes: []nodeInfo{
			{Name: "box0", Status: "ready", Eligible: true},
			{Name: "box1", Status: "ready", Eligible: true},
			{Name: "box2", Status: "down"},
		},
	}
	s := build(now, nv, nil, mv, nil)

	box0 := machineByName(t, s, "box0")
	if box0.CPU == nil || box0.CPU.Level != LevelCrit {
		t.Errorf("97%% CPU should be critical: %+v", box0.CPU)
	}
	if box0.Memory == nil || box0.Memory.Level != LevelWarn || box0.Memory.Used != int64(6.75*gib) {
		t.Errorf("85%% memory should warn: %+v", box0.Memory)
	}
	if len(box0.Disks) != 3 {
		t.Fatalf("want /, /big and /boot/efi, got %+v", box0.Disks)
	}
	levels := map[string]Level{}
	for _, d := range box0.Disks {
		levels[d.Mount] = d.Level
	}
	if levels["/"] != LevelWarn || levels["/big"] != LevelCrit || levels["/boot/efi"] != LevelOK {
		t.Errorf("disk levels = %v", levels)
	}
	if box0.Disks[0].Mount != "/" || box0.Disks[0].Inodes == nil || box0.Disks[0].Inodes.Level != LevelCrit {
		t.Errorf("95%% of inodes should be critical: %+v", box0.Disks[0])
	}
	if box0.Level != LevelCrit {
		t.Errorf("box0 level = %s", box0.Level)
	}

	if p := hasProblem(s, "box1", "not reporting"); p == nil || p.Level != LevelWarn {
		t.Errorf("a ready machine sending no metrics should warn: %+v", s.Problems)
	}
	if p := hasProblem(s, "box2", "lost contact"); p == nil || p.Level != LevelCrit {
		t.Errorf("a down machine is critical: %+v", s.Problems)
	}
	if hasProblem(s, "box2", "not reporting") != nil {
		t.Error("a down machine is not also 'not reporting'")
	}

	for i := 1; i < len(s.Problems); i++ {
		if s.Problems[i].Level.rank() > s.Problems[i-1].Level.rank() {
			t.Fatalf("problems not worst first: %+v", s.Problems)
		}
	}
}

func TestServicesAreJudged(t *testing.T) {
	started := now.Add(-time.Hour)
	run := func(id, job, node string) deploy.AllocState {
		return deploy.AllocState{ID: id, JobID: job, ClientStatus: "running", DesiredStatus: "run", NodeName: node,
			Tasks: []deploy.TaskState{{Name: "t", State: "running", StartedAt: started}}}
	}
	nv := &nomadView{
		Jobs: map[string]deploy.JobState{
			"shop-web":       {ID: "shop-web", Group: "shop", Service: "web", Count: 2},
			"shop-db":        {ID: "shop-db", Group: "shop", Service: "db", Count: 1},
			"shop-db-backup": {ID: "shop-db-backup", Group: "shop", Service: "db-backup", Count: 1, Periodic: true},
			"shop-lost":      {ID: "shop-lost", Group: "shop", Service: "lost", Count: 1},
			"orca-traefik":   {ID: "orca-traefik", Group: "orca", Service: "traefik", Count: 1},
		},
		Allocs: []deploy.AllocState{
			run("w1", "shop-web", "box0"),
			run("w2", "shop-web", "box1"),
			// Replaced moments ago: its last samples are still in the store.
			{ID: "w0", JobID: "shop-web", ClientStatus: "complete", DesiredStatus: "stop"},
			run("d1", "shop-db", "box0"),
			run("t1", "orca-traefik", "box0"),
			{ID: "b1", JobID: "shop-db-backup/periodic-1790000000", ClientStatus: "failed", DesiredStatus: "run",
				CreateTime: 1, Tasks: []deploy.TaskState{{Name: "dump", State: "dead", Failed: true, Fail: "Exit Code: 1"}}},
		},
		Placement: map[string]string{"shop-lost": "no capacity: memory"},
	}
	mv := &metricsView{
		Machines:        map[string]*machineMetrics{},
		AllocCPUPercent: map[string]float64{"w1": 30, "w2": 20, "w0": 400, "d1": 5},
		AllocMemUsed:    map[string]float64{"w1": 100 << 20, "w2": 100 << 20, "w0": 500 << 20, "d1": 470 << 20},
		AllocMemLimit:   map[string]float64{"w1": 256 << 20, "w2": 256 << 20, "w0": 256 << 20, "d1": 512 << 20},
	}
	s := build(now, nv, nil, mv, nil)

	web := serviceByName(t, s, "shop", "web")
	if web.Health != "running" || web.Running != 2 || web.Level != LevelOK {
		t.Errorf("web = %+v", web)
	}
	// Only the two allocations running now: the replaced one would add four
	// cores and half a gigabyte that are not being used.
	if web.CPUCores == nil || *web.CPUCores != 0.5 {
		t.Errorf("web cores = %v, want 0.5", web.CPUCores)
	}
	if web.Memory == nil || web.Memory.Used != 200<<20 || web.Memory.Total != 512<<20 {
		t.Errorf("web memory = %+v", web.Memory)
	}

	db := serviceByName(t, s, "shop", "db")
	if db.Memory == nil || db.Memory.Level != LevelCrit {
		t.Errorf("db at 92%% of its limit should be critical: %+v", db.Memory)
	}
	if db.Backup == nil || db.Backup.Health != "failed" || db.Backup.Message != "Exit Code: 1" {
		t.Errorf("the failed backup should be on the database's row: %+v", db.Backup)
	}
	for _, svc := range s.Services {
		if svc.Name == "db-backup" {
			t.Error("a database's backup job should not also be a row of its own")
		}
	}
	if hasProblem(s, "shop/db", "backup failed") == nil || hasProblem(s, "shop/db", "memory at 92%") == nil {
		t.Errorf("problems = %+v", s.Problems)
	}

	lost := serviceByName(t, s, "shop", "lost")
	if lost.Health != "unplaced" || lost.Level != LevelCrit || lost.Message != "no capacity: memory" {
		t.Errorf("an unplaced service should say why: %+v", lost)
	}

	if last := s.Services[len(s.Services)-1]; !last.Platform {
		t.Errorf("orca's own services should come after yours: %+v", s.Services)
	}
}

// A store that cannot be read blanks its sections, and that has to show as a
// problem rather than as a quiet cluster. Switched off on purpose, it does not.
func TestUnavailableSources(t *testing.T) {
	nv := &nomadView{Jobs: map[string]deploy.JobState{}, Nodes: []nodeInfo{{Name: "box0", Status: "ready"}}}

	s := build(now, nv, nil, nil, errors.New("connection refused"))
	if s.Unavailable["metrics"] == "" || hasProblem(s, "metrics", "connection refused") == nil {
		t.Errorf("unreachable metrics should be reported: %+v", s)
	}
	if len(s.Machines) != 1 || s.Machines[0].Status != "ready" {
		t.Errorf("what Nomad knows should still be shown: %+v", s.Machines)
	}
	if hasProblem(s, "box0", "not reporting") != nil {
		t.Error("with metrics unreadable, no machine should be blamed for not reporting")
	}

	s = build(now, nv, nil, nil, errMetricsOff)
	if len(s.Problems) != 0 {
		t.Errorf("metrics switched off is not a problem: %+v", s.Problems)
	}

	s = build(now, nil, errors.New("no leader"), nil, errMetricsOff)
	if p := hasProblem(s, "nomad", "no leader"); p == nil || p.Level != LevelCrit {
		t.Errorf("an unreadable scheduler is critical: %+v", s.Problems)
	}
}

func TestBytesHuman(t *testing.T) {
	for n, want := range map[int64]string{512: "512B", 1536: "1.5K", 15 * gib: "15G", 1 << 20: "1.0M"} {
		if got := BytesHuman(n); got != want {
			t.Errorf("BytesHuman(%d) = %q, want %q", n, got, want)
		}
	}
}
