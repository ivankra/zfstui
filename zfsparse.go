// Parses the machine-readable output of zpool/zfs commands. Hand-rolled on
// purpose. Discovery uses `zpool get -Hp all` / `zfs get -Hp all`: one line
// per (object, property, value, source), so EVERY property is available for
// detail views and adding a displayed field never changes the protocol.
// Typed struct fields are derived from the property maps in one place here.
package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// propTable is `* get -Hp all` output pivoted: object -> property -> value,
// plus the property source ("local", "inherited from X", "default", "-").
type propTable struct {
	order []string // object names in first-seen order
	vals  map[string]map[string]string
	srcs  map[string]map[string]string
}

// parsePropTable parses NAME\tPROPERTY\tVALUE\tSOURCE lines.
func parsePropTable(out string) (*propTable, error) {
	t := &propTable{vals: map[string]map[string]string{}, srcs: map[string]map[string]string{}}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 4)
		if len(f) < 3 {
			return nil, fmt.Errorf("malformed property line %q", line)
		}
		name, prop, val := f[0], f[1], f[2]
		src := ""
		if len(f) == 4 {
			src = f[3]
		}
		if t.vals[name] == nil {
			t.order = append(t.order, name)
			t.vals[name] = map[string]string{}
			t.srcs[name] = map[string]string{}
		}
		t.vals[name][prop] = val
		if src != "" && src != "-" {
			t.srcs[name][prop] = src
		}
	}
	return t, nil
}

// props wraps one object's property map with forgiving accessors.
type props map[string]string

// str: "-" means absent.
func (p props) str(k string) string {
	v := p[k]
	if v == "-" {
		return ""
	}
	return v
}

// num: numeric -p value; "-", "" and unparsable mean 0. Trailing "%" and "x"
// suffixes are tolerated (capacity, dedupratio on some versions).
func (p props) num(k string) int64 {
	v := strings.TrimRight(p.str(k), "%x")
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		// e.g. dedupratio "1.00": take the integer part
		if f, ferr := strconv.ParseFloat(v, 64); ferr == nil {
			return int64(f)
		}
		return 0
	}
	return n
}

func (p props) boolish(k string) bool {
	v := p[k]
	return v == "yes" || v == "on"
}

// ParsePools parses `zpool get -Hp all` output.
func ParsePools(host, out string) ([]Pool, error) {
	t, err := parsePropTable(out)
	if err != nil {
		return nil, fmt.Errorf("pools: %w", err)
	}
	var pools []Pool
	for _, name := range t.order {
		p := props(t.vals[name])
		pools = append(pools, Pool{
			Host: host, Name: name,
			GUID: p.str("guid"),
			Size: p.num("size"), Alloc: p.num("allocated"), Free: p.num("free"),
			Cap: p.num("capacity"), Frag: p.num("fragmentation"),
			Dedup: p.str("dedupratio"), Health: p.str("health"),
			Props: t.vals[name], PropSrc: t.srcs[name],
		})
	}
	return pools, nil
}

// ParseDatasets parses `zfs get -Hp -t filesystem,volume all` output.
func ParseDatasets(host, out string) ([]Dataset, error) {
	t, err := parsePropTable(out)
	if err != nil {
		return nil, fmt.Errorf("datasets: %w", err)
	}
	var dss []Dataset
	for _, name := range t.order {
		dss = append(dss, datasetFromProps(host, name, t.vals[name], t.srcs[name]))
	}
	return dss, nil
}

// datasetFromProps derives the typed fields — THE single mapping, shared by
// the parser and the sqlite load path.
func datasetFromProps(host, name string, vals, srcs map[string]string) Dataset {
	p := props(vals)
	return Dataset{
		Host: host, Name: name,
		Type: p.str("type"), GUID: p.str("guid"),
		Used: p.num("used"), Avail: p.num("available"), Refer: p.num("referenced"),
		Creation: p.num("creation"), CreateTXG: p.num("createtxg"), Written: p.num("written"),
		Origin: p.str("origin"), Encryption: p.str("encryption"),
		KeyStatus: p.str("keystatus"), KeyLocation: p.str("keylocation"),
		Mounted: p.boolish("mounted"), CanMount: p.str("canmount"),
		Mountpoint: p.str("mountpoint"), ReadOnly: p.boolish("readonly"),
		ResumeToken: p.str("receive_resume_token"), Compression: p.str("compression"),
		RecordSize: p.num("recordsize"),
		Props:      vals, PropSrc: srcs,
	}
}

