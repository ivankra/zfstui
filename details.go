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


// modalTable appends a section header, plain column header row, and data rows.
func modalTable(lines []string, title string, headers []string, rows [][]string) []string {
	if len(rows) == 0 {
		return lines
	}
	ws := colWidths(headers, rows)
	lines = append(lines, "", sectionHeader(title))
	lines = append(lines, styleCyan.Render(tableLine("", ws, headers, nil)))
	for _, row := range rows {
		lines = append(lines, tableLine("", ws, row, nil))
	}
	return lines
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
		lines = append(lines, m.replicaRows(c.replicas)...)
		return segs, lines
	}
	return nil, nil
}

// --- per-view builders ---

func (m *Model) replicaDetails(r *Replica) ([]string, []detLine) {
	d := r.Dataset
	segs := []string{"dataset " + d.Host + ":" + d.Name}
	info := ""
	if d.Mountpoint != "" && d.Mountpoint != "none" {
		mp := d.Mountpoint
		if !d.Mounted {
			mp += " (unmounted)"
		}
		info = mp + ", "
	}
	info += humanShort(d.Used) + " used"
	if r.Latest != nil {
		info += ", @" + r.Latest.Name
		if r.Latest.Refer > 0 {
			info += " (" + humanShort(r.Latest.Refer) + ")"
		}
	} else {
		info += ", no snapshots"
	}
	if r.Written > 0 {
		info += " +" + humanShort(r.Written)
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
		lines = append(lines, styled("* interrupted recv (resume token present)", styleCyan))
	}
	if jobs := m.data.JobsByReplica[replicaKeyOf(r)]; len(jobs) > 0 {
		lines = append(lines, sectionLine("* JOBS"))
		lines = append(lines, m.jobRows(jobs, 5)...)
	}
	snaps := m.data.SnapsByReplica[d.Host+":"+d.Name]
	if len(snaps) > 0 {
		headers := []string{"SNAPSHOT", "USED", "REFER", "GUID"}
		var snapRows [][]string
		for i := len(snaps) - 1; i >= 0; i-- {
			s := snaps[i]
			snapRows = append(snapRows, []string{s.Name, orDash(humanShort(s.Used)), humanShort(s.Refer), orDash(s.GUID)})
		}
		ws := colWidths(headers, snapRows)
		right := map[int]bool{1: true, 2: true}
		lines = append(lines, sectionLine(tableLine("* ", ws, headers, right)))
		for i := len(snaps) - 1; i >= 0; i-- {
			s := snaps[i]
			row := snapRows[len(snaps)-1-i]
			lines = append(lines, detLine{
				text:  tableLine("  ", ws, row, right),
				style: styleDetRow,
				open:  func(*Model) *textModal { return snapshotModal(s) },
			})
		}
	} else {
		lines = append(lines, plainLine("* no snapshots"))
	}
	return segs, lines
}

func (m *Model) groupDetails(g *Group) ([]string, []detLine) {
	return []string{"group " + g.DisplayName}, m.replicaRows(g.Members)
}

// replicaRows renders selectable member rows as a table:
// REPLICA  STATUS  USED  REFER  LATEST
func (m *Model) replicaRows(members []*Replica) []detLine {
	headers := []string{"REPLICA", "USED", "REFER", "STATUS", "LATEST"}
	var rows [][]string
	for _, r := range members {
		name := r.Dataset.Host + ":" + r.Dataset.Name
		used := orDash(humanShort(r.Dataset.Used))
		refer := "-"
		if r.Latest != nil {
			refer = humanShort(r.Latest.Refer)
		}
		status := string(r.State)
		latest := replicaBrief(r)
		if j := m.runningJobFor(r); j != nil {
			verb := "send"
			if replicaKeyOf(r) == j.Meta["dest"] {
				verb = "recv"
			}
			latest += "  " + spinnerFrames[m.spin%len(spinnerFrames)] + verb + " " + jobProgressLine(j)
		}
		rows = append(rows, []string{name, used, refer, status, latest})
	}
	ws := colWidths(headers, rows)
	right := map[int]bool{1: true, 2: true} // USED, REFER
	lines := []detLine{sectionLine(tableLine("* ", ws, headers, right))}
	for i, r := range members {
		r := r
		lines = append(lines, detLine{
			text:  tableLine("  ", ws, rows[i], right),
			style: styleDetRow,
			open:  func(m *Model) *textModal { return m.datasetModal(r.Dataset) },
		})
	}
	return lines
}

