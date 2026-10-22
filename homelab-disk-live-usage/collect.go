package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---- raw counters (one sample = one pass over /proc and cgroupfs) ----

type diskRaw struct{ rIOs, rSect, wIOs, wSect, ticks uint64 }
type ioRaw struct{ r, w uint64 }

type sample struct {
	t                time.Time
	disks            map[string]diskRaw
	cpuTotal, iowait uint64
	pswpin, pswpout  uint64
	mem              map[string]uint64 // selected /proc/meminfo fields, kB
	cgroups          map[string]ioRaw  // cgroup dir -> io.stat sums
	procs            map[int]ioRaw     // pid -> /proc/<pid>/io
	procsSeen        int
	procsDenied      int
}

// ---- JSON snapshot sent to the UI (rates in bytes/s) ----

type DiskStat struct {
	Name      string  `json:"name"`
	Read      float64 `json:"read"`
	Write     float64 `json:"write"`
	ReadIOPS  float64 `json:"riops"`
	WriteIOPS float64 `json:"wiops"`
	Util      float64 `json:"util"` // percent of wall time the device was busy
}

type GroupStat struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"` // "app" (Docker containers) or "service" (systemd unit)
	Containers []string `json:"containers,omitempty"`
	Read       float64  `json:"read"`
	Write      float64  `json:"write"`
}

type ProcStat struct {
	PID   int     `json:"pid"`
	Name  string  `json:"name"`
	Cmd   string  `json:"cmd"`
	Group string  `json:"group"`
	Read  float64 `json:"read"`
	Write float64 `json:"write"`
}

type Snapshot struct {
	Time        int64       `json:"t"`  // unix ms
	Interval    float64     `json:"dt"` // seconds covered by these rates
	Disks       []DiskStat  `json:"disks"`
	IOWait      float64     `json:"iowait"` // percent of CPU time
	SwapIn      float64     `json:"swapIn"`
	SwapOut     float64     `json:"swapOut"`
	SwapUsed    uint64      `json:"swapUsed"` // bytes
	SwapTotal   uint64      `json:"swapTotal"`
	MemAvail    uint64      `json:"memAvail"`
	MemTotal    uint64      `json:"memTotal"`
	Groups      []GroupStat `json:"groups"`
	Procs       []ProcStat  `json:"procs"`
	ProcsSeen   int         `json:"procsSeen"`
	ProcsDenied int         `json:"procsDenied"`
	Notes       []string    `json:"notes"`
}

// ---- collector ----

type procMeta struct{ name, cmd, cid, unit string }

type collector struct {
	proc, sys, cg string
	docker        *dockerClient
	topN          int
	pageSize      uint64

	isDisk      map[string]bool
	meta        map[int]*procMeta
	containers  map[string]*container // by full id
	services    []string              // cgroup dirs of systemd services
	lastRefresh time.Time
	dockerErr   string
	buf         []byte
}

func newCollector(proc, sys, cg string, d *dockerClient, topN int) *collector {
	return &collector{
		proc: proc, sys: sys, cg: cg, docker: d, topN: topN,
		pageSize:   uint64(os.Getpagesize()),
		isDisk:     map[string]bool{},
		meta:       map[int]*procMeta{},
		containers: map[string]*container{},
		buf:        make([]byte, 4096),
	}
}

// readSmall reads a small pseudo-file into the shared buffer (no allocation).
func (c *collector) readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n, err := f.Read(c.buf)
	if n == 0 && err != nil && err != io.EOF { // empty file (e.g. idle io.stat) is fine
		return nil, err
	}
	return c.buf[:n], nil
}

func (c *collector) sample() *sample {
	s := &sample{
		t:       time.Now(),
		disks:   map[string]diskRaw{},
		mem:     map[string]uint64{},
		cgroups: map[string]ioRaw{},
		procs:   make(map[int]ioRaw, 256),
	}
	c.readDiskstats(s)
	c.readCPU(s)
	c.readVMStat(s)
	c.readMeminfo(s)
	if time.Since(c.lastRefresh) > 5*time.Second {
		c.refreshGroups()
	}
	for _, ct := range c.containers {
		c.readIOStat(s, ct.cgDir)
	}
	for _, dir := range c.services {
		c.readIOStat(s, dir)
	}
	c.readProcs(s)
	return s
}