// ParseSnapshots parses `zfs get -Hp -t snapshot all` output.
func ParseSnapshots(host, out string) ([]Snapshot, error) {
	t, err := parsePropTable(out)
	if err != nil {
		return nil, fmt.Errorf("snapshots: %w", err)
	}
	var snaps []Snapshot
	for _, full := range t.order {
		ds, name, ok := strings.Cut(full, "@")
		if !ok {
			return nil, fmt.Errorf("snapshot name without '@': %q", full)
		}
		snaps = append(snaps, snapshotFromProps(host, ds, name, t.vals[full], t.srcs[full]))
	}
	return snaps, nil
}

func snapshotFromProps(host, ds, name string, vals, srcs map[string]string) Snapshot {
	p := props(vals)
	return Snapshot{
		Host: host, Dataset: ds, Name: name,
		GUID: p.str("guid"),
		Used: p.num("used"), Refer: p.num("referenced"),
		Creation: p.num("creation"), CreateTXG: p.num("createtxg"),
		Props: vals, PropSrc: srcs,
	}
}

// Section markers used to batch all discovery commands into one ssh round trip.
const (
	MarkPools     = "===ZFSTUI:pools==="
	MarkDisks     = "===ZFSTUI:disks==="
	MarkDatasets  = "===ZFSTUI:datasets==="
	MarkSnapshots = "===ZFSTUI:snapshots==="
	MarkJobs      = "===ZFSTUI:jobs==="
	MarkRC        = "===ZFSTUI:rc=" // followed by "<n>==="
)

// jobsProbe dumps every /tmp/zfstui-job-* dir: status, meta, last progress
// line and expected size. Also used standalone for live progress polling.
const jobsProbe = `for d in /tmp/zfstui-job-*; do [ -d "$d" ] || continue; ` +
	`echo "==J== $d"; head -n1 "$d/status" 2>/dev/null; ` +
	`echo "==M=="; cat "$d/meta.json" 2>/dev/null; echo; ` +
	`echo "==P=="; tail -n1 "$d/progress" 2>/dev/null; echo; ` +
	`echo "==E=="; head -n1 "$d/expected" 2>/dev/null; echo; done; true`

// DiscoverCommand returns the shell command run on each host during refresh.
// Each section echoes its marker, runs one command, and records its exit code
// so partial failures (e.g. no zpool binary) are attributable.
func DiscoverCommand(sudo bool) string {
	pfx := ""
	if sudo {
		pfx = "sudo -n "
	}
	section := func(mark, cmd string) string {
		return fmt.Sprintf("echo '%s'; %s%s; echo '%s'$?'==='", mark, pfx, cmd, MarkRC)
	}
	// Disks: per pool, vdev properties (OpenZFS ≥ 2.2; tolerated if absent)
	// + verbatim `zpool status`; then SMART for every physical disk
	// (smartctl handles SATA and NVMe; skipped if not installed).
	disksProbe := `for p in $(` + pfx + `zpool list -H -o name); do ` +
		`echo "==P== $p"; ` + pfx + `zpool get -Hp all "$p" all-vdevs 2>/dev/null; ` +
		`echo "==S=="; ` + pfx + `zpool status "$p" 2>/dev/null; done; ` +
		`echo "==SMART=="; if command -v smartctl >/dev/null 2>&1; then ` +
		`for d in $(lsblk -dno NAME,TYPE 2>/dev/null | awk '$2=="disk"{print $1}'); do ` +
		`echo "==D== /dev/$d"; ` + pfx + `smartctl -a "/dev/$d" 2>&1 | head -c 20000; echo; done; ` +
		`else echo "==NOSMART== smartctl not installed"; fi; true`
	return strings.Join([]string{
		section(MarkPools, "zpool get -Hp all"),
		fmt.Sprintf("echo '%s'; %s; echo '%s'$?'==='", MarkDisks, disksProbe, MarkRC),
		section(MarkDatasets, "zfs get -Hp -t filesystem,volume all"),
		section(MarkSnapshots, "zfs get -Hp -t snapshot all"),
		// Jobs need no sudo: dirs are owned by the ssh user that made them.
		fmt.Sprintf("echo '%s'; %s; echo '%s'$?'==='", MarkJobs, jobsProbe, MarkRC),
	}, "; ")
}