// jobRows renders selectable job rows, newest first.
func (m *Model) jobRows(jobs []JobInfo, limit int) []detLine {
	type entry struct {
		j      JobInfo
		status string
		desc   string
		st     lipgloss.Style
	}
	var entries []entry
	for _, j := range jobs {
		if len(entries) >= limit {
			break
		}
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
			status = spinnerFrames[m.spin%len(spinnerFrames)]
			desc += "  " + jobProgressLine(&j)
		case "done":
			status = "✓"
		case "failed":
			status, st = "✗", styleErr
		default:
			status = j.State
		}
		entries = append(entries, entry{j: j, status: status, desc: desc, st: st})
	}
	// Column widths from actual data (braille spinner: 1 display col, 3 bytes).
	stW, idW := 0, 0
	for _, e := range entries {
		if w := lipgloss.Width(e.status); w > stW {
			stW = w
		}
		if w := len(e.j.ID); w > idW {
			idW = w
		}
	}
	var lines []detLine
	for _, e := range entries {
		e := e
		stPad := strings.Repeat(" ", stW-lipgloss.Width(e.status))
		lines = append(lines, detLine{
			text:  fmt.Sprintf("  %s%s %-*s  %s", e.status, stPad, idW, e.j.ID, e.desc),
			style: e.st,
			open:  func(*Model) *textModal { return jobModal(e.j) },
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
		segs = append(segs, fmt.Sprintf("%s, %d%% used, %s free", humanShort(size), alloc*100/size, humanShort(free)))
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
		segs = append(segs, fmt.Sprintf("%s, %d%% used, %s free", humanShort(p.Size), p.Alloc*100/p.Size, humanShort(p.Free)))
	}
	var lines []detLine
	// scrub/errors summary first (full text in the modal); the errors bullet
	// only appears when there ARE errors; the scrub bullet turns red with them.
	pp := *p
	ppVdevs := m.vdevsFor(host, pool)
	ppDss := dss
	for _, ln := range strings.Split(p.Props["_zpool-status"], "\n") {
		t := strings.TrimSpace(ln)
		open := func(m *Model) *textModal { return m.poolModal(pp, ppVdevs, ppDss) }
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
		headers := []string{"DATASET", "USED", "REFER", "MOUNTPOINT", "SNAPSHOT"}
		var dsRows [][]string
		for _, d := range dss {
			dsRows = append(dsRows, []string{d.Name, humanShort(d.Used), humanShort(d.Refer), orDash(d.Mountpoint), m.snapCell(d)})
		}
		ws := colWidths(headers, dsRows)
		right := map[int]bool{1: true, 2: true}
		lines = append(lines, sectionLine(tableLine("* ", ws, headers, right)))
		for i, d := range dss {
			d := d
			lines = append(lines, detLine{
				text:  tableLine("  ", ws, dsRows[i], right),
				style: styleDetRow,
				open:  func(m *Model) *textModal { return m.datasetModal(&d) },
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
	return []string{d.Dev, orDash(d.Model), orDash(d.Serial), humanShort(d.Size), diskType(d), diskHealth(d)}
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
		return humanShort(v.Size)
	}
	return "-"
}

func (m *Model) snapCell(d Dataset) string {
	snaps := m.data.SnapsByReplica[d.Host+":"+d.Name]
	if len(snaps) == 0 {
		return "-"
	}
	s := "@" + snaps[len(snaps)-1].Name
	if d.Written > 0 {
		s += " +" + humanShort(d.Written)
	}
	return s
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
			humanShort(v.Size), humanShort(v.Alloc), humanShort(v.Free), v.Frag))
	}
	return makeModal("vdev "+v.Pool+"/"+v.Name, propsDump(lines, "zpool properties", v.Props, v.PropSrc))
}

// appendVerbatim adds a titled verbatim text block to modal lines.
func appendVerbatim(lines []string, title, text string) []string {
	if strings.TrimSpace(text) == "" {
		return lines
	}
	lines = append(lines, "", sectionHeader(title))
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
			info += fmt.Sprintf(", %d datasets, %d snapshots, %d/%d jobs running",
				len(hs.Datasets), len(hs.Snapshots), running, len(hs.Jobs))
		}
		lines = append(lines, info)
	}
	if hs != nil && len(hs.Disks) > 0 {
		var rows [][]string
		for _, d := range hs.Disks {
			rows = append(rows, []string{d.Dev, d.Model, orDash(d.Serial), humanShort(d.Size), diskType(d), diskHealth(d)})
		}
		lines = modalTable(lines, "disks", []string{"DISK", "MODEL", "SERIAL", "SIZE", "TYPE", "SMART"}, rows)
	}
	if hs != nil {
		var vrows [][]string
		for _, v := range hs.VDevs {
			if !isLeafVDev(v) {
				continue
			}
			vrows = append(vrows, []string{v.Name, v.Pool, v.State,
				fmt.Sprint(v.RErr), fmt.Sprint(v.WErr), fmt.Sprint(v.CErr),
				vdevSize(v), m.vdevDiskLabel(host, v)})
		}
		lines = modalTable(lines, "vdevs", []string{"VDEV", "POOL", "STATE", "READ", "WRITE", "CKSUM", "SIZE", "DISK"}, vrows)
	}
	if hs != nil && len(hs.Pools) > 0 {
		var rows [][]string
		for _, p := range hs.Pools {
			pct := int64(0)
			if p.Size > 0 {
				pct = p.Alloc * 100 / p.Size
			}
			rows = append(rows, []string{p.Name, p.Health, humanShort(p.Size),
				fmt.Sprintf("%s (%d%%)", humanShort(p.Alloc), pct), humanShort(p.Free), fmt.Sprintf("%d%%", p.Frag)})
		}
		lines = modalTable(lines, "pools", []string{"POOL", "HEALTH", "SIZE", "ALLOC", "FREE", "FRAG"}, rows)
	}
	if hs != nil && len(hs.Datasets) > 0 {
		var rows [][]string
		for _, d := range hs.Datasets {
			rows = append(rows, []string{d.Name, humanShort(d.Used), humanShort(d.Refer), orDash(d.Mountpoint), m.snapCell(d)})
		}
		lines = modalTable(lines, "datasets", []string{"DATASET", "USED", "REFER", "MOUNTPOINT", "SNAPSHOT"}, rows)
	}
	return makeModal("host "+host, lines)
}

