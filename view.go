// Unified object views. ONE builder per object type (host, pool, dataset,
// snapshot, disk, vdev, job, group) produces a structured, interactive view
// that renders two ways from the same code:
//
//   - compact, embedded in the Details pane (full=false): no section headers,
//     low-priority blocks (property dumps, verbatim zpool status / SMART)
//     dropped — just the key chips and selectable tables.
//   - a full, scrollable interactive window (full=true): cyan centered section
//     headers, the complete property maps and verbatim text — the old text
//     modal look, now with a row cursor and Enter-to-drill.
//
// Windows live on a stack (Model.windows); Enter on a selectable row pushes a
// sub-window, Esc pops it. Builders are re-run every render so live elements
// (spinners, transfer progress/ETA) stay current in both pane and window.
package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// viewLine is one line of an object view. Used by both the compact Details
// pane and full interactive windows.
type viewLine struct {
	text    string
	style   lipgloss.Style
	open    func() *objView // non-nil makes the row selectable → pushes a window
	section bool            // centered "── title ──" header; full windows only
}

// objView is a rebuildable, interactive view of one object. build is called on
// every render (so spinners/progress stay live); full toggles compact-pane vs
// full-window content. The returned segs are pane title chips; segs[0] doubles
// as the window title.
type objView struct {
	build func(m *Model, full bool) (segs []string, lines []viewLine)
}

// window is a stacked, scrollable, interactive object window.
type window struct {
	view *objView
	cur  int // selectable-line cursor
	off  int // vertical scroll offset
	xoff int // horizontal pan (display columns)
}

func vline(text string, style lipgloss.Style) viewLine { return viewLine{text: text, style: style} }
func vsection(title string) viewLine                   { return viewLine{text: title, section: true} }
func vrow(text string, style lipgloss.Style, open func() *objView) viewLine {
	return viewLine{text: text, style: style, open: open}
}

// toViews wraps the []string emitted by propsDump into plain viewLines (tabs
// expanded; any embedded ANSI, e.g. the cyan header/label, is preserved by the
// no-op styleNormal).
func toViews(ss []string) []viewLine {
	out := make([]viewLine, 0, len(ss))
	for _, s := range ss {
		out = append(out, vline(expandTabs(s), styleNormal))
	}
	return out
}

// detSelectable returns the indices of selectable lines.
func detSelectable(lines []viewLine) []int {
	var sel []int
	for i, l := range lines {
		if l.open != nil {
			sel = append(sel, i)
		}
	}
	return sel
}

func firstSelectable(lines []viewLine) int {
	for i, l := range lines {
		if l.open != nil {
			return i
		}
	}
	return 0
}

// lastSelectable returns the index of the last selectable line, or -1 if none.
func lastSelectable(lines []viewLine) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].open != nil {
			return i
		}
	}
	return -1
}

// selInView returns the first (dir>=0) or last (dir<0) selectable row inside
// the viewport [off, off+bodyH), or -1 if none is visible.
func selInView(sel []int, off, bodyH, dir int) int {
	found := -1
	for _, li := range sel {
		if li >= off && li < off+bodyH {
			if dir < 0 {
				found = li // keep last
			} else {
				return li // first
			}
		}
	}
	return found
}

// scrollMove combines cursor stepping (among selectable rows) with free
// scrolling. When the cursor is off-screen (e.g. after a page jump) it is
// pulled onto a visible selectable row so the very next press responds; only
// when the visible region has no selectable row at all does it free-scroll —
// which is also how trailing non-selectable content (property dumps) is
// reached past the last parkable row. Caller clamps the returned offset.
func scrollMove(lines []viewLine, sel []int, cur, off, dir, bodyH int) (int, int) {
	if len(sel) == 0 {
		return cur, off + dir
	}
	if cur < off || cur >= off+bodyH { // cursor off-screen
		if v := selInView(sel, off, bodyH, dir); v >= 0 {
			return v, off // grab a visible row instead of scrolling blindly
		}
		return cur, off + dir // nothing to park on here: free-scroll
	}
	newCur, newOff := moveSelectable(lines, sel, cur, off, dir, bodyH)
	if newCur == cur { // at first/last selectable: free-scroll to reveal more
		return cur, off + dir
	}
	return newCur, newOff
}

// moveSelectable steps a row cursor among selectable lines, scrolling to keep
// it visible (pulling up to 3 non-selectable context lines — e.g. section
// headers — back into view). Shared by the Details pane and windows.
func moveSelectable(lines []viewLine, sel []int, cur, off, dir, bodyH int) (int, int) {
	if len(sel) == 0 {
		return cur, off
	}
	pos := 0
	for i, li := range sel {
		if li >= cur {
			pos = i
			break
		}
		pos = i + 1
	}
	if li := sel[min(max(pos, 0), len(sel)-1)]; li == cur {
		pos += dir
	} else if dir < 0 {
		pos--
	}
	pos = min(max(pos, 0), len(sel)-1)
	cur = sel[pos]
	top := cur
	for steps := 0; top > 0 && lines[top-1].open == nil && steps < 3; steps++ {
		top--
	}
	if top < off {
		off = top
	}
	if cur >= off+bodyH {
		off = cur - bodyH + 1
	}
	return cur, off
}

// --- table helpers (shared layout) ---

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

