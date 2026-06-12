// The Details pane: structured, interactive content. Each selection (host /
// pool / group / dataset cell) renders title segments plus a list of lines;
// some lines are selectable rows (snapshots, jobs, pools, datasets,
// replicas) — when the pane is focused, ↑↓ move a cursor between them and
// Enter opens a modal with everything known about the object.
package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// detLine is one Details pane line; open != nil makes it selectable.
type detLine struct {
	text  string
	style lipgloss.Style
	open  func(m *Model) *textModal
}

func plainLine(text string) detLine                { return detLine{text: text, style: styleSection} }
func sectionLine(text string) detLine              { return detLine{text: text, style: styleSection} }
func styled(text string, s lipgloss.Style) detLine { return detLine{text: text, style: s} }

func colWidths(headers []string, rows [][]string) []int {
	ws := make([]int, len(headers))
	for i, h := range headers {
		ws[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(ws) && len(cell) > ws[i] {
				ws[i] = len(cell)
			}
		}
	}
	return ws
}

func tableLine(prefix string, ws []int, cells []string, right map[int]bool) string {
	var b strings.Builder
	b.WriteString(prefix)
	for i, cell := range cells {
		if i > 0 {
			b.WriteString("  ")
		}
		if right[i] {
			fmt.Fprintf(&b, "%*s", ws[i], cell)
		} else {
			fmt.Fprintf(&b, "%-*s", ws[i], cell)
		}
	}
	return b.String()
}

// detailsContent builds the pane for the current selection.
func (m *Model) detailsContent() (segs []string, lines []detLine) {
	g, c := m.selected()
	switch {
	case m.curRow == -2:
		if m.curCol >= 1 && m.curCol <= len(m.cols) {
			return m.hostDetails(m.hostOfCol(m.curCol - 1))
		}
	case m.curRow == -1:
		if m.curCol >= 1 && m.curCol <= len(m.cols) {
			parts := strings.SplitN(m.cols[m.curCol-1], ":", 2)
			return m.poolDetails(parts[0], parts[1])
		}
	case g != nil && m.curCol == 0:
		return m.groupDetails(g)
	case g != nil && c != nil && len(c.replicas) == 1:
		return m.replicaDetails(c.replicas[0])
	case g != nil && c != nil && len(c.replicas) > 1:
		segs = []string{c.replicas[0].Dataset.Host + ":…", fmt.Sprintf("%d replicas in this cell", len(c.replicas))}
		lines = append(lines, sectionLine("* REPLICAS"))
		lines = append(lines, m.replicaRows(c.replicas)...)
		return segs, lines
	}
	return []string{"Details"}, nil
}

// --- per-view builders ---

func (m *Model) replicaDetails(r *Replica) ([]string, []detLine) {
	d := r.Dataset
	id := d.Host + ":" + d.Name
	if d.Mounted && d.Mountpoint != "" {
		id += " (" + d.Mountpoint + ")"
	}
	segs := []string{id}
	info := human(d.Used) + " used"
	if r.Latest != nil {
		info += ", @" + r.Latest.Name
		if r.Latest.Refer > 0 {
			info += " (" + human(r.Latest.Refer) + ")"
		}
	} else {
		info += ", no snapshots"
	}
	if r.Written > 0 {
		info += " +" + human(r.Written)
	}
	segs = append(segs, info)
	if d.Encrypted() {
		segs = append(segs, d.Encryption)
	}

	var lines []detLine
	if d.Origin != "" {
		lines = append(lines, plainLine("clone of "+d.Origin))
	}
	if d.ResumeToken != "" {
		lines = append(lines, styled("interrupted recv (resume token present)", styleResume))
	}
	if jobs := m.data.JobsByReplica[replicaKeyOf(r)]; len(jobs) > 0 {
		lines = append(lines, sectionLine("* JOBS"))
		lines = append(lines, m.jobRows(jobs, 5)...)
	}
	snaps := m.data.SnapsByReplica[d.Host+":"+d.Name]
	if len(snaps) > 0 {
		nameW := 8
		for _, s := range snaps {
			if len(s.Name) > nameW {
				nameW = len(s.Name)
			}
		}
		lines = append(lines, sectionLine(fmt.Sprintf("* %-*s  %7s  %7s", nameW, "SNAPSHOT", "USED", "REFER")))
		for _, s := range snaps {
			s := s
			lines = append(lines, detLine{
				text:  fmt.Sprintf("  %-*s  %7s  %7s", nameW, s.Name, human(s.Used), human(s.Refer)),
				style: styleDetRow,
				open:  func(*Model) *textModal { return snapshotModal(s) },
			})
		}
	} else {
		lines = append(lines, plainLine("no snapshots"))
	}
	return segs, lines
}