// poolModalFor looks up a pool by host:name (nil if not cached).
func (m *Model) poolModalFor(host, pool string) *textModal {
	for i := range m.data.States {
		if m.data.States[i].Host != host {
			continue
		}
		for _, p := range m.data.States[i].Pools {
			if p.Name == pool {
				var dss []Dataset
				for _, d := range m.data.States[i].Datasets {
					if d.Pool() == pool {
						dss = append(dss, d)
					}
				}
				return m.poolModal(p, m.vdevsFor(m.data.States[i].Host, pool), dss)
			}
		}
	}
	return nil
}

// --- object detail modals (everything we know) ---

// propsDump appends the full sorted property map with sources.
func propsDump(lines []string, title string, props, srcs map[string]string) []string {
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
	keyW := 0
	for _, k := range keys {
		if len(k) > keyW {
			keyW = len(k)
		}
	}
	valW, srcW := 5, 6 // min widths for "VALUE" / "SOURCE"
	for _, k := range keys {
		if v := len(props[k]); v > valW {
			valW = v
		}
		if s := srcs[k]; len(s) > srcW {
			srcW = len(s)
		}
	}
	lines = append(lines, "", sectionHeader(title))
	lines = append(lines, styleCyan.Render(fmt.Sprintf("%-*s  %-*s  %s", keyW, "PROPERTY", valW, "VALUE", "SOURCE")))
	for _, k := range keys {
		src := orDash(srcs[k])
		lines = append(lines, fmt.Sprintf("%-*s  %-*s  %s", keyW, k, valW, props[k], src))
	}
	return lines
}