// vtable emits a table as viewLines: a cyan column-header row then data rows,
// flush (no bullets/indent). The column header labels the table, so there's no
// separate centered title; the full window just prepends a blank separator so
// consecutive tables read as distinct blocks. rstyle (may be nil) styles each
// data row; open (may be nil) makes rows selectable, drilling into open(i).
func vtable(full bool, headers []string, rows [][]string, right map[int]bool,
	rstyle func(i int) lipgloss.Style, open func(i int) *objView) []viewLine {
	if len(rows) == 0 {
		return nil
	}
	ws := colWidths(headers, rows)
	var out []viewLine
	if full {
		out = append(out, vline("", styleNormal)) // separator above the block
	}
	out = append(out, vline(tableLine("", ws, headers, right), styleCyan))
	for i, row := range rows {
		i := i
		st := styleNormal
		if rstyle != nil {
			st = rstyle(i)
		}
		var op func() *objView
		if open != nil {
			op = func() *objView { return open(i) }
		}
		out = append(out, vrow(tableLine("", ws, row, right), st, op))
	}
	return out
}

// --- lookups (live, by identity) ---

func (m *Model) findHostState(host string) *HostState {
	for i := range m.data.States {
		if m.data.States[i].Host == host {
			return &m.data.States[i]
		}
	}
	return nil
}

func (m *Model) findDataset(host, name string) *Dataset {
	hs := m.findHostState(host)
	if hs == nil {
		return nil
	}
	for i := range hs.Datasets {
		if hs.Datasets[i].Name == name {
			return &hs.Datasets[i]
		}
	}
	return nil
}

func (m *Model) findPool(host, pool string) *Pool {
	hs := m.findHostState(host)
	if hs == nil {
		return nil
	}
	for i := range hs.Pools {
		if hs.Pools[i].Name == pool {
			return &hs.Pools[i]
		}
	}
	return nil
}

// findGroupByKey re-resolves a group across reloads by its stable Key (smallest
// member host:name). DisplayName can collide (e.g. a pool's root dataset and a
// child literally named after the pool both render as "cml-root"), so name is
// NOT safe for lookup — that mixed up which group the Details pane showed.
func (m *Model) findGroupByKey(key string) *Group {
	for _, g := range m.data.Groups {
		if g.Key() == key {
			return g
		}
	}
	return nil
}

func (m *Model) findJob(host, id string) *JobInfo {
	hs := m.findHostState(host)
	if hs == nil {
		return nil
	}
	for i := range hs.Jobs {
		if hs.Jobs[i].ID == id {
			return &hs.Jobs[i]
		}
	}
	return nil
}

// --- group / replica views ---

func (m *Model) groupView(g *Group) *objView {
	key := g.Key()
	return &objView{build: func(m *Model, full bool) ([]string, []viewLine) {
		gg := m.findGroupByKey(key)
		if gg == nil {
			gg = g
		}
		// The REPLICAS table is the window's only/first block; drop the leading
		// blank vtable self-spaces with so the window opens straight into it.
		lines := m.replicaTable(gg.Members, full)
		if len(lines) > 0 && lines[0].text == "" {
			lines = lines[1:]
		}
		return []string{"group " + gg.DisplayName}, lines
	}}
}

// cellView covers the rare cell holding more than one replica.
func (m *Model) cellView(replicas []*Replica) *objView {
	rs := replicas
	return &objView{build: func(m *Model, full bool) ([]string, []viewLine) {
		segs := []string{rs[0].Dataset.Host + ":…", fmt.Sprintf("%d replicas in this cell", len(rs))}
		return segs, m.replicaTable(rs, full)
	}}
}

// replicaTable renders selectable member rows; the LATEST column gains a live
// spinner + progress while a job touches the replica.
func (m *Model) replicaTable(members []*Replica, full bool) []viewLine {
	headers := []string{"REPLICA", "STATUS", "USED", "AVAIL", "SNAPSHOTS", "LATEST (REFER)"}
	var rows [][]string
	for _, r := range members {
		name := r.Dataset.Host + ":" + r.Dataset.Name
		used := orDash(humanShort(r.Dataset.Used))
		avail := orDash(humanShort(r.Dataset.Avail))
		nsnaps := strconv.Itoa(len(m.data.SnapsByReplica[name]))
		latest := replicaBrief(r)
		if j := m.runningJobFor(r); j != nil {
			verb := "send"
			if replicaKeyOf(r) == j.Meta["dest"] {
				verb = "recv"
			}
			latest += "  " + m.spinnerFrame() + verb + " " + jobProgressLine(j)
		}
		rows = append(rows, []string{name, replicaStatus(r), used, avail, nsnaps, latest})
	}
	open := func(i int) *objView {
		r := members[i]
		return m.datasetView(r.Dataset.Host, r.Dataset.Name)
	}
	return vtable(full, headers, rows, map[int]bool{2: true, 3: true, 4: true}, nil, open)
}

// --- dataset view ---

func (m *Model) datasetView(host, name string) *objView {
	return &objView{build: func(m *Model, full bool) ([]string, []viewLine) {
		d := m.findDataset(host, name)
		if d == nil {
			return []string{"dataset " + host + ":" + name}, []viewLine{vline("not in cache", styleNormal)}
		}
		return m.datasetContent(d, full)
	}}
}