func (m *Model) groupDetails(g *Group) ([]string, []detLine) {
	segs := []string{"group " + g.DisplayName}
	latestSeg := ""
	var writtenSum int64
	for _, r := range g.Members {
		writtenSum += r.Written
		if r.Latest != nil && r.Latest.Name == g.LatestName && latestSeg == "" {
			latestSeg = "@" + g.LatestName
			if r.Latest.Refer > 0 {
				latestSeg += " (" + human(r.Latest.Refer) + ")"
			}
		}
	}
	if latestSeg != "" {
		if writtenSum > 0 {
			latestSeg += " +" + human(writtenSum)
		}
		segs = append(segs, latestSeg)
	}
	lines := []detLine{sectionLine("* REPLICAS")}
	lines = append(lines, m.replicaRows(g.Members)...)
	return segs, lines
}

// replicaRows renders selectable member rows (group view, multi-replica cells).
func (m *Model) replicaRows(members []*Replica) []detLine {
	nameW := 0
	for _, r := range members {
		if l := len(r.Dataset.Host + ":" + r.Dataset.Name); l > nameW {
			nameW = l
		}
	}
	var lines []detLine
	for _, r := range members {
		r := r
		live := ""
		if j := m.runningJobFor(r); j != nil {
			live = "  " + spinnerFrames[m.spin%len(spinnerFrames)] + " " + jobProgressLine(j)
		}
		lines = append(lines, detLine{
			text: fmt.Sprintf("  %-*s  %-9s  %s%s",
				nameW, r.Dataset.Host+":"+r.Dataset.Name, r.State, replicaBrief(r), live),
			style: styleDetRow,
			open:  func(*Model) *textModal { return datasetModal(r.Dataset) },
		})
	}
	return lines
}

// jobRows renders selectable job rows, newest first.
func (m *Model) jobRows(jobs []JobInfo, limit int) []detLine {
	var lines []detLine
	for _, j := range jobs {
		if len(lines) >= limit {
			break
		}
		j := j
		desc := j.Kind
		switch j.Kind {
		case "sync":
			desc = j.Meta["src"] + " → " + j.Meta["dest"]
			if j.Meta["full"] == "true" {
				desc += " (full)"
			}
		case "snapshot":
			desc = "snapshot " + j.Meta["dataset"] + "@" + j.Meta["snapshot"]
		}
		status := ""
		st := styleDetRow
		switch j.State {
		case "running":
			status = spinnerFrames[m.spin%len(spinnerFrames)] + " "
			if j.Expected > 0 && j.Progress > 0 {
				status += fmt.Sprintf("%d%%", min(j.Progress*100/j.Expected, 100))
			} else {
				status += "run"
			}
		case "done":
			status = "OK"
		case "failed":
			status, st = "FAIL", styleErr
		default:
			status = j.State
		}
		lines = append(lines, detLine{
			text:  fmt.Sprintf("  %-7s %-6s %s", j.ID, status, desc),
			style: st,
			open:  func(*Model) *textModal { return jobModal(j) },
		})
	}
	return lines
}

func (m *Model) hostDetails(host string) ([]string, []detLine) {
	segs := []string{"host " + host}
	var size, alloc, free int64
	var disks []Disk
	for i := range m.data.States {
		hs := &m.data.States[i]
		if hs.Host != host {
			continue
		}
		disks = hs.Disks
		for _, p := range hs.Pools {
			size += p.Size
			alloc += p.Alloc
			free += p.Free
		}
	}
	if size > 0 {
		segs = append(segs, fmt.Sprintf("%s, %d%% used, %s free", human(size), alloc*100/size, human(free)))
	}

	var lines []detLine
	access := ""
	for _, h := range m.hosts {
		if h.Name == host {
			access = h.URL()
			if h.NeedSudo() {
				access += " (sudo)"
			}
		}
	}
	for _, hi := range m.data.Hosts {
		if hi.Name != host {
			continue
		}
		when := hi.LastRefresh.Format("2006-01-02 15:04:05")
		host := host
		if hi.Reachable {
			lines = append(lines, detLine{text: "* ssh: " + access + ", refreshed " + when,
				style: styleSection, open: func(m *Model) *textModal { return m.hostModal(host) }})
		} else {
			lines = append(lines, detLine{text: "* UNREACHABLE at last refresh (" + when + "): " + hi.LastError,
				style: styleErr, open: func(m *Model) *textModal { return m.hostModal(host) }})
		}
	}
	if len(disks) > 0 {
		lines = append(lines, diskTableLines(disks)...)
	}
	return segs, lines
}