func (c *collector) readDiskstats(s *sample) {
	b, err := os.ReadFile(filepath.Join(c.proc, "diskstats"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 14 || !c.wholeDisk(f[2]) {
			continue
		}
		s.disks[f[2]] = diskRaw{
			rIOs: atou(f[3]), rSect: atou(f[5]),
			wIOs: atou(f[7]), wSect: atou(f[9]),
			ticks: atou(f[12]),
		}
	}
}

// wholeDisk keeps real block devices (sda, mmcblk0, nvme0n1, zram0...) and
// drops partitions, loop and ram devices. /sys/block is not namespaced.
func (c *collector) wholeDisk(name string) bool {
	if v, ok := c.isDisk[name]; ok {
		return v
	}
	v := false
	if !strings.HasPrefix(name, "loop") && !strings.HasPrefix(name, "ram") {
		_, err := os.Stat(filepath.Join(c.sys, "block", strings.ReplaceAll(name, "/", "!")))
		v = err == nil
	}
	c.isDisk[name] = v
	return v
}

func (c *collector) readCPU(s *sample) {
	b, err := c.readSmall(filepath.Join(c.proc, "stat")) // first line is all we need
	if err != nil {
		return
	}
	line, _, _ := bytes.Cut(b, []byte("\n"))
	f := strings.Fields(string(line))
	if len(f) < 9 || f[0] != "cpu" {
		return
	}
	for _, v := range f[1:9] { // user..steal; guest is already counted in user
		s.cpuTotal += atou(v)
	}
	s.iowait = atou(f[5])
}

func (c *collector) readVMStat(s *sample) {
	b, err := os.ReadFile(filepath.Join(c.proc, "vmstat"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(line, " ")
		switch k {
		case "pswpin":
			s.pswpin = atou(v)
		case "pswpout":
			s.pswpout = atou(v)
		}
	}
}

func (c *collector) readMeminfo(s *sample) {
	b, err := os.ReadFile(filepath.Join(c.proc, "meminfo"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch k {
		case "MemTotal", "MemAvailable", "SwapTotal", "SwapFree":
			s.mem[k] = atou(strings.TrimSuffix(strings.TrimSpace(v), " kB"))
		}
	}
}

// readIOStat sums rbytes/wbytes over all devices in a cgroup v2 io.stat.
func (c *collector) readIOStat(s *sample, dir string) {
	if dir == "" {
		return
	}
	b, err := c.readSmall(filepath.Join(c.cg, dir, "io.stat"))
	if err != nil {
		return
	}
	var r ioRaw
	for _, line := range bytes.Split(b, []byte("\n")) {
		for _, kv := range bytes.Fields(line) {
			if v, ok := bytes.CutPrefix(kv, []byte("rbytes=")); ok {
				r.r += atou(string(v))
			} else if v, ok := bytes.CutPrefix(kv, []byte("wbytes=")); ok {
				r.w += atou(string(v))
			}
		}
	}
	s.cgroups[dir] = r
}

func (c *collector) readProcs(s *sample) {
	d, err := os.Open(c.proc)
	if err != nil {
		return
	}
	names, _ := d.Readdirnames(-1)
	d.Close()
	for _, n := range names {
		pid, err := strconv.Atoi(n)
		if err != nil {
			continue
		}
		s.procsSeen++
		b, err := c.readSmall(c.proc + "/" + n + "/io")
		if err != nil {
			if os.IsPermission(err) {
				s.procsDenied++
			}
			continue
		}
		var r ioRaw
		for _, line := range bytes.Split(b, []byte("\n")) {
			if v, ok := bytes.CutPrefix(line, []byte("read_bytes: ")); ok {
				r.r = atou(string(v))
			} else if v, ok := bytes.CutPrefix(line, []byte("write_bytes: ")); ok {
				r.w = atou(string(v))
			}
		}
		s.procs[pid] = r
	}
}

// refreshGroups re-reads the container list and systemd services (every 5 s).
func (c *collector) refreshGroups() {
	c.lastRefresh = time.Now()
	list, err := c.docker.containers()
	if err != nil {
		c.dockerErr = err.Error()
	} else {
		c.dockerErr = ""
		next := make(map[string]*container, len(list))
		for _, ct := range list {
			if old, ok := c.containers[ct.id]; ok {
				ct.cgDir = old.cgDir
			} else {
				ct.cgDir = c.findCgroup(ct.id)
			}
			next[ct.id] = ct
		}
		c.containers = next
	}

	c.services = c.services[:0]
	ents, _ := os.ReadDir(filepath.Join(c.cg, "system.slice"))
	for _, e := range ents {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".service") {
			c.services = append(c.services, "system.slice/"+e.Name())
		}
	}
}

// findCgroup locates a container's cgroup: systemd driver, cgroupfs driver,
// then a shallow walk for anything else (e.g. Docker Desktop's VM).
func (c *collector) findCgroup(id string) string {
	for _, rel := range []string{"system.slice/docker-" + id + ".scope", "docker/" + id} {
		if _, err := os.Stat(filepath.Join(c.cg, rel)); err == nil {
			return rel
		}
	}
	found := ""
	filepath.WalkDir(c.cg, func(path string, e fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return fs.SkipDir
		}
		if !e.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(c.cg, path)
		if strings.Contains(e.Name(), id) {
			found = rel
			return fs.SkipAll
		}
		if strings.Count(rel, "/") >= 3 {
			return fs.SkipDir
		}
		return nil
	})
	return found
}

// ---- deltas ----

func (c *collector) diff(prev, cur *sample) *Snapshot {
	dt := cur.t.Sub(prev.t).Seconds()
	rate := func(a, b uint64) float64 {
		if b < a { // counter reset (container restart, pid reuse)
			return 0
		}
		return float64(b-a) / dt
	}
	snap := &Snapshot{Time: cur.t.UnixMilli(), Interval: dt,
		Disks: []DiskStat{}, Groups: []GroupStat{}, Procs: []ProcStat{}, Notes: []string{}}

	for name, d := range cur.disks {
		p, ok := prev.disks[name]
		if !ok || d.rIOs+d.wIOs == 0 {
			continue
		}
		util := rate(p.ticks, d.ticks) / 10 // ms busy per second -> percent
		if util > 100 {
			util = 100
		}
		snap.Disks = append(snap.Disks, DiskStat{
			Name: name,
			Read: rate(p.rSect, d.rSect) * 512, Write: rate(p.wSect, d.wSect) * 512,
			ReadIOPS: rate(p.rIOs, d.rIOs), WriteIOPS: rate(p.wIOs, d.wIOs),
			Util: util,
		})
	}
	sort.Slice(snap.Disks, func(i, j int) bool { return snap.Disks[i].Name < snap.Disks[j].Name })

	// iowait may go backwards on some kernels, so guard both counters.
	if tot := cur.cpuTotal - prev.cpuTotal; cur.cpuTotal > prev.cpuTotal && cur.iowait >= prev.iowait {
		snap.IOWait = 100 * float64(cur.iowait-prev.iowait) / float64(tot)
	}
	snap.SwapIn = rate(prev.pswpin, cur.pswpin) * float64(c.pageSize)
	snap.SwapOut = rate(prev.pswpout, cur.pswpout) * float64(c.pageSize)
	snap.MemTotal = cur.mem["MemTotal"] * 1024
	snap.MemAvail = cur.mem["MemAvailable"] * 1024
	snap.SwapTotal = cur.mem["SwapTotal"] * 1024
	if st, sf := cur.mem["SwapTotal"], cur.mem["SwapFree"]; st >= sf {
		snap.SwapUsed = (st - sf) * 1024
	}

	// Apps: containers grouped by Umbrel app id.
	apps := map[string]*GroupStat{}
	missingCg, missingStat := 0, 0
	for _, ct := range c.containers {
		g := apps[ct.app]
		if g == nil {
			g = &GroupStat{ID: ct.app, Kind: "app"}
			apps[ct.app] = g
		}
		g.Containers = append(g.Containers, ct.service)
		if ct.cgDir == "" {
			missingCg++
			continue
		}
		a, ok1 := prev.cgroups[ct.cgDir]
		b, ok2 := cur.cgroups[ct.cgDir]
		if !ok2 {
			missingStat++
		}
		if ok1 && ok2 {
			g.Read += rate(a.r, b.r)
			g.Write += rate(a.w, b.w)
		}
	}
	for _, g := range apps {
		sort.Strings(g.Containers)
		snap.Groups = append(snap.Groups, *g)
	}
	// Host services (umbrel.service = umbreld, docker, ...): only when busy.
	for _, dir := range c.services {
		a, ok1 := prev.cgroups[dir]
		b, ok2 := cur.cgroups[dir]
		if !ok1 || !ok2 {
			continue
		}
		g := GroupStat{ID: strings.TrimSuffix(filepath.Base(dir), ".service"), Kind: "service", Read: rate(a.r, b.r), Write: rate(a.w, b.w)}
		if g.Read+g.Write > 0 {
			snap.Groups = append(snap.Groups, g)
		}
	}
	sort.Slice(snap.Groups, func(i, j int) bool {
		a, b := snap.Groups[i], snap.Groups[j]
		if ta, tb := a.Read+a.Write, b.Read+b.Write; ta != tb {
			return ta > tb
		}
		return a.ID < b.ID
	})

	// Processes: top N by bytes moved this interval.
	type cand struct {
		pid  int
		r, w float64
	}
	var cands []cand
	for pid, b := range cur.procs {
		if a, ok := prev.procs[pid]; ok {
			if r, w := rate(a.r, b.r), rate(a.w, b.w); r+w > 0 {
				cands = append(cands, cand{pid, r, w})
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].r+cands[i].w > cands[j].r+cands[j].w })
	if len(cands) > c.topN {
		cands = cands[:c.topN]
	}
	for _, k := range cands {
		m := c.procMeta(k.pid)
		snap.Procs = append(snap.Procs, ProcStat{PID: k.pid, Name: m.name, Cmd: m.cmd, Group: c.groupName(m), Read: k.r, Write: k.w})
	}
	for pid := range c.meta {
		if _, ok := cur.procs[pid]; !ok {
			delete(c.meta, pid)
		}
	}
	snap.ProcsSeen, snap.ProcsDenied = cur.procsSeen, cur.procsDenied

	if c.dockerErr != "" {
		snap.Notes = append(snap.Notes, "Docker API unavailable, apps not shown: "+c.dockerErr)
	}
	if _, err := os.Stat(c.cg); err != nil {
		snap.Notes = append(snap.Notes, "cgroup root "+c.cg+" not mounted: per-app I/O unavailable")
	} else {
		if missingCg > 0 {
			snap.Notes = append(snap.Notes, fmt.Sprintf("%d container(s) without a cgroup directory", missingCg))
		}
		if missingStat > 0 {
			snap.Notes = append(snap.Notes, fmt.Sprintf("%d container(s) without io.stat (io controller not enabled?)", missingStat))
		}
	}
	if cur.procsDenied > 0 {
		snap.Notes = append(snap.Notes, fmt.Sprintf("%d of %d processes unreadable: needs pid: host, root, CAP_SYS_PTRACE (and AppArmor unconfined)", cur.procsDenied, cur.procsSeen))
	}
	return snap
}

func (c *collector) procMeta(pid int) *procMeta {
	if m, ok := c.meta[pid]; ok {
		return m
	}
	dir := c.proc + "/" + strconv.Itoa(pid)
	m := &procMeta{}
	if b, err := os.ReadFile(dir + "/comm"); err == nil {
		m.name = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(dir + "/cmdline"); err == nil {
		m.cmd = strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
		if len(m.cmd) > 200 {
			m.cmd = m.cmd[:200] + "…"
		}
	}
	if m.cmd == "" {
		m.cmd = "[" + m.name + "]" // kernel thread
	}
	// cgroup path is relative to our cgroup namespace (may start with /../..),
	// but still contains the container id or the systemd unit name.
	if b, err := os.ReadFile(dir + "/cgroup"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			path, ok := strings.CutPrefix(line, "0::")
			if !ok {
				continue
			}
			if id := containerID(path); id != "" {
				m.cid = id
			} else {
				for _, seg := range strings.Split(path, "/") {
					if strings.HasSuffix(seg, ".service") {
						m.unit = strings.TrimSuffix(seg, ".service")
					}
				}
			}
		}
	}
	c.meta[pid] = m
	return m
}

func (c *collector) groupName(m *procMeta) string {
	if m.cid != "" {
		if ct, ok := c.containers[m.cid]; ok {
			return ct.app + "/" + ct.service
		}
		return "container " + m.cid[:12]
	}
	if m.unit != "" {
		return m.unit + ".service"
	}
	return ""
}

// containerID finds a 64-char hex run in a cgroup path.
func containerID(path string) string {
	run := 0
	for i := 0; i < len(path); i++ {
		ch := path[i]
		if ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' {
			run++
			if run == 64 && (i+1 == len(path) || !isHex(path[i+1])) {
				return path[i-63 : i+1]
			}
		} else {
			run = 0
		}
	}
	return ""
}

func isHex(ch byte) bool { return ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' }

func atou(s string) uint64 {
	v, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return v
}