func (m *Model) datasetContent(d *Dataset, full bool) ([]string, []viewLine) {
	snaps := m.data.SnapsByReplica[d.Host+":"+d.Name]
	var latest *Snapshot
	if len(snaps) > 0 {
		latest = &snaps[len(snaps)-1]
	}

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
	if latest != nil {
		info += ", @" + latest.Name
		if latest.Refer > 0 {
			info += " (" + humanShort(latest.Refer) + ")"
		}
	} else {
		info += ", no snapshots"
	}
	if d.Written > 0 {
		info += " +" + humanShort(d.Written)
	}
	segs = append(segs, info)

	var lines []viewLine
	if full {
		mp := orDash(d.Mountpoint)
		if d.Mountpoint != "" && d.Mountpoint != "none" && !d.Mounted {
			mp += " (unmounted)"
		}
		// Right-aligned labels so colons line up; comma-separated value phrases.
		kv := func(k, v string) viewLine { return vline(fmt.Sprintf("%10s: %s", k, v), styleNormal) }
		lines = append(lines, kv("created", snapCreated(d.Creation)))
		if d.Encrypted() {
			enc := d.Encryption + ", key: " + d.KeyStatus
			if root := props(d.Props).str("encryptionroot"); root != "" {
				enc += ", root: " + root
			}
			lines = append(lines, kv("encryption", enc))
		}
		ch := props(d.Props).num("usedbychildren")
		space := hz(d.OwnUsed()) + " used"
		if ch > 0 {
			space += " + " + humanShort(ch) + " children"
		}
		space += fmt.Sprintf(", %s avail, %s refer, %s written", hz(d.Avail), hz(d.Refer), hz(d.Written))
		lines = append(lines, kv("mountpoint", mp), kv("space", space))
		if d.Origin != "" {
			lines = append(lines, kv("origin", d.Origin+" (clone)"))
		}
		if d.ResumeToken != "" {
			lines = append(lines, kv("resume", "interrupted recv, token present"))
		}
	} else {
		if d.Origin != "" {
			lines = append(lines, vline("clone of "+d.Origin, styleClone))
		}
		if d.ResumeToken != "" {
			lines = append(lines, vline("resume token present", styleCyan))
		}
	}

	if jobs := m.data.JobsByReplica[d.Host+":"+d.Name]; len(jobs) > 0 {
		lines = append(lines, m.vjobs(jobs, 5, full)...)
	}

	if len(snaps) > 0 {
		headers := []string{"SNAPSHOT", "USED", "REFER", "GUID", "CREATED"}
		var rows [][]string
		for i := len(snaps) - 1; i >= 0; i-- {
			s := snaps[i]
			rows = append(rows, []string{s.Name, orDash(humanShort(s.Used)), humanShort(s.Refer), orDash(s.GUID), strings.TrimSuffix(snapCreated(s.Creation), " UTC")})
		}
		open := func(i int) *objView { return snapshotView(snaps[len(snaps)-1-i]) }
		lines = append(lines, vtable(full, headers, rows, map[int]bool{1: true, 2: true}, nil, open)...)
	} else if !full {
		lines = append(lines, vline("no snapshots", styleSection))
	}
	if full {
		lines = append(lines, toViews(propsDump(nil, d.Props, d.PropSrc))...)
	}
	return segs, lines
}

// --- snapshot view ---

func snapshotView(s Snapshot) *objView {
	return &objView{build: func(m *Model, full bool) ([]string, []viewLine) { return snapshotContent(s, full) }}
}

// utcStamp formats a time as a UTC wall-clock stamp, used for every
// human-facing timestamp so the displayed time is unambiguous.
func utcStamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") + " UTC" }

// snapCreated formats a snapshot creation epoch as a UTC wall-clock stamp.
func snapCreated(epoch int64) string { return utcStamp(time.Unix(epoch, 0)) }

func snapshotContent(s Snapshot, full bool) ([]string, []viewLine) {
	segs := []string{"snapshot " + s.Host + ":" + s.Dataset + "@" + s.Name}
	// host/dataset are in the title; guid/space/createtxg are in the props dump.
	kv := func(k, v string) viewLine { return vline(fmt.Sprintf("%7s: %s", k, v), styleNormal) }
	space := humanShort(s.Refer) + " refer"
	if s.Used > 0 {
		space = humanShort(s.Used) + " used, " + space
	}
	lines := []viewLine{
		kv("created", snapCreated(s.Creation)),
		kv("space", space),
	}
	if full {
		lines = append(lines, toViews(propsDump(nil, s.Props, s.PropSrc))...)
	}
	return segs, lines
}

// --- host view ---

func (m *Model) hostView(host string) *objView {
	return &objView{build: func(m *Model, full bool) ([]string, []viewLine) { return m.hostContent(host, full) }}
}

