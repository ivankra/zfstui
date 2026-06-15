// Parses the machine-readable output of zpool/zfs commands. Hand-rolled on
// purpose. Discovery uses `zpool get -Hp all` / `zfs get -Hp all`: one line
// per (object, property, value, source), so EVERY property is available for
// detail views and adding a displayed field never changes the protocol.
// Typed struct fields are derived from the property maps in one place here.
package main

import (
	"encoding/base64"
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
	MarkHost      = "===ZFSTUI:host===" // host-level scalars (boot time)
	MarkRC        = "===ZFSTUI:rc="     // followed by "<n>==="
)

// jobsProbe emits ONE line per /tmp/zfstui-job-* dir: the dir path followed by
// space-separated key=value tokens whose values are base64 (newlines
// stripped):
//
//	<dir> status=<b64> meta=<b64> progress=<b64> expected=<b64> log=<b64>
//
// base64 contains no spaces/newlines, so arbitrary log or meta content can
// never split a token or alias the framing; the key is always present (an
// empty value is just "log=", unambiguous). The dir is a known-safe path
// (jobDirPrefix + base62 id) and anchors the line. Also used standalone for
// live progress polling.
const jobsProbe = `for d in /tmp/zfstui-job-*; do [ -d "$d" ] || continue; ` +
	`b() { base64 2>/dev/null | tr -d '\n'; }; ` +
	`printf '%s status=%s meta=%s progress=%s expected=%s log=%s\n' "$d" ` +
	`"$(head -n1 "$d/status" 2>/dev/null | b)" ` +
	`"$(cat "$d/meta.json" 2>/dev/null | b)" ` +
	`"$(tail -n1 "$d/progress" 2>/dev/null | b)" ` +
	`"$(head -n1 "$d/expected" 2>/dev/null | b)" ` +
	`"$(tail -n 200 "$d/log" 2>/dev/null | b)"; done; true`

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
	// + verbatim `zpool status`; then lsblk physical-disk inventory and SMART
	// for every real disk (smartctl handles SATA and NVMe; skipped if absent).
	disksProbe := `for p in $(` + pfx + `zpool list -H -o name); do ` +
		`echo "==P== $p"; ` + pfx + `zpool get -Hp all "$p" all-vdevs 2>/dev/null; ` +
		`echo "==S=="; ` + pfx + `zpool status "$p" 2>/dev/null; done; ` +
		`echo "==LSBLK=="; lsblk -b -P -o NAME,KNAME,TYPE,PKNAME,PATH,SIZE,MODEL,SERIAL,WWN,TRAN,ROTA,PARTLABEL,PARTUUID 2>/dev/null; ` +
		`echo "==SMART=="; if command -v smartctl >/dev/null 2>&1; then ` +
		`for d in $(lsblk -dn -o NAME,TYPE 2>/dev/null | awk '$2=="disk" && $1 !~ /^zd[0-9]+$/ {print $1}'); do ` +
		`echo "==D== /dev/$d"; ` + pfx + `smartctl -a "/dev/$d" 2>&1 | head -c 20000; echo; done; ` +
		`else echo "==NOSMART== smartctl not installed"; fi; true`
	return strings.Join([]string{
		section(MarkPools, "zpool get -Hp all"),
		fmt.Sprintf("echo '%s'; %s; echo '%s'$?'==='", MarkDisks, disksProbe, MarkRC),
		section(MarkDatasets, "zfs get -Hp -t filesystem,volume all"),
		section(MarkSnapshots, "zfs get -Hp -t snapshot all"),
		// Jobs need no sudo: dirs are owned by the ssh user that made them.
		fmt.Sprintf("echo '%s'; %s; echo '%s'$?'==='", MarkJobs, jobsProbe, MarkRC),
		// Host scalars (boot time from /proc/stat btime); no sudo, never fatal.
		fmt.Sprintf("echo '%s'; %s; echo '%s'$?'==='", MarkHost,
			`awk '/^btime/{print $2}' /proc/stat 2>/dev/null; true`, MarkRC),
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
		case trimmed == MarkPools || trimmed == MarkDisks || trimmed == MarkDatasets || trimmed == MarkSnapshots || trimmed == MarkJobs || trimmed == MarkHost:
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
	if sec := sections[MarkHost]; sec != nil { // non-required: tolerate absence
		if n, e := strconv.ParseInt(strings.TrimSpace(sec.String()), 10, 64); e == nil {
			st.BootTime = n
		}
	}
	var statusTexts map[string]string
	st.VDevs, statusTexts, st.Disks = ParseDisks(host, sections[MarkDisks].String())
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
	size := p.num("size")
	if size == 0 {
		size = p.num("psize")
	}
	if size == 0 {
		size = p.num("asize")
	}
	return VDev{
		Host: host, Pool: pool, Name: name,
		Parent: p.str("parent"), DiskID: p.str("_disk_id"), State: state, Path: p.str("path"),
		Size: size, Alloc: p.num("allocated"), Free: p.num("free"),
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
		"parent": v.Parent, "_disk_id": v.DiskID, "state": v.State, "path": v.Path,
		"size": fmt.Sprint(v.Size), "allocated": fmt.Sprint(v.Alloc),
		"free": fmt.Sprint(v.Free), "fragmentation": fmt.Sprint(v.Frag),
		"read_errors": fmt.Sprint(v.RErr), "write_errors": fmt.Sprint(v.WErr),
		"checksum_errors": fmt.Sprint(v.CErr),
	}
}

// ParseDisks parses the disks discovery section: per-pool vdev property
// tables and verbatim zpool status, then lsblk/smartctl physical disks.
func ParseDisks(host, out string) (vdevs []VDev, statusTexts map[string]string, disks []Disk) {
	statusTexts = map[string]string{}
	pool := ""
	mode := "" // vdevs | status | lsblk | smart
	var buf strings.Builder
	var propBuf strings.Builder
	var smartDev string
	var lsblkRows []map[string]string
	smartTexts := map[string]string{}
	flushPool := func() {
		if pool == "" {
			return
		}
		if t, err := parsePropTable(propBuf.String()); err == nil {
			for _, name := range t.order {
				vdevs = append(vdevs, vdevFromProps(host, pool, name, t.vals[name], t.srcs[name]))
			}
		}
		if s := strings.TrimRight(buf.String(), "\n\r "); s != "" {
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
		if s := strings.TrimRight(buf.String(), "\n\r "); s != "" {
			smartTexts[smartDev] = s
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
		case line == "==LSBLK==":
			flushPool()
			flushSmart()
			mode = "lsblk"
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
			case "lsblk":
				if kv := parseKeyValueLine(line); len(kv) > 0 {
					lsblkRows = append(lsblkRows, kv)
				}
			}
		}
	}
	flushPool()
	flushSmart()
	disks, aliases := disksFromLSBLK(host, lsblkRows, smartTexts)
	attachDiskIDsToVDevs(vdevs, aliases)
	return vdevs, statusTexts, disks
}

func parseKeyValueLine(line string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(line); {
		for i < len(line) && line[i] == ' ' {
			i++
		}
		start := i
		for i < len(line) && line[i] != '=' {
			i++
		}
		if i >= len(line) || start == i {
			break
		}
		key := line[start:i]
		i++
		if i >= len(line) || line[i] != '"' {
			break
		}
		i++
		var b strings.Builder
		for i < len(line) {
			if line[i] == '"' {
				i++
				break
			}
			if line[i] == '\\' && i+1 < len(line) {
				i++
			}
			b.WriteByte(line[i])
			i++
		}
		out[key] = b.String()
	}
	return out
}

func diskFromProps(host string, vals map[string]string) Disk {
	p := props(vals)
	d := Disk{
		Host: host,
		Dev:  p.str("PATH"), KName: p.str("KNAME"),
		Model: p.str("MODEL"), Serial: p.str("SERIAL"), WWN: p.str("WWN"),
		Size: p.num("SIZE"), Transport: p.str("TRAN"),
		Rotational: vals["ROTA"] == "1",
		Smart:      p.str("_smart"),
		Props:      vals,
	}
	if d.Dev == "" && d.KName != "" {
		d.Dev = "/dev/" + d.KName
	}
	d.ID = p.str("_id")
	if d.ID == "" {
		d.ID = stableDiskID(d)
	}
	return d
}

func (d Disk) toProps() map[string]string {
	if d.Props != nil {
		return d.Props
	}
	rota := "0"
	if d.Rotational {
		rota = "1"
	}
	return map[string]string{
		"_id": d.ID, "_smart": d.Smart,
		"PATH": d.Dev, "KNAME": d.KName, "TYPE": "disk",
		"MODEL": d.Model, "SERIAL": d.Serial, "WWN": d.WWN,
		"SIZE": fmt.Sprint(d.Size), "TRAN": d.Transport, "ROTA": rota,
	}
}

func disksFromLSBLK(host string, rows []map[string]string, smartTexts map[string]string) ([]Disk, map[string]string) {
	var disks []Disk
	diskByKName := map[string]string{}
	indexByID := map[string]int{}
	aliases := map[string]string{}
	addAlias := func(alias, id string) {
		if alias != "" && alias != "-" && id != "" {
			aliases[alias] = id
		}
	}
	for _, row := range rows {
		if row["TYPE"] != "disk" {
			continue
		}
		if isPseudoDiskName(firstNonEmpty(row["KNAME"], row["NAME"])) {
			continue
		}
		d := diskFromProps(host, row)
		if d.ID == "" {
			continue
		}
		indexByID[d.ID] = len(disks)
		disks = append(disks, d)
		if d.KName != "" {
			diskByKName[d.KName] = d.ID
		}
		if name := row["NAME"]; name != "" {
			diskByKName[name] = d.ID
		}
	}
	for _, row := range rows {
		id := ""
		if row["TYPE"] == "disk" {
			id = diskByKName[firstNonEmpty(row["KNAME"], row["NAME"])]
		} else if pk := row["PKNAME"]; pk != "" && pk != "-" {
			id = diskByKName[pk]
		}
		if id == "" {
			continue
		}
		addAlias(row["NAME"], id)
		addAlias(row["KNAME"], id)
		addAlias(row["PATH"], id)
		addAlias("/dev/"+row["NAME"], id)
		addAlias("/dev/"+row["KNAME"], id)
		if pl := row["PARTLABEL"]; pl != "" && pl != "-" {
			addAlias(pl, id)
			addAlias("/dev/disk/by-partlabel/"+pl, id)
		}
	}
	for dev, text := range smartTexts {
		if id := aliases[dev]; id != "" {
			if idx, ok := indexByID[id]; ok {
				disks[idx].Smart = text
				if disks[idx].Props != nil {
					disks[idx].Props["_smart"] = text
					disks[idx].Props["_id"] = disks[idx].ID
				}
			}
			continue
		}
	}
	return disks, aliases
}

func isPseudoDiskName(name string) bool {
	if !strings.HasPrefix(name, "zd") || len(name) == 2 {
		return false
	}
	for _, r := range name[2:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func diskAliasesFromDisks(disks []Disk) map[string]string {
	aliases := map[string]string{}
	add := func(alias, id string) {
		if alias != "" && alias != "-" && id != "" {
			aliases[alias] = id
		}
	}
	for _, d := range disks {
		add(d.ID, d.ID)
		add(d.Dev, d.ID)
		add(d.KName, d.ID)
		add(baseName(d.Dev), d.ID)
		if d.KName != "" {
			add("/dev/"+d.KName, d.ID)
		}
	}
	return aliases
}

func attachDiskIDsToVDevs(vdevs []VDev, diskAliases map[string]string) {
	for i := range vdevs {
		v := &vdevs[i]
		for _, a := range append(vdevAliases(v.Path), vdevAliases(v.Name)...) {
			if id := diskAliases[a]; id != "" {
				v.DiskID = id
				break
			}
		}
		if v.Props != nil {
			if v.DiskID != "" {
				v.Props["_disk_id"] = v.DiskID
			}
		}
	}
}

func vdevAliases(s string) []string {
	if s == "" || s == "-" {
		return nil
	}
	out := []string{s}
	if b := baseName(s); b != "" && b != s {
		out = append(out, b)
	}
	const byPartLabel = "/dev/disk/by-partlabel/"
	if strings.HasPrefix(s, byPartLabel) {
		out = append(out, strings.TrimPrefix(s, byPartLabel))
	}
	return out
}

func stableDiskID(d Disk) string {
	if d.WWN != "" && d.WWN != "-" {
		return "wwn:" + compactID(d.WWN)
	}
	if d.Serial != "" && d.Serial != "-" {
		return "serial:" + compactID(d.Serial)
	}
	if d.Dev != "" {
		return "dev:" + compactID(baseName(d.Dev))
	}
	return ""
}

func compactID(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), "_")
}

func baseName(s string) string {
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" && v != "-" {
			return v
		}
	}
	return ""
}