func (m *Model) datasetModal(d *Dataset) *textModal {
	mp := orDash(d.Mountpoint)
	if d.Mountpoint != "" && d.Mountpoint != "none" && !d.Mounted {
		mp += " (unmounted)"
	}
	kv := func(k, v string) string { return fmt.Sprintf("%-11s%s", k, v) }
	lines := []string{
		kv("host", d.Host),
		kv("type", d.Type),
		kv("mountpoint", mp),
		kv("space", fmt.Sprintf("used %s  avail %s  refer %s  written %s",
			humanShort(d.Used), humanShort(d.Avail), humanShort(d.Refer), humanShort(d.Written))),
		kv("created", time.Unix(d.Creation, 0).Format("2006-01-02 15:04:05")),
	}
	if d.Encrypted() {
		lines = append(lines, kv("encryption", d.Encryption+"  key: "+d.KeyStatus))
	}
	if d.Origin != "" {
		lines = append(lines, kv("origin", d.Origin+"  (clone)"))
	}
	if d.ResumeToken != "" {
		lines = append(lines, kv("resume", "interrupted recv, token present"))
	}
	snaps := m.data.SnapsByReplica[d.Host+":"+d.Name]
	if len(snaps) > 0 {
		headers := []string{"SNAPSHOT", "USED", "REFER", "GUID"}
		var rows [][]string
		for i := len(snaps) - 1; i >= 0; i-- {
			s := snaps[i]
			rows = append(rows, []string{s.Name, orDash(humanShort(s.Used)), humanShort(s.Refer), orDash(s.GUID)})
		}
		lines = modalTable(lines, "snapshots", headers, rows)
	}
	return makeModal("dataset "+d.Host+":"+d.Name, propsDump(lines, "zfs properties", d.Props, d.PropSrc))
}

func snapshotModal(s Snapshot) *textModal {
	lines := []string{
		"host      " + s.Host,
		"dataset   " + s.Dataset,
		"guid      " + orDash(s.GUID) + "   (THE cross-replica identity)",
		"created   " + time.Unix(s.Creation, 0).Format("2006-01-02 15:04:05"),
		fmt.Sprintf("space     used %s  refer %s", humanShort(s.Used), humanShort(s.Refer)),
		fmt.Sprintf("createtxg %d   (pool-local)", s.CreateTXG),
	}
	return makeModal("snapshot "+s.Dataset+"@"+s.Name, propsDump(lines, "zfs properties", s.Props, s.PropSrc))
}

func (m *Model) poolModal(p Pool, vdevs []VDev, dss []Dataset) *textModal {
	lines := []string{
		"health    " + p.Health,
		fmt.Sprintf("space     size %s  alloc %s (%d%%)  free %s",
			humanShort(p.Size), humanShort(p.Alloc), p.Cap, humanShort(p.Free)),
		fmt.Sprintf("frag      %d%%   dedup %s", p.Frag, p.Dedup),
	}
	if len(vdevs) > 0 {
		var rows [][]string
		for _, v := range vdevs {
			rows = append(rows, []string{v.Name, v.State,
				fmt.Sprint(v.RErr), fmt.Sprint(v.WErr), fmt.Sprint(v.CErr),
				vdevSize(v), m.vdevDiskLabel(p.Host, v)})
		}
		lines = modalTable(lines, "vdevs", []string{"VDEV", "STATE", "READ", "WRITE", "CKSUM", "SIZE", "DISK"}, rows)
	}
	if len(dss) > 0 {
		var rows [][]string
		for _, d := range dss {
			rows = append(rows, []string{d.Name, humanShort(d.Used), humanShort(d.Refer), orDash(d.Mountpoint), m.snapCell(d)})
		}
		lines = modalTable(lines, "datasets", []string{"DATASET", "USED", "REFER", "MOUNTPOINT", "SNAPSHOT"}, rows)
	}
	out := appendVerbatim(lines, "zpool status", p.Props["_zpool-status"])
	out = propsDump(out, "zpool properties", p.Props, p.PropSrc)
	return makeModal("pool "+p.Host+":"+p.Name, out)
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
		fmt.Sprintf("space     size %s  media %s  transport %s", humanShort(d.Size), rota, orDash(d.Transport)),
		"smart     " + diskHealth(d),
	}
	out := appendVerbatim(lines, "smartctl -a", d.Smart)
	out = propsDump(out, "lsblk properties", d.Props, nil)
	return makeModal("disk "+d.Host+":"+baseName(d.Dev), out)
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
	return makeModal("job "+j.ID, lines)
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