func (m *Model) hostContent(host string, full bool) ([]string, []viewLine) {
	segs := []string{"host " + host}
	hs := m.findHostState(host)
	var size, alloc, free int64
	if hs != nil {
		for _, p := range hs.Pools {
			size += p.Size
			alloc += p.Alloc
			free += p.Free
		}
	}
	if size > 0 {
		segs = append(segs, spaceSummary(size, alloc, free))
	}

	var h Host
	found := false
	for _, hh := range m.hosts {
		if hh.Name == host {
			h, found = hh, true
		}
	}

	var lines []viewLine
	if full {
		kv := func(k, v string) viewLine { return vline(fmt.Sprintf("%9s: %s", k, v), styleNormal) }
		if found {
			ssh := h.URL()
			if h.NeedSudo() {
				ssh += " (sudo)"
			}
			if h.SendUser != "" {
				ssh += ", send_user=" + h.SendUser
			}
			if h.RecvUser != "" {
				ssh += ", recv_user=" + h.RecvUser
			}
			if h.Pool != "" {
				ssh += ", pool=" + h.Pool + " (not enforced yet)"
			}
			lines = append(lines, kv("ssh", ssh))
		}
		if size > 0 {
			lines = append(lines, kv("space", spaceSummary(size, alloc, free)))
		}
		for _, hi := range m.data.Hosts {
			if hi.Name != host {
				continue
			}
			if hi.BootTime > 0 {
				bt := time.Unix(hi.BootTime, 0)
				lines = append(lines, kv("uptime", fmt.Sprintf("%s (booted %s)",
					fmtUptime(time.Since(bt)), utcStamp(bt))))
			}
			when := utcStamp(hi.LastRefresh)
			if !hi.Reachable {
				lines = append(lines, vline(fmt.Sprintf("%9s: UNREACHABLE at last refresh (%s): %s", "refreshed", when, hi.LastError), styleErr))
				continue
			}
			info := when
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
			lines = append(lines, kv("refreshed", info))
		}
		if hs != nil {
			lines = append(lines, m.poolTable(hs.Pools, full)...)
			lines = append(lines, m.diskTable(hs.Disks, full)...)
			lines = append(lines, m.vdevTable(host, leafVDevs(hs.VDevs), true, full)...)
			lines = append(lines, m.datasetTable(host, hs.Datasets, full)...)
			if len(hs.Jobs) > 0 {
				// Sorted newest-first (meta "created", ID as tiebreak).
				jobs := append([]JobInfo(nil), hs.Jobs...)
				sort.SliceStable(jobs, func(i, j int) bool { return jobLess(jobs[i], jobs[j]) })
				lines = append(lines, m.vjobs(jobs, 25, full)...)
			}
		}
	} else {
		access := ""
		if found {
			access = h.URL()
			if h.NeedSudo() {
				access += " (sudo)"
			}
		}
		open := func() *objView { return m.hostView(host) }
		for _, hi := range m.data.Hosts {
			if hi.Name != host {
				continue
			}
			when := utcStamp(hi.LastRefresh)
			if hi.Reachable {
				lines = append(lines, vrow("ssh: "+access+", refreshed "+when, styleSection, open))
			} else {
				lines = append(lines, vrow("UNREACHABLE at last refresh ("+when+"): "+hi.LastError, styleErr, open))
			}
		}
		if hs != nil {
			lines = append(lines, m.poolTable(hs.Pools, full)...)
			lines = append(lines, m.diskTable(hs.Disks, full)...)
		}
	}
	return segs, lines
}

// --- pool view ---

func (m *Model) poolView(host, pool string) *objView {
	return &objView{build: func(m *Model, full bool) ([]string, []viewLine) { return m.poolContent(host, pool, full) }}
}

func (m *Model) poolContent(host, pool string, full bool) ([]string, []viewLine) {
	segs := []string{"pool " + host + ":" + pool}
	p := m.findPool(host, pool)
	if p == nil {
		return segs, []viewLine{vline("not in cache", styleNormal)}
	}
	if p.Size > 0 {
		segs = append(segs, spaceSummary(p.Size, p.Alloc, p.Free))
	}
	var dss []Dataset
	if hs := m.findHostState(host); hs != nil {
		for _, d := range hs.Datasets {
			if d.Pool() == pool {
				dss = append(dss, d)
			}
		}
	}

	status := p.Props["_zpool-status"]
	var lines []viewLine
	if full {
		if status != "" {
			// Top section IS the verbatim zpool status: vdev lines are focusable
			// (Enter → vdev window), scan/errors redden on trouble, and a space
			// summary is appended — so there's no separate VDEV table.
			lines = append(lines, m.zpoolStatusLines(host, pool, p)...)
		} else {
			lines = append(lines, vline("health    "+p.Health, styleNormal))
			if scan := scanSummary(p); scan != "" {
				lines = append(lines, vline("scan      "+scan, scanStyle(scan)))
			}
			lines = append(lines, vline(fmt.Sprintf("space     %s, %d%% frag, %s dedup",
				spaceSummary(p.Size, p.Alloc, p.Free), p.Frag, p.Dedup), styleNormal))
			lines = append(lines, m.vdevTable(host, m.vdevsFor(host, pool), false, full)...)
		}
		lines = append(lines, m.datasetTable(host, dss, full)...)
		lines = append(lines, toViews(propsDump(nil, p.Props, p.PropSrc))...)
		return segs, lines
	}
	// Compact pane: scan/errors bullets (drill into the pool window) + tables.
	open := func() *objView { return m.poolView(host, pool) }
	for _, ln := range strings.Split(status, "\n") {
		t := strings.TrimSpace(ln)
		if s, ok := strings.CutPrefix(t, "scan:"); ok {
			scan := strings.TrimSpace(s)
			st := styleSection
			if scanHasErrors(scan) {
				st = styleErr
			}
			lines = append(lines, vrow(scan, st, open))
		}
		if s, ok := strings.CutPrefix(t, "errors:"); ok {
			if e := strings.TrimSpace(s); e != "No known data errors" {
				lines = append(lines, vrow("errors: "+e, styleErr, open))
			}
		}
	}
	lines = append(lines, m.vdevTable(host, m.vdevsFor(host, pool), false, full)...)
	lines = append(lines, m.datasetTable(host, dss, full)...)
	return segs, lines
}

// scanHasErrors reports whether a scan: summary mentions errors.
func scanHasErrors(scan string) bool {
	return strings.Contains(scan, "error") && !strings.Contains(scan, "with 0 errors")
}

func scanStyle(scan string) lipgloss.Style {
	if scanHasErrors(scan) {
		return styleErr
	}
	return styleNormal
}