// ParseJobs parses jobsProbe output: one line per job dir, anchored by the
// dir path and followed by base64-valued key=value tokens (see jobsProbe).
// Lines without the dir prefix (shell noise, blanks) are skipped.
func ParseJobs(host, out string) []JobInfo {
	b64 := func(s string) string {
		d, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return ""
		}
		return string(d)
	}
	var jobs []JobInfo
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, jobDirPrefix) {
			continue
		}
		fields := strings.Fields(line)
		dir := fields[0]
		j := JobInfo{Host: host, Dir: dir, ID: strings.TrimPrefix(dir, jobDirPrefix), Meta: map[string]string{}}
		for _, tok := range fields[1:] {
			key, val, ok := strings.Cut(tok, "=")
			if !ok {
				continue
			}
			v := b64(val)
			switch key {
			case "status":
				j.Status = strings.TrimSpace(v)
				j.State, _, _ = strings.Cut(j.Status, " ")
			case "meta":
				_ = json.Unmarshal([]byte(v), &j.Meta)
				if k := j.Meta["kind"]; k != "" {
					j.Kind = k
				}
			case "progress":
				if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
					j.Progress = n
				}
			case "expected":
				if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
					j.Expected = n
				}
			case "log":
				j.Log = strings.TrimRight(v, "\n")
			}
		}
		if j.State == "" {
			j.State = "unknown"
		}
		jobs = append(jobs, j)
	}
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