func (m *Model) poolDetails(host, pool string) ([]string, []detLine) {
	segs := []string{"pool " + host + ":" + pool}
	var p *Pool
	var dss []Dataset
	for i := range m.data.States {
		hs := &m.data.States[i]
		if hs.Host != host {
			continue
		}
		for j := range hs.Pools {
			if hs.Pools[j].Name == pool {
				p = &hs.Pools[j]
			}
		}
		for _, d := range hs.Datasets {
			if d.Pool() == pool {
				dss = append(dss, d)
			}
		}
	}
	if p == nil {
		return segs, []detLine{plainLine("not in cache")}
	}
	if p.Size > 0 {
		segs = append(segs, fmt.Sprintf("%s, %d%% used, %s free", human(p.Size), p.Alloc*100/p.Size, human(p.Free)))
	}
	var lines []detLine
	// scrub/errors summary first (full text in the modal); the errors bullet
	// only appears when there ARE errors; the scrub bullet turns red with them.
	pp := *p
	for _, ln := range strings.Split(p.Props["_zpool-status"], "\n") {
		t := strings.TrimSpace(ln)
		open := func(*Model) *textModal { return poolModal(pp) }
		if s, ok := strings.CutPrefix(t, "scan:"); ok {
			st := styleSection
			scan := strings.TrimSpace(s)
			if strings.Contains(scan, "error") && !strings.Contains(scan, "with 0 errors") {
				st = styleErr
			}
			lines = append(lines, detLine{text: "* " + scan, style: st, open: open})
		}
		if s, ok := strings.CutPrefix(t, "errors:"); ok {
			if e := strings.TrimSpace(s); e != "No known data errors" {
				lines = append(lines, detLine{text: "* errors: " + e, style: styleErr, open: open})
			}
		}
	}
	// Leaf vdevs only. The full topology remains available via verbatim
	// `zpool status` in the pool modal.
	if vds := m.vdevsFor(host, pool); len(vds) > 0 {
		lines = append(lines, m.vdevTableLines(host, vds, false)...)
	}
	if len(dss) > 0 {
		nameW := 7
		for _, d := range dss {
			if l := len(displayDSName(d)); l > nameW {
				nameW = l
			}
		}
		lines = append(lines, sectionLine(fmt.Sprintf("* %-*s  %7s %7s  %s", nameW, "DATASET", "USED", "REFER", "MOUNTPOINT")))
		for _, d := range dss {
			d := d
			lines = append(lines, detLine{
				text: fmt.Sprintf("  %-*s  %7s %7s  %s",
					nameW, displayDSName(d), human(d.Used), human(d.Refer), orDash(d.Mountpoint)),
				style: styleDetRow,
				open:  func(*Model) *textModal { return datasetModal(&d) },
			})
		}
	}
	return segs, lines
}

// displayDSName shows the in-pool name; the pool's root dataset keeps its name.
func displayDSName(d Dataset) string {
	if n := d.NameInPool(); n != "" {
		return n
	}
	return d.Name
}

func diskTableLines(disks []Disk) []detLine {
	disks = append([]Disk(nil), disks...)
	sort.SliceStable(disks, func(i, j int) bool {
		if disks[i].Dev != disks[j].Dev {
			return disks[i].Dev < disks[j].Dev
		}
		return disks[i].ID < disks[j].ID
	})
	headers := []string{"DISK", "MODEL", "SERIAL", "SIZE", "TYPE", "SMART"}
	var rows [][]string
	for _, d := range disks {
		rows = append(rows, diskRow(d))
	}
	ws := colWidths(headers, rows)
	right := map[int]bool{3: true}
	lines := []detLine{sectionLine(tableLine("* ", ws, headers, right))}
	for i, d := range disks {
		d := d
		st := styleDetRow
		if h := diskHealth(d); h == "WARN" || h == "FAIL" {
			st = styleErr
		}
		lines = append(lines, detLine{text: tableLine("  ", ws, rows[i], right), style: st,
			open: func(*Model) *textModal { return diskModal(d) }})
	}
	return lines
}