// zpoolStatusLines renders the verbatim `zpool status` as the pool window's top
// section: a cyan "zpool status" label, the text with vdev lines turned into
// focusable rows (Enter → vdev window, reddened on errors), the scan/errors
// lines reddened on trouble, and a trailing space/frag/dedup summary.
func (m *Model) zpoolStatusLines(host, pool string, p *Pool) []viewLine {
	byName := map[string]VDev{}
	if hs := m.findHostState(host); hs != nil {
		for _, v := range hs.VDevs {
			if v.Pool == pool {
				byName[v.Name] = v
			}
		}
	}
	// No label, no leading blank: the window opens straight into the zpool
	// status text (the box padding already provides top spacing).
	var out []viewLine
	for _, raw := range strings.Split(strings.TrimRight(p.Props["_zpool-status"], "\n"), "\n") {
		text := expandTabs(stripCtrl(raw))
		trimmed := strings.TrimSpace(raw)
		fields := strings.Fields(raw)
		// A vdev row: the first token names a known vdev → focusable.
		if len(fields) > 0 {
			if v, ok := byName[fields[0]]; ok {
				st := styleNormal
				if v.State != "ONLINE" || v.RErr > 0 || v.WErr > 0 || v.CErr > 0 {
					st = styleErr
				}
				out = append(out, vrow(text, st, func() *objView { return m.vdevView(host, v) }))
				continue
			}
		}
		st := styleNormal
		if strings.HasPrefix(trimmed, "scan:") && scanHasErrors(trimmed) {
			st = styleErr
		} else if strings.HasPrefix(trimmed, "errors:") && !strings.Contains(trimmed, "No known data errors") {
			st = styleErr
		}
		out = append(out, vline(text, st))
	}
	// Right-align "space" to the same 6-char colon column zpool status uses
	// ("  pool:", " state:", "  scan:", "errors:").
	out = append(out, vline(fmt.Sprintf("%6s: %s, %d%% frag, %s dedup",
		"space", spaceSummary(p.Size, p.Alloc, p.Free), p.Frag, p.Dedup), styleNormal))
	return out
}

// spaceSummary formats capacity as "1.9G total, 315M (16%) used, 1.6G free" —
// shared by the pool/host detail chips and their window space rows.
func spaceSummary(size, alloc, free int64) string {
	return fmt.Sprintf("%s total, %s (%d%%) used, %s free",
		humanShort(size), humanShort(alloc), pctOf(alloc, size), humanShort(free))
}

// zpoolStatusField returns the trimmed remainder of the first line in the
// verbatim `zpool status` blob that begins with label (e.g. "scan:",
// "errors:"), and whether such a line was found.
func zpoolStatusField(p *Pool, label string) (string, bool) {
	if p == nil {
		return "", false
	}
	for _, ln := range strings.Split(p.Props["_zpool-status"], "\n") {
		if s, ok := strings.CutPrefix(strings.TrimSpace(ln), label); ok {
			return strings.TrimSpace(s), true
		}
	}
	return "", false
}

// scanSummary returns the trimmed `scan:` line from the verbatim zpool status
// (the scrub/resilver status), or "" if absent.
func scanSummary(p *Pool) string {
	s, _ := zpoolStatusField(p, "scan:")
	return s
}

// --- disk / vdev / job / dataset tables ---

func (m *Model) diskTable(disks []Disk, full bool) []viewLine {
	if len(disks) == 0 {
		return nil
	}
	ds := append([]Disk(nil), disks...)
	sort.SliceStable(ds, func(i, j int) bool {
		if ds[i].Dev != ds[j].Dev {
			return ds[i].Dev < ds[j].Dev
		}
		return ds[i].ID < ds[j].ID
	})
	headers := []string{"DISK", "MODEL", "SERIAL", "SIZE", "TYPE", "SMART"}
	var rows [][]string
	for _, d := range ds {
		rows = append(rows, diskRow(d))
	}
	rstyle := func(i int) lipgloss.Style {
		if h := diskHealth(ds[i]); h == "WARN" || h == "FAIL" {
			return styleErr
		}
		return styleNormal
	}
	open := func(i int) *objView { return diskView(ds[i]) }
	return vtable(full, headers, rows, map[int]bool{3: true}, rstyle, open)
}

func (m *Model) vdevTable(host string, vdevs []VDev, showPool, full bool) []viewLine {
	if len(vdevs) == 0 {
		return nil
	}
	headers := []string{"VDEV", "STATE", "READ", "WRITE", "CKSUM", "SIZE", "DISK"}
	right := map[int]bool{2: true, 3: true, 4: true, 5: true}
	if showPool {
		headers = []string{"VDEV", "POOL", "STATE", "READ", "WRITE", "CKSUM", "SIZE", "DISK"}
		right = map[int]bool{3: true, 4: true, 5: true, 6: true}
	}
	var rows [][]string
	for _, v := range vdevs {
		rows = append(rows, m.vdevRow(host, v, showPool))
	}
	rstyle := func(i int) lipgloss.Style {
		v := vdevs[i]
		if v.State != "ONLINE" || v.RErr > 0 || v.WErr > 0 || v.CErr > 0 {
			return styleErr
		}
		return styleNormal
	}
	open := func(i int) *objView { return m.vdevView(host, vdevs[i]) }
	return vtable(full, headers, rows, right, rstyle, open)
}

func (m *Model) poolTable(pools []Pool, full bool) []viewLine {
	if len(pools) == 0 {
		return nil
	}
	headers := []string{"POOL", "HEALTH", "SIZE", "ALLOC", "FREE", "FRAG"}
	var rows [][]string
	for _, p := range pools {
		rows = append(rows, []string{p.Name, p.Health, humanShort(p.Size),
			fmt.Sprintf("%s (%d%%)", humanShort(p.Alloc), pctOf(p.Alloc, p.Size)), humanShort(p.Free), fmt.Sprintf("%d%%", p.Frag)})
	}
	open := func(i int) *objView { return m.poolView(pools[i].Host, pools[i].Name) }
	return vtable(full, headers, rows, map[int]bool{2: true, 4: true}, nil, open)
}

