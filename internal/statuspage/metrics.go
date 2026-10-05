package statuspage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
)

// What the page asks the metric store. Host series come from the node
// exporter on each machine, labeled node=<name>; allocation series from each
// machine's Nomad agent, per task, labeled with the allocation.
const (
	hostSel = `job="nodes"`

	qUp         = `up{` + hostSel + `}`
	qCPU        = `100 * (1 - avg by (node) (rate(node_cpu_seconds_total{` + hostSel + `,mode="idle"}[2m])))`
	qCores      = `count by (node) (node_cpu_seconds_total{` + hostSel + `,mode="idle"})`
	qLoad1      = `node_load1{` + hostSel + `}`
	qMemTotal   = `node_memory_MemTotal_bytes{` + hostSel + `}`
	qMemAvail   = `node_memory_MemAvailable_bytes{` + hostSel + `}`
	qMemPercent = `100 * (1 - node_memory_MemAvailable_bytes{` + hostSel + `} / node_memory_MemTotal_bytes{` + hostSel + `})`
	qSwapTotal  = `node_memory_SwapTotal_bytes{` + hostSel + `}`
	qSwapFree   = `node_memory_SwapFree_bytes{` + hostSel + `}`
	qUptime     = `node_time_seconds{` + hostSel + `} - node_boot_time_seconds{` + hostSel + `}`
	qReboot     = `orca_reboot_required{` + hostSel + `}`

	// Real filesystems. Memory-backed ones are not disks, and the node
	// exporter already leaves out Docker's layers and Nomad's allocation
	// mounts.
	fsSel        = `{` + hostSel + `,fstype!~"tmpfs|ramfs|devtmpfs|overlay|squashfs|nsfs|fuse.lxcfs"}`
	qFSSize      = `node_filesystem_size_bytes` + fsSel
	qFSAvail     = `node_filesystem_avail_bytes` + fsSel
	qFSFiles     = `node_filesystem_files` + fsSel
	qFSFilesFree = `node_filesystem_files_free` + fsSel

	// The machine's own interfaces. Bridges and veths carry container
	// traffic that also crosses the real interface, so counting them would
	// count it twice.
	netSel = `{` + hostSel + `,device!~"lo|veth.*|docker.*|br-.*|nomad|cni.*|virbr.*"}`
	qRx    = `sum by (node) (rate(node_network_receive_bytes_total` + netSel + `[2m]))`
	qTx    = `sum by (node) (rate(node_network_transmit_bytes_total` + netSel + `[2m]))`

	// The stores report on their own disks, caps and floors included, so
	// the page is not told the settings a second time.
	qLogsUsed     = `sum(vl_data_size_bytes)`
	qLogsCap      = `max(vl_max_disk_space_usage_bytes)`
	qLogsFree     = `max(vl_free_disk_space_bytes)`
	qLogsRO       = `max(vl_storage_is_read_only)`
	qMetricsUsed  = `sum(vm_data_size_bytes)`
	qMetricsFree  = `max(vm_free_disk_space_bytes)`
	qMetricsFloor = `max(vm_free_disk_space_limit_bytes)`
	qMetricsRO    = `max(vm_storage_is_read_only)`

	qAllocCPU = `sum by (alloc_id) (nomad_client_allocs_cpu_total_percent)`

	// An allocation's usage counts the files the kernel is caching for it,
	// its own binary included, and the limit counts them too. But the kernel
	// gives those up before it kills anything, so a small service with a
	// large binary sits at its limit from the moment it starts on a cold
	// machine and is in no danger. What is judged is what cannot be given up.
	//
	// The cache is still asked for and shown beside it, because shared memory
	// is counted as cache and cannot be given up either: a database's shared
	// buffers are in that figure and nowhere else.
	qAllocMem      = `sum by (alloc_id) (nomad_client_allocs_memory_usage - nomad_client_allocs_memory_cache)`
	qAllocMemCache = `sum by (alloc_id) (nomad_client_allocs_memory_cache)`
	qAllocMemLimit = `sum by (alloc_id) (nomad_client_allocs_memory_allocated)`
)

const (
	historyWindow = time.Hour
	historyStep   = time.Minute
)