func diskRow(d Disk) []string {
	return []string{d.Dev, orDash(d.Model), orDash(d.Serial), human(d.Size), diskType(d), diskHealth(d)}
}

func (m *Model) vdevTableLines(host string, vdevs []VDev, showPool bool) []detLine {
	headers := []string{"VDEV", "STATE", "READ", "WRITE", "CKSUM", "SIZE", "DISK"}
	if showPool {
		headers = []string{"VDEV", "POOL", "STATE", "READ", "WRITE", "CKSUM", "SIZE", "DISK"}
	}
	var rows [][]string
	for _, v := range vdevs {
		rows = append(rows, m.vdevRow(host, v, showPool))
	}
	ws := colWidths(headers, rows)
	right := map[int]bool{2: true, 3: true, 4: true, 5: true}
	if showPool {
		right = map[int]bool{3: true, 4: true, 5: true, 6: true}
	}
	lines := []detLine{sectionLine(tableLine("* ", ws, headers, right))}
	for i, v := range vdevs {
		v := v
		st := styleDetRow
		if v.State != "ONLINE" || v.RErr > 0 || v.WErr > 0 || v.CErr > 0 {
			st = styleErr
		}
		lines = append(lines, detLine{text: tableLine("  ", ws, rows[i], right), style: st,
			open: func(m *Model) *textModal { return m.vdevModal(host, v) }})
	}
	return lines
}

func (m *Model) vdevRow(host string, v VDev, showPool bool) []string {
	if showPool {
		return []string{
			v.Name, v.Pool, v.State,
			fmt.Sprint(v.RErr), fmt.Sprint(v.WErr), fmt.Sprint(v.CErr),
			vdevSize(v), m.vdevDiskLabel(host, v),
		}
	}
	return []string{
		v.Name, v.State,
		fmt.Sprint(v.RErr), fmt.Sprint(v.WErr), fmt.Sprint(v.CErr),
		vdevSize(v), m.vdevDiskLabel(host, v),
	}
}

// vdevsFor returns the cached leaf vdevs of host:pool (discovery order).
func (m *Model) vdevsFor(host, pool string) []VDev {
	for i := range m.data.States {
		if m.data.States[i].Host != host {
			continue
		}
		var out []VDev
		for _, v := range m.data.States[i].VDevs {
			if v.Pool == pool && isLeafVDev(v) {
				out = append(out, v)
			}
		}
		return out
	}
	return nil
}

func isLeafVDev(v VDev) bool {
	if v.Props != nil {
		p := props(v.Props)
		if children := p.str("children"); children != "" {
			return children == "-"
		}
		if p.num("numchildren") > 0 {
			return false
		}
	}
	return v.Path != "" || v.Size > 0
}

func vdevSize(v VDev) string {
	if v.Size > 0 {
		return human(v.Size)
	}
	return "-"
}

func (m *Model) vdevDiskLabel(host string, v VDev) string {
	if v.DiskID != "" {
		for i := range m.data.States {
			hs := &m.data.States[i]
			if hs.Host != host {
				continue
			}
			for _, d := range hs.Disks {
				if d.ID == v.DiskID && d.Dev != "" {
					return d.Dev
				}
			}
		}
	}
	if v.Path != "" {
		return v.Path
	}
	return "-"
}

func (m *Model) vdevModal(host string, v VDev) *textModal {
	lines := []string{
		"host      " + v.Host,
		"pool      " + v.Pool,
		"state     " + v.State,
		fmt.Sprintf("errors    read %d  write %d  cksum %d", v.RErr, v.WErr, v.CErr),
	}
	if disk := m.vdevDiskLabel(host, v); disk != "-" {
		lines = append(lines, "disk      "+disk)
	}
	if v.DiskID != "" {
		lines = append(lines, "disk id   "+v.DiskID)
	}
	if v.Path != "" {
		lines = append(lines, "path      "+v.Path)
	}
	if v.Size > 0 {
		lines = append(lines, fmt.Sprintf("space     size %s  alloc %s  free %s  frag %d%%",
			human(v.Size), human(v.Alloc), human(v.Free), v.Frag))
	}
	return &textModal{title: "vdev " + v.Pool + "/" + v.Name, lines: propsDump(lines, v.Props, v.PropSrc)}
}