func (m *Model) datasetTable(host string, dss []Dataset, full bool) []viewLine {
	if len(dss) == 0 {
		return nil
	}
	headers := []string{"DATASET", "USED", "REFER", "MOUNTPOINT", "SNAPSHOT"}
	var rows [][]string
	for _, d := range dss {
		rows = append(rows, []string{d.Name, humanShort(d.Used), humanShort(d.Refer), orDash(d.Mountpoint), m.snapCell(d)})
	}
	open := func(i int) *objView { return m.datasetView(dss[i].Host, dss[i].Name) }
	return vtable(full, headers, rows, map[int]bool{1: true, 2: true}, nil, open)
}

// vjobs renders selectable job rows (newest first), live status + progress.
func (m *Model) vjobs(jobs []JobInfo, limit int, full bool) []viewLine {
	type entry struct {
		j            JobInfo
		status, desc string
		st           lipgloss.Style
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
		case "snapshot":
			desc = "snapshot " + j.Meta["dataset"] + "@" + j.Meta["snapshot"]
		}
		status, st := "", styleNormal
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
	var lines []viewLine
	if full {
		lines = append(lines, vline("", styleNormal)) // block separator
	}
	// Left-aligned cyan "JOBS" label (job rows have no column header to self-label).
	lines = append(lines, vline("JOBS", styleCyan))
	for _, e := range entries {
		e := e
		stPad := strings.Repeat(" ", stW-lipgloss.Width(e.status))
		host, id := e.j.Host, e.j.ID
		lines = append(lines, vrow(fmt.Sprintf("%s%s %-*s  %s", e.status, stPad, idW, e.j.ID, e.desc),
			e.st, func() *objView { return m.jobView(host, id) }))
	}
	return lines
}

func (m *Model) jobView(host, id string) *objView {
	return &objView{build: func(m *Model, full bool) ([]string, []viewLine) {
		j := m.findJob(host, id)
		if j == nil {
			return []string{"job " + host + ":" + id}, []viewLine{vline("job not found (finished and pruned?)", styleNormal)}
		}
		return jobContent(*j, full)
	}}
}

func jobContent(j JobInfo, full bool) ([]string, []viewLine) {
	segs := []string{"job " + j.Host + ":" + j.ID}
	lines := []viewLine{
		vline("host      "+j.Host, styleNormal),
		vline("kind      "+j.Kind, styleNormal),
		vline("state     "+j.State+"   ("+j.Status+")", styleNormal),
		vline("dir       "+j.Dir, styleNormal),
	}
	if j.Progress > 0 || j.Expected > 0 {
		lines = append(lines, vline("progress  "+jobProgressLine(&j), styleNormal))
	}
	var keys []string
	for k := range j.Meta {
		if k != "command" && k != "id" && k != "kind" && k != "host" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		lines = append(lines, vline(fmt.Sprintf("%-9s %s", k, j.Meta[k]), styleNormal))
	}
	if cmd := j.Meta["command"]; cmd != "" {
		lines = append(lines, vline("", styleNormal), vsection("script"))
		for _, cl := range strings.Split(strings.TrimRight(cmd, "\n"), "\n") {
			lines = append(lines, vline(cl, styleNormal))
		}
	}
	if log := strings.TrimRight(j.Log, "\n"); log != "" {
		lines = append(lines, vline("", styleNormal), vsection("log"))
		for _, ll := range strings.Split(log, "\n") {
			lines = append(lines, vline(expandTabs(stripCtrl(ll)), styleNormal))
		}
	}
	return segs, lines
}

// --- disk / vdev views ---

func diskView(d Disk) *objView {
	return &objView{build: func(m *Model, full bool) ([]string, []viewLine) { return diskContent(d, full) }}
}

func diskContent(d Disk, full bool) ([]string, []viewLine) {
	segs := []string{"disk " + d.Host + ":" + d.Dev}
	// One-liner summary, e.g. "1.8T NVME SSD, SMART: OK".
	lines := []viewLine{
		vline(fmt.Sprintf("%s %s, SMART: %s", humanShort(d.Size), strings.ToUpper(diskType(d)), diskHealth(d)), styleNormal),
	}
	if len(d.Parts) > 0 {
		headers := []string{"PARTITION", "SIZE", "FSTYPE", "LABEL", "MOUNTPOINT"}
		var rows [][]string
		for _, p := range d.Parts {
			rows = append(rows, []string{
				firstNonEmpty(p["NAME"], p["KNAME"]),
				partSize(p),
				p["FSTYPE"],
				firstNonEmpty(p["PARTLABEL"], p["LABEL"]),
				firstNonEmpty(p["MOUNTPOINT"], p["MOUNTPOINTS"]),
			})
		}
		host, parts := d.Host, d.Parts
		open := func(i int) *objView { return partitionView(host, parts[i]) }
		lines = append(lines, vtable(full, headers, rows, map[int]bool{1: true}, nil, open)...)
	}
	if full {
		// model/serial/wwn/dev all live in the lsblk dump; SMART verbatim last.
		lines = append(lines, toViews(propsDump(nil, d.Props, nil))...)
		if smart := strings.TrimRight(d.Smart, "\n"); strings.TrimSpace(smart) != "" {
			lines = append(lines, vline("", styleNormal), vsection("smartctl -a"))
			for _, ln := range strings.Split(smart, "\n") {
				lines = append(lines, vline(stripCtrl(ln), styleNormal))
			}
		}
	}
	return segs, lines
}