// metricsClient queries VictoriaMetrics' Prometheus API.
type metricsClient struct {
	base string
	http *http.Client
}

type sample struct {
	labels map[string]string
	value  float64
}

type series struct {
	labels map[string]string
	points []Point
}

func (c *metricsClient) query(ctx context.Context, expr string) ([]sample, error) {
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/api/v1/query", url.Values{"query": {expr}}, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("query %s: %s", expr, resp.Error)
	}
	out := make([]sample, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		v, ok := promValue(r.Value[1])
		if !ok {
			continue
		}
		out = append(out, sample{labels: r.Metric, value: v})
	}
	return out, nil
}

func (c *metricsClient) queryRange(ctx context.Context, expr string, end time.Time) ([]series, error) {
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	params := url.Values{
		"query": {expr},
		"start": {strconv.FormatInt(end.Add(-historyWindow).Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.Itoa(int(historyStep.Seconds()))},
	}
	if err := c.get(ctx, "/api/v1/query_range", params, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("query %s: %s", expr, resp.Error)
	}
	out := make([]series, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		s := series{labels: r.Metric}
		for _, pv := range r.Values {
			ts, okT := pv[0].(float64)
			v, okV := promValue(pv[1])
			if okT && okV {
				s.points = append(s.points, Point{T: int64(ts), V: v})
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func (c *metricsClient) get(ctx context.Context, path string, params url.Values, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// promValue reads a sample value, which the API sends as a string so that
// NaN and infinities survive JSON.
func promValue(v any) (float64, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f != f { // NaN
		return 0, false
	}
	return f, true
}

// fetchMetrics asks every question at once. Any failure is the store being
// unreachable or broken, and the whole view is reported unavailable rather
// than shown with holes that look like zeroes.
func fetchMetrics(ctx context.Context, c *metricsClient, now time.Time) (*metricsView, error) {
	instant := []string{
		qUp, qCPU, qCores, qLoad1, qMemTotal, qMemAvail, qSwapTotal, qSwapFree, qUptime, qReboot,
		qFSSize, qFSAvail, qFSFiles, qFSFilesFree, qRx, qTx,
		qAllocCPU, qAllocMem, qAllocMemCache, qAllocMemLimit,
		qLogsUsed, qLogsCap, qLogsFree, qLogsRO, qMetricsUsed, qMetricsFree, qMetricsFloor, qMetricsRO,
	}
	ranged := []string{qCPU, qMemPercent}

	results := map[string][]sample{}
	history := map[string][]series{}
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	for _, q := range instant {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.query(ctx, q)
			if err != nil {
				fail(err)
				return
			}
			mu.Lock()
			results[q] = r
			mu.Unlock()
		}()
	}
	for _, q := range ranged {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.queryRange(ctx, q, now)
			if err != nil {
				fail(err)
				return
			}
			mu.Lock()
			history[q] = r
			mu.Unlock()
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return assembleMetrics(results, history), nil
}

// assembleMetrics turns query results into per-machine and per-allocation
// numbers. Separate from fetching so it can be tested on canned results.
func assembleMetrics(results map[string][]sample, history map[string][]series) *metricsView {
	mv := &metricsView{
		Machines:        map[string]*machineMetrics{},
		AllocCPUPercent: byLabel(results[qAllocCPU], "alloc_id"),
		AllocMemUsed:    byLabel(results[qAllocMem], "alloc_id"),
		AllocMemCache:   byLabel(results[qAllocMemCache], "alloc_id"),
		AllocMemLimit:   byLabel(results[qAllocMemLimit], "alloc_id"),
	}
	machine := func(name string) *machineMetrics {
		m, ok := mv.Machines[name]
		if !ok {
			m = &machineMetrics{}
			mv.Machines[name] = m
		}
		return m
	}

	for _, s := range results[qUp] {
		if n := s.labels["node"]; n != "" {
			machine(n).Up = s.value == 1
		}
	}
	perNode := func(q string, set func(*machineMetrics, float64)) {
		for _, s := range results[q] {
			if n := s.labels["node"]; n != "" {
				set(machine(n), s.value)
			}
		}
	}
	ptr := func(v float64) *float64 { return &v }
	perNode(qCPU, func(m *machineMetrics, v float64) { m.CPUPercent = ptr(v) })
	perNode(qCores, func(m *machineMetrics, v float64) { m.Cores = int(v) })
	perNode(qLoad1, func(m *machineMetrics, v float64) { m.Load1 = ptr(v) })
	perNode(qMemTotal, func(m *machineMetrics, v float64) { m.MemTotal = ptr(v) })
	perNode(qMemAvail, func(m *machineMetrics, v float64) { m.MemAvailable = ptr(v) })
	perNode(qSwapTotal, func(m *machineMetrics, v float64) { m.SwapTotal = ptr(v) })
	perNode(qSwapFree, func(m *machineMetrics, v float64) { m.SwapFree = ptr(v) })
	perNode(qUptime, func(m *machineMetrics, v float64) { m.Uptime = ptr(v) })
	perNode(qRx, func(m *machineMetrics, v float64) { m.Rx = ptr(v) })
	perNode(qTx, func(m *machineMetrics, v float64) { m.Tx = ptr(v) })
	perNode(qReboot, func(m *machineMetrics, v float64) { m.RebootRequired = v == 1 })

	one := func(q string) *float64 {
		if r := results[q]; len(r) > 0 {
			return ptr(r[0].value)
		}
		return nil
	}
	mv.Stores = map[string]*storeMetrics{
		"logs":    {Used: one(qLogsUsed), Cap: one(qLogsCap), Free: one(qLogsFree), ReadOnly: one(qLogsRO)},
		"metrics": {Used: one(qMetricsUsed), Free: one(qMetricsFree), MinFree: one(qMetricsFloor), ReadOnly: one(qMetricsRO)},
	}

	for node, disks := range assembleDisks(results) {
		machine(node).Disks = disks
	}

	for _, s := range history[qCPU] {
		if n := s.labels["node"]; n != "" {
			machine(n).CPUHistory = s.points
		}
	}
	for _, s := range history[qMemPercent] {
		if n := s.labels["node"]; n != "" {
			machine(n).MemoryHist = s.points
		}
	}
	return mv
}

// assembleDisks joins the filesystem series into one Disk per device per
// machine. A device mounted in several places (a bind mount) is one disk,
// shown at its shortest mount point.
func assembleDisks(results map[string][]sample) map[string][]Disk {
	type key struct{ node, mount string }
	values := func(q string) map[key]float64 {
		out := map[key]float64{}
		for _, s := range results[q] {
			out[key{s.labels["node"], s.labels["mountpoint"]}] = s.value
		}
		return out
	}
	avail, files, filesFree := values(qFSAvail), values(qFSFiles), values(qFSFilesFree)

	type devKey struct{ node, device string }
	chosen := map[devKey]Disk{}
	for _, s := range results[qFSSize] {
		node, mount, device := s.labels["node"], s.labels["mountpoint"], s.labels["device"]
		k := key{node, mount}
		a, ok := avail[k]
		if node == "" || !ok || s.value <= 0 {
			continue
		}
		d := Disk{Mount: mount, Device: device, FSType: s.labels["fstype"], Usage: diskUsage(s.value, a)}
		if total, ok := files[k]; ok && total > 0 {
			p := 100 * (total - filesFree[k]) / total
			d.Inodes = &Gauge{Percent: p, Level: percentLevel(p, fullWarn, fullCrit)}
		}
		dk := devKey{node, device}
		if prev, ok := chosen[dk]; !ok || len(mount) < len(prev.Mount) {
			chosen[dk] = d
		}
	}

	out := map[string][]Disk{}
	for dk, d := range chosen {
		out[dk.node] = append(out[dk.node], d)
	}
	for node := range out {
		sort.Slice(out[node], func(i, j int) bool { return out[node][i].Mount < out[node][j].Mount })
	}
	return out
}

func byLabel(samples []sample, label string) map[string]float64 {
	out := map[string]float64{}
	for _, s := range samples {
		if k := s.labels[label]; k != "" {
			out[k] += s.value
		}
	}
	return out
}