// ParseDiscovery splits combined DiscoverCommand output and parses each section.
func ParseDiscovery(host, out string) (HostState, error) {
	st := HostState{Host: host}
	sections := map[string]*strings.Builder{}
	var cur *strings.Builder
	curMark := ""
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == MarkPools || trimmed == MarkDisks || trimmed == MarkDatasets || trimmed == MarkSnapshots || trimmed == MarkJobs:
			curMark = trimmed
			cur = &strings.Builder{}
			sections[curMark] = cur
		case strings.HasPrefix(trimmed, MarkRC):
			rc := strings.TrimSuffix(strings.TrimPrefix(trimmed, MarkRC), "===")
			if rc != "0" {
				return st, fmt.Errorf("%s: discovery section %s exited with %s", host, curMark, rc)
			}
			cur = nil
		case cur != nil:
			cur.WriteString(line)
			cur.WriteString("\n")
		}
	}
	for _, mark := range []string{MarkPools, MarkDisks, MarkDatasets, MarkSnapshots, MarkJobs} {
		if sections[mark] == nil {
			return st, fmt.Errorf("%s: discovery output missing section %s", host, mark)
		}
	}
	var err error
	if st.Pools, err = ParsePools(host, sections[MarkPools].String()); err != nil {
		return st, err
	}
	if st.Datasets, err = ParseDatasets(host, sections[MarkDatasets].String()); err != nil {
		return st, err
	}
	if st.Snapshots, err = ParseSnapshots(host, sections[MarkSnapshots].String()); err != nil {
		return st, err
	}
	st.Jobs = ParseJobs(host, sections[MarkJobs].String())
	var statusTexts map[string]string
	st.VDevs, statusTexts, st.Smart = ParseDisks(host, sections[MarkDisks].String())
	// Attach verbatim `zpool status` to the pool as a synthetic property
	// (underscore prefix: excluded from generic dumps, persists for free).
	for i := range st.Pools {
		if txt := statusTexts[st.Pools[i].Name]; txt != "" {
			if st.Pools[i].Props == nil {
				st.Pools[i].Props = map[string]string{}
			}
			st.Pools[i].Props["_zpool-status"] = txt
		}
	}
	return st, nil
}

// vdevStateNames: `zpool get -Hp` reports vdev state numerically (the
// vdev_state enum); map it back to the familiar words.
var vdevStateNames = map[string]string{
	"0": "UNKNOWN", "1": "CLOSED", "2": "OFFLINE", "3": "REMOVED",
	"4": "CANT_OPEN", "5": "FAULTED", "6": "DEGRADED", "7": "ONLINE",
}

// vdevFromProps derives typed vdev fields (shared with the store).
func vdevFromProps(host, pool, name string, vals, srcs map[string]string) VDev {
	p := props(vals)
	state := p.str("state")
	if s, ok := vdevStateNames[state]; ok {
		state = s
	}
	return VDev{
		Host: host, Pool: pool, Name: name,
		Parent: p.str("parent"), State: state, Path: p.str("path"),
		Size: p.num("size"), Alloc: p.num("allocated"), Free: p.num("free"),
		Frag: p.num("fragmentation"),
		RErr: p.num("read_errors"), WErr: p.num("write_errors"), CErr: p.num("checksum_errors"),
		Props: vals, PropSrc: srcs,
	}
}

func (v VDev) toProps() map[string]string {
	if v.Props != nil {
		return v.Props
	}
	return map[string]string{
		"parent": v.Parent, "state": v.State, "path": v.Path,
		"size": fmt.Sprint(v.Size), "allocated": fmt.Sprint(v.Alloc),
		"free": fmt.Sprint(v.Free), "fragmentation": fmt.Sprint(v.Frag),
		"read_errors": fmt.Sprint(v.RErr), "write_errors": fmt.Sprint(v.WErr),
		"checksum_errors": fmt.Sprint(v.CErr),
	}
}

// ParseDisks parses the disks discovery section: per-pool vdev property
// tables and verbatim zpool status, then per-disk SMART reports.
func ParseDisks(host, out string) (vdevs []VDev, statusTexts map[string]string, smart []SmartReport) {
	statusTexts = map[string]string{}
	pool := ""
	mode := "" // vdevs | status | smart
	var buf strings.Builder
	var propBuf strings.Builder
	var smartDev string
	flushPool := func() {
		if pool == "" {
			return
		}
		if t, err := parsePropTable(propBuf.String()); err == nil {
			for _, name := range t.order {
				vdevs = append(vdevs, vdevFromProps(host, pool, name, t.vals[name], t.srcs[name]))
			}
		}
		if s := strings.TrimSpace(buf.String()); s != "" {
			statusTexts[pool] = s
		}
		pool = ""
		propBuf.Reset()
		buf.Reset()
	}
	flushSmart := func() {
		if smartDev == "" {
			return
		}
		if s := strings.TrimSpace(buf.String()); s != "" {
			smart = append(smart, SmartReport{Host: host, Dev: smartDev, Text: s})
		}
		smartDev = ""
		buf.Reset()
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "==P== "):
			flushPool()
			flushSmart()
			pool = strings.TrimSpace(strings.TrimPrefix(line, "==P== "))
			mode = "vdevs"
		case line == "==S==":
			mode = "status"
		case line == "==SMART==":
			flushPool()
			mode = "smart"
		case strings.HasPrefix(line, "==D== "):
			flushSmart()
			smartDev = strings.TrimSpace(strings.TrimPrefix(line, "==D== "))
		case strings.HasPrefix(line, "==NOSMART=="):
			// informational only
		default:
			switch mode {
			case "vdevs":
				propBuf.WriteString(line)
				propBuf.WriteString("\n")
			case "status":
				buf.WriteString(line)
				buf.WriteString("\n")
			case "smart":
				if smartDev != "" {
					buf.WriteString(line)
					buf.WriteString("\n")
				}
			}
		}
	}
	flushPool()
	flushSmart()
	return vdevs, statusTexts, smart
}