// partSize renders a partition's lsblk SIZE (bytes, from `-b`) human-readably.
func partSize(p map[string]string) string {
	if n, err := strconv.ParseInt(p["SIZE"], 10, 64); err == nil {
		return humanShort(n)
	}
	return p["SIZE"]
}

func partitionView(host string, p map[string]string) *objView {
	return &objView{build: func(m *Model, full bool) ([]string, []viewLine) { return partitionContent(host, p, full) }}
}

func partitionContent(host string, p map[string]string, full bool) ([]string, []viewLine) {
	dev := firstNonEmpty(p["PATH"], "/dev/"+firstNonEmpty(p["NAME"], p["KNAME"]))
	segs := []string{"partition " + host + ":" + dev}
	summary := partSize(p)
	if fs := p["FSTYPE"]; fs != "" {
		summary += " " + fs
	}
	if mnt := firstNonEmpty(p["MOUNTPOINT"], p["MOUNTPOINTS"]); mnt != "" {
		summary += " → " + mnt
	}
	lines := []viewLine{vline(summary, styleNormal)}
	if full {
		lines = append(lines, toViews(propsDump(nil, p, nil))...)
	}
	return segs, lines
}

func (m *Model) vdevView(host string, v VDev) *objView {
	return &objView{build: func(m *Model, full bool) ([]string, []viewLine) { return m.vdevContent(host, v, full) }}
}

func (m *Model) vdevContent(host string, v VDev, full bool) ([]string, []viewLine) {
	segs := []string{"vdev " + v.Host + ":" + v.Pool + "/" + v.Name} // host/pool in title
	kv := func(k, val string) viewLine { return vline(fmt.Sprintf("%6s: %s", k, val), styleNormal) }
	lines := []viewLine{
		kv("state", v.State),
		kv("errors", fmt.Sprintf("read %d, write %d, cksum %d", v.RErr, v.WErr, v.CErr)),
	}
	if disk := m.vdevDiskLabel(host, v); disk != "-" {
		lines = append(lines, kv("disk", disk))
	}
	if v.Path != "" {
		lines = append(lines, kv("path", v.Path))
	}
	var sp []string
	if v.Size > 0 {
		sp = append(sp, humanShort(v.Size)+" total")
	}
	if v.Alloc > 0 {
		used := humanShort(v.Alloc) + " used"
		if v.Size > 0 {
			used = fmt.Sprintf("%s (%d%%) used", humanShort(v.Alloc), pctOf(v.Alloc, v.Size))
		}
		sp = append(sp, used)
	}
	if v.Free > 0 {
		sp = append(sp, humanShort(v.Free)+" free")
	}
	if v.Frag > 0 {
		sp = append(sp, fmt.Sprintf("%d%% frag", v.Frag))
	}
	if len(sp) > 0 {
		lines = append(lines, kv("space", strings.Join(sp, ", ")))
	}
	if full {
		lines = append(lines, toViews(propsDump(nil, v.Props, v.PropSrc))...)
	}
	return segs, lines
}

// textView is a plain, non-object window: job-failure logs and other plain text.
func textView(title string, lines []string) *objView {
	return &objView{build: func(m *Model, full bool) ([]string, []viewLine) {
		return []string{title}, toViews(lines)
	}}
}

// --- shared low-level helpers (vdev/disk/snapshot) ---

func leafVDevs(vs []VDev) []VDev {
	var out []VDev
	for _, v := range vs {
		if isLeafVDev(v) {
			out = append(out, v)
		}
	}
	return out
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
	hs := m.findHostState(host)
	if hs == nil {
		return nil
	}
	var out []VDev
	for _, v := range hs.VDevs {
		if v.Pool == pool && isLeafVDev(v) {
			out = append(out, v)
		}
	}
	return out
}

func (m *Model) vdevDiskLabel(host string, v VDev) string {
	if v.DiskID != "" {
		if hs := m.findHostState(host); hs != nil {
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

func diskRow(d Disk) []string {
	return []string{d.Dev, orDash(d.Model), orDash(d.Serial), humanShort(d.Size), diskType(d), diskHealth(d)}
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
	media := "SSD"
	if d.Rotational {
		media = "HDD"
	}
	if d.Transport == "" {
		return media
	}
	return strings.ToUpper(d.Transport) + " " + media
}

// --- verbatim / dumps shared with the full-window renderer ---

// stripCtrl removes control bytes that corrupt fixed-width rendering — most
// notably the carriage returns and ANSI escapes that pv/ssh/zfs progress
// output sprays into job logs (a bare \r is zero-width to lipgloss but resets
// the cursor on a real terminal, shoving the box border mid-line). Tabs are
// kept for expandTabs to handle.
func stripCtrl(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r != '\t' && (r < 0x20 || r == 0x7f) }) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r == '\t' || r >= 0x20 && r != 0x7f {
			return r
		}
		return -1
	}, s)
}

// expandTabs replaces tab characters with spaces to the next 8-column stop.
func expandTabs(s string) string {
	if !strings.Contains(s, "\t") {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		if r == '\t' {
			n := 8 - col%8
			b.WriteString(strings.Repeat(" ", n))
			col += n
		} else {
			b.WriteRune(r)
			col++
		}
	}
	return b.String()
}