// appendVerbatim adds a titled verbatim text block to modal lines.
func appendVerbatim(lines []string, title, text string) []string {
	if strings.TrimSpace(text) == "" {
		return lines
	}
	lines = append(lines, "", "── "+title+" "+strings.Repeat("─", 30))
	for _, ln := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		lines = append(lines, ln)
	}
	return lines
}

// hostModal: everything known about a host (access spec, refresh state,
// pools, cache counts).
func (m *Model) hostModal(host string) *textModal {
	var lines []string
	for _, h := range m.hosts {
		if h.Name != host {
			continue
		}
		ssh := "ssh: " + h.URL()
		if h.NeedSudo() {
			ssh += " (sudo)"
		}
		if h.SendUser != "" {
			ssh += "  send_user=" + h.SendUser
		}
		if h.RecvUser != "" {
			ssh += "  recv_user=" + h.RecvUser
		}
		if h.Pool != "" {
			ssh += "  pool=" + h.Pool + " (not enforced yet)"
		}
		lines = append(lines, ssh)
	}
	var hs *HostState
	for i := range m.data.States {
		if m.data.States[i].Host == host {
			hs = &m.data.States[i]
		}
	}
	for _, hi := range m.data.Hosts {
		if hi.Name != host {
			continue
		}
		when := hi.LastRefresh.Format("2006-01-02 15:04:05")
		info := "refreshed: " + when
		if !hi.Reachable {
			info = "UNREACHABLE at last refresh (" + when + "): " + hi.LastError
		}
		if hs != nil {
			running := 0
			for _, j := range hs.Jobs {
				if j.State == "running" {
					running++
				}
			}
			info += fmt.Sprintf(", %d dataset(s), %d snapshot(s), %d/%d job(s) running",
				len(hs.Datasets), len(hs.Snapshots), running, len(hs.Jobs))
		}
		lines = append(lines, info)
	}
	if hs != nil {
		lines = append(lines, "")
		for _, p := range hs.Pools {
			pct := int64(0)
			if p.Size > 0 {
				pct = p.Alloc * 100 / p.Size
			}
			lines = append(lines, fmt.Sprintf("pool %-12s %-9s size %-8s alloc %s (%d%%) free %s",
				p.Name, p.Health, human(p.Size), human(p.Alloc), pct, human(p.Free)))
		}
		if len(hs.Disks) > 0 {
			lines = append(lines, "")
			for _, d := range hs.Disks {
				lines = append(lines, fmt.Sprintf("disk %-9s %-6s %-7s %-12s %s",
					d.Dev, diskHealth(d), human(d.Size), orDash(d.Serial), d.Model))
				if d.Serial != "" {
					lines = append(lines, "  serial "+d.Serial)
				}
				if d.WWN != "" {
					lines = append(lines, "  wwn    "+d.WWN)
				}
				lines = append(lines, "  type   "+diskType(d))
			}
		}
	}
	return &textModal{title: "host " + host, lines: lines}
}

// poolModalFor looks up a pool by host:name (nil if not cached).
func (m *Model) poolModalFor(host, pool string) *textModal {
	for i := range m.data.States {
		if m.data.States[i].Host != host {
			continue
		}
		for _, p := range m.data.States[i].Pools {
			if p.Name == pool {
				return poolModal(p)
			}
		}
	}
	return nil
}

// --- object detail modals (everything we know) ---