// ParseJobs parses jobsProbe output. Malformed entries are skipped — job
// dirs may be mid-write or owned by another user.
func ParseJobs(host, out string) []JobInfo {
	var jobs []JobInfo
	var cur *JobInfo
	mode := ""
	var metaBuf strings.Builder
	flush := func() {
		if cur == nil {
			return
		}
		cur.Meta = map[string]string{}
		_ = json.Unmarshal([]byte(metaBuf.String()), &cur.Meta)
		if k := cur.Meta["kind"]; k != "" {
			cur.Kind = k
		}
		if cur.State == "" {
			cur.State = "unknown"
		}
		jobs = append(jobs, *cur)
		cur = nil
		metaBuf.Reset()
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "==J== "):
			flush()
			dir := strings.TrimSpace(strings.TrimPrefix(line, "==J== "))
			cur = &JobInfo{Host: host, Dir: dir, ID: strings.TrimPrefix(dir, jobDirPrefix)}
			mode = "status"
		case line == "==M==":
			mode = "meta"
		case line == "==P==":
			mode = "progress"
		case line == "==E==":
			mode = "expected"
		case cur == nil:
			// ignore noise outside entries
		default:
			switch mode {
			case "status":
				if cur.Status == "" && strings.TrimSpace(line) != "" {
					cur.Status = strings.TrimSpace(line)
					cur.State, _, _ = strings.Cut(cur.Status, " ")
				}
			case "meta":
				metaBuf.WriteString(line)
				metaBuf.WriteString("\n")
			case "progress":
				if v, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64); err == nil {
					cur.Progress = v
				}
			case "expected":
				if v, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64); err == nil {
					cur.Expected = v
				}
			}
		}
	}
	flush()
	return jobs
}

// poolFromProps mirrors ParsePools' field mapping (shared with the store).
func poolFromProps(host, name string, vals, srcs map[string]string) Pool {
	p := props(vals)
	return Pool{
		Host: host, Name: name,
		GUID: p.str("guid"),
		Size: p.num("size"), Alloc: p.num("allocated"), Free: p.num("free"),
		Cap: p.num("capacity"), Frag: p.num("fragmentation"),
		Dedup: p.str("dedupratio"), Health: p.str("health"),
		Props: vals, PropSrc: srcs,
	}
}

// The toProps inverses synthesize a property map from typed fields when an
// object was built in code (tests) rather than parsed. Keep in lockstep with
// the fromProps mappings above.

func (d Dataset) toProps() map[string]string {
	if d.Props != nil {
		return d.Props
	}
	m := map[string]string{
		"type": d.Type, "guid": d.GUID,
		"used": fmt.Sprint(d.Used), "available": fmt.Sprint(d.Avail),
		"referenced": fmt.Sprint(d.Refer), "creation": fmt.Sprint(d.Creation),
		"createtxg": fmt.Sprint(d.CreateTXG), "written": fmt.Sprint(d.Written),
		"origin": d.Origin, "encryption": d.Encryption,
		"keystatus": d.KeyStatus, "keylocation": d.KeyLocation,
		"canmount": d.CanMount, "mountpoint": d.Mountpoint,
		"receive_resume_token": d.ResumeToken, "compression": d.Compression,
		"recordsize": fmt.Sprint(d.RecordSize),
	}
	if d.Mounted {
		m["mounted"] = "yes"
	}
	if d.ReadOnly {
		m["readonly"] = "on"
	}
	return m
}

func (s Snapshot) toProps() map[string]string {
	if s.Props != nil {
		return s.Props
	}
	return map[string]string{
		"guid": s.GUID, "used": fmt.Sprint(s.Used), "referenced": fmt.Sprint(s.Refer),
		"creation": fmt.Sprint(s.Creation), "createtxg": fmt.Sprint(s.CreateTXG),
	}
}

func (p Pool) toProps() map[string]string {
	if p.Props != nil {
		return p.Props
	}
	return map[string]string{
		"guid": p.GUID, "size": fmt.Sprint(p.Size), "allocated": fmt.Sprint(p.Alloc),
		"free": fmt.Sprint(p.Free), "capacity": fmt.Sprint(p.Cap),
		"fragmentation": fmt.Sprint(p.Frag), "dedupratio": p.Dedup, "health": p.Health,
	}
}