// propsDump appends the full sorted property map, preceded by a blank line.
// No centered title — the cyan PROPERTY/VALUE column header labels the block.
// The SOURCE column is shown only when sources exist (zfs/zpool `get`); maps
// without them (e.g. lsblk) render just PROPERTY/VALUE.
func propsDump(lines []string, props, srcs map[string]string) []string {
	if len(props) == 0 {
		return append(lines, "", "(no full property map cached — refresh to populate)")
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		if strings.HasPrefix(k, "_") { // synthetic verbatims rendered separately
			continue
		}
		if props[k] == "" { // skip empty props (e.g. lsblk's unset FS* columns)
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	hasSrc := len(srcs) > 0
	keyW := 0
	for _, k := range keys {
		if len(k) > keyW {
			keyW = len(k)
		}
	}
	lines = append(lines, "")
	if !hasSrc {
		lines = append(lines, styleCyan.Render(fmt.Sprintf("%-*s  %s", keyW, "PROPERTY", "VALUE")))
		for _, k := range keys {
			lines = append(lines, fmt.Sprintf("%-*s  %s", keyW, k, props[k]))
		}
		return lines
	}
	valW := 5 // min width for "VALUE"
	for _, k := range keys {
		if v := len(props[k]); v > valW {
			valW = v
		}
	}
	lines = append(lines, styleCyan.Render(fmt.Sprintf("%-*s  %-*s  %s", keyW, "PROPERTY", valW, "VALUE", "SOURCE")))
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("%-*s  %-*s  %s", keyW, k, valW, props[k], orDash(srcs[k])))
	}
	return lines
}

// --- window stack: navigation + rendering ---

func (m *Model) topWindow() *window {
	if len(m.windows) == 0 {
		return nil
	}
	return m.windows[len(m.windows)-1]
}

// content builds the window's full lines with section gaps applied. All
// window navigation/render goes through this so cursor indices stay consistent.
func (w *window) content(m *Model) ([]string, []viewLine) {
	// Each block (table/jobs/verbatim/props) spaces itself with a leading blank
	// in full mode, so no extra layout pass is needed.
	return w.view.build(m, true)
}

func (m *Model) pushView(v *objView) {
	if v == nil {
		return
	}
	w := &window{view: v}
	_, lines := w.content(m)
	w.cur = firstSelectable(lines)
	m.windows = append(m.windows, w)
}

func (m *Model) popWindow() {
	if n := len(m.windows); n > 0 {
		m.windows = m.windows[:n-1]
	}
}

func (m *Model) windowBodyH() int {
	h := m.height - 7
	if h > 23 {
		h = 23
	}
	if h < 4 {
		h = 4
	}
	return h
}

func (w *window) clampOff(n, bodyH int) {
	maxOff := n - bodyH
	if maxOff < 0 {
		maxOff = 0
	}
	if w.off > maxOff {
		w.off = maxOff
	}
	if w.off < 0 {
		w.off = 0
	}
}

func (w *window) move(m *Model, dir int) {
	_, lines := w.content(m)
	sel := detSelectable(lines)
	bodyH := m.windowBodyH()
	w.cur, w.off = scrollMove(lines, sel, w.cur, w.off, dir, bodyH)
	w.clampOff(len(lines), bodyH)
}

func (w *window) page(m *Model, dir int) {
	_, lines := w.content(m)
	bodyH := m.windowBodyH()
	w.off += dir * bodyH
	w.clampOff(len(lines), bodyH)
	// Land on the page's leading selectable in the travel direction: the FIRST
	// visible row for PgUp, the LAST for PgDown (-dir flips selInView's
	// single-step convention, which grabs the trailing row instead).
	if v := selInView(detSelectable(lines), w.off, bodyH, -dir); v >= 0 {
		w.cur = v
	}
}

// windowView renders the top window as a centered dialog box composited over
// the base view by the caller. Section lines become centered cyan headers;
// selectable rows under the cursor get a white-bg highlight.
func (m *Model) windowView() string {
	w := m.topWindow()
	segs, lines := w.content(m)
	title := ""
	if len(segs) > 0 {
		title = segs[0]
	}
	bodyH := m.windowBodyH()
	w.clampOff(len(lines), bodyH)

	maxW := 0
	for _, l := range lines {
		if l.section {
			continue
		}
		if x := lipgloss.Width(l.text); x > maxW {
			maxW = x
		}
	}
	capW := m.width * 9 / 10
	if capW < 20 {
		capW = 20
	}
	if maxW > capW {
		maxW = capW
	}
	if maxW < 20 {
		maxW = 20
	}
	if w.xoff > maxW-10 {
		w.xoff = max(maxW-10, 0)
	}
	if w.xoff < 0 {
		w.xoff = 0
	}

	var b strings.Builder
	end := w.off + bodyH
	if end > len(lines) {
		end = len(lines)
	}
	for i := w.off; i < end; i++ {
		l := lines[i]
		if l.section {
			inner := " " + l.text + " "
			dashTotal := maxW - lipgloss.Width(inner)
			if dashTotal < 4 {
				dashTotal = 4
			}
			left := dashTotal / 2
			right := dashTotal - left
			b.WriteString(pad(styleCyan.Render(strings.Repeat("─", left)+inner+strings.Repeat("─", right)), maxW) + "\n")
			continue
		}
		txt := l.text
		if w.xoff > 0 {
			r := []rune(txt)
			if w.xoff < len(r) {
				txt = "…" + string(r[w.xoff:])
			} else {
				txt = ""
			}
		}
		if l.open != nil && i == w.cur {
			st := lipgloss.NewStyle().Background(lipgloss.Color("7")).Foreground(lipgloss.Color("0"))
			b.WriteString(st.Render(pad(txt, maxW)) + "\n")
		} else {
			b.WriteString(l.style.Render(pad(txt, maxW)) + "\n")
		}
	}
	for i := end - w.off; i < bodyH; i++ {
		b.WriteString("\n")
	}
	content := strings.TrimRight(b.String(), "\n")
	return dialogBox(title, content)
}