// propsDump appends the full sorted property map with sources.
func propsDump(lines []string, props, srcs map[string]string) []string {
	if len(props) == 0 {
		return append(lines, "", "(no full property map cached — refresh to populate)")
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		if !strings.HasPrefix(k, "_") { // synthetic verbatims rendered separately
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	lines = append(lines, "", "── properties "+strings.Repeat("─", 30))
	keyW := 0
	for _, k := range keys {
		if len(k) > keyW {
			keyW = len(k)
		}
	}
	for _, k := range keys {
		l := fmt.Sprintf("%-*s  %s", keyW, k, props[k])
		if s := srcs[k]; s != "" && s != "local" {
			l += "  (" + s + ")"
		}
		lines = append(lines, l)
	}
	return lines
}

func datasetModal(d *Dataset) *textModal {
	lines := []string{
		"host      " + d.Host,
		"type      " + d.Type,
		"guid      " + orDash(d.GUID) + "   (dataset guids differ per replica)",
		fmt.Sprintf("space     used %s  avail %s  refer %s  written %s",
			human(d.Used), human(d.Avail), human(d.Refer), human(d.Written)),
		"created   " + time.Unix(d.Creation, 0).Format("2006-01-02 15:04:05"),
	}
	if d.Origin != "" {
		lines = append(lines, "origin    "+d.Origin+"  (clone)")
	}
	if d.ResumeToken != "" {
		lines = append(lines, "resume    interrupted recv, token present")
	}
	return &textModal{title: d.Host + ":" + d.Name, lines: propsDump(lines, d.Props, d.PropSrc)}
}

func snapshotModal(s Snapshot) *textModal {
	lines := []string{
		"host      " + s.Host,
		"dataset   " + s.Dataset,
		"guid      " + orDash(s.GUID) + "   (THE cross-replica identity)",
		"created   " + time.Unix(s.Creation, 0).Format("2006-01-02 15:04:05"),
		fmt.Sprintf("space     used %s  refer %s", human(s.Used), human(s.Refer)),
		fmt.Sprintf("createtxg %d   (pool-local)", s.CreateTXG),
	}
	return &textModal{title: s.Dataset + "@" + s.Name, lines: propsDump(lines, s.Props, s.PropSrc)}
}

func poolModal(p Pool) *textModal {
	lines := []string{
		"host      " + p.Host,
		"health    " + p.Health,
		fmt.Sprintf("space     size %s  alloc %s (%d%%)  free %s",
			human(p.Size), human(p.Alloc), p.Cap, human(p.Free)),
		fmt.Sprintf("frag      %d%%   dedup %s", p.Frag, p.Dedup),
		"guid      " + orDash(p.GUID),
	}
	out := appendVerbatim(lines, "zpool status "+p.Name, p.Props["_zpool-status"])
	out = propsDump(out, p.Props, p.PropSrc)
	return &textModal{title: "pool " + p.Host + ":" + p.Name, lines: out}
}

func diskModal(d Disk) *textModal {
	rota := "solid-state"
	if d.Rotational {
		rota = "rotational"
	}
	lines := []string{
		"host      " + d.Host,
		"id        " + d.ID,
		"dev       " + d.Dev,
		"model     " + orDash(d.Model),
		"serial    " + orDash(d.Serial),
		"wwn       " + orDash(d.WWN),
		fmt.Sprintf("space     size %s  media %s  transport %s", human(d.Size), rota, orDash(d.Transport)),
		"smart     " + diskHealth(d),
	}
	out := appendVerbatim(lines, "smartctl -a "+d.Dev, d.Smart)
	out = propsDump(out, d.Props, nil)
	return &textModal{title: "disk " + d.Host + ":" + baseName(d.Dev), lines: out}
}

func diskHealth(d Disk) string {
	t := d.Smart
	switch {
	case strings.TrimSpace(t) == "":
		return "-"
	case strings.Contains(t, "SMART overall-health self-assessment test result: PASSED"),
		strings.Contains(t, "SMART Health Status: OK"),
		strings.Contains(t, "SMART overall-health: PASSED"),
		strings.Contains(t, "Critical Warning:                   0x00"),
		strings.Contains(t, "Critical Warning: 0x00"):
		return "OK"
	case strings.Contains(t, "FAILED"),
		strings.Contains(t, "Critical Warning:") && !strings.Contains(t, "Critical Warning:                   0x00") && !strings.Contains(t, "Critical Warning: 0x00"):
		return "FAIL"
	case strings.Contains(t, "Unable to detect device type"):
		return "-"
	default:
		return "SMART"
	}
}

func diskType(d Disk) string {
	media := "ssd"
	if d.Rotational {
		media = "hdd"
	}
	if d.Transport == "" {
		return media
	}
	return d.Transport + " " + media
}

func jobModal(j JobInfo) *textModal {
	lines := []string{
		"host      " + j.Host,
		"kind      " + j.Kind,
		"state     " + j.State + "   (" + j.Status + ")",
		"dir       " + j.Dir,
	}
	if j.Progress > 0 || j.Expected > 0 {
		lines = append(lines, "progress  "+jobProgressLine(&j))
	}
	var keys []string
	for k := range j.Meta {
		if k != "command" && k != "id" && k != "kind" && k != "host" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("%-9s %s", k, j.Meta[k]))
	}
	if cmd := j.Meta["command"]; cmd != "" {
		lines = append(lines, "", "command:")
		for _, cl := range strings.Split(strings.TrimRight(cmd, "\n"), "\n") {
			lines = append(lines, "  "+cl)
		}
	}
	return &textModal{title: "job " + j.ID, lines: lines}
}

// --- details cursor / rendering ---

// detSelectable returns indices of selectable lines.
func detSelectable(lines []detLine) []int {
	var sel []int
	for i, l := range lines {
		if l.open != nil {
			sel = append(sel, i)
		}
	}
	return sel
}

// detMove moves the details cursor among selectable rows (or plain-scrolls
// when nothing is selectable), keeping it visible.
func (m *Model) detMove(dir int) {
	_, lines := m.detailsContent()
	sel := detSelectable(lines)
	if len(sel) == 0 {
		m.detOff += dir
		return
	}
	// Find current position in sel and step.
	pos := 0
	for i, li := range sel {
		if li >= m.detCur {
			pos = i
			break
		}
		pos = i + 1
	}
	if li := sel[min(max(pos, 0), len(sel)-1)]; li == m.detCur {
		pos += dir
	} else if dir < 0 {
		pos--
	}
	pos = min(max(pos, 0), len(sel)-1)
	m.detCur = sel[pos]
	// Scroll to keep the cursor visible — including up to 3 non-selectable
	// context lines directly above it (section headers like "* JOBS" must
	// scroll back into view when moving up; this also returns detOff to 0
	// at the first row).
	top := m.detCur
	for steps := 0; top > 0 && lines[top-1].open == nil && steps < 3; steps++ {
		top--
	}
	if top < m.detOff {
		m.detOff = top
	}
	if m.detCur >= m.detOff+detailsBodyH {
		m.detOff = m.detCur - detailsBodyH + 1
	}
}

// detOpen opens the modal for the selected row.
func (m *Model) detOpen() {
	_, lines := m.detailsContent()
	if m.detCur >= 0 && m.detCur < len(lines) && lines[m.detCur].open != nil {
		m.modal = lines[m.detCur].open(m)
	}
}

// detSnap clamps detCur onto a selectable row (after selection changes).
func (m *Model) detSnap() {
	_, lines := m.detailsContent()
	sel := detSelectable(lines)
	if len(sel) == 0 {
		m.detCur = 0
		return
	}
	for _, li := range sel {
		if li >= m.detCur {
			m.detCur = li
			return
		}
	}
	m.detCur = sel[len(sel)-1]
}

// detailsBody renders the pane body with scrolling and the row cursor.
func (m *Model) detailsBody() string {
	_, lines := m.detailsContent()
	maxOff := len(lines) - detailsBodyH
	if maxOff < 0 {
		maxOff = 0
	}
	if m.detOff > maxOff {
		m.detOff = maxOff
	}
	if m.detOff < 0 {
		m.detOff = 0
	}
	var b strings.Builder
	for i := 0; i < detailsBodyH; i++ {
		idx := m.detOff + i
		if idx < len(lines) {
			l := lines[idx]
			st := l.style
			if m.focus == 1 && idx == m.detCur && l.open != nil {
				// Focused details: a loud white-bg cursor (the subtle tint
				// is for the unfocused main-table row guide only).
				st = lipgloss.NewStyle().Background(lipgloss.Color("7")).Foreground(lipgloss.Color("0"))
			}
			txt := pad(l.text, m.width-2)
			rendered := st.Render(txt)
			switch {
			case i == 0 && m.detOff > 0:
				rendered += styleDim.Render(" ↑")
			case i == detailsBodyH-1 && idx < len(lines)-1:
				rendered += styleDim.Render(" ↓")
			}
			b.WriteString(rendered)
		}
		b.WriteString("\n")
	}
	return b.String()
}
