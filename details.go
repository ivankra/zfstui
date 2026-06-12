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

func plainLine(text string) detLine                { return detLine{text: text} }
func sectionLine(text string) detLine              { return detLine{text: text, style: styleSection} }
func styled(text string, s lipgloss.Style) detLine { return detLine{text: text, style: s} }

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
				text: fmt.Sprintf("  %-*s  %7s  %7s", nameW, s.Name, human(s.Used), human(s.Refer)),
				open: func(*Model) *textModal { return snapshotModal(s) },
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
			open: func(*Model) *textModal { return datasetModal(r.Dataset) },
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
		st := lipgloss.NewStyle()
		switch j.State {
		case "running":
			status = spinnerFrames[m.spin%len(spinnerFrames)] + " "
			if j.Expected > 0 && j.Progress > 0 {
				status += fmt.Sprintf("%d%%", min(j.Progress*100/j.Expected, 100))
			} else {
				status += "run"
			}
		case "done":
			status = "OK" // plain white
		case "failed":
			status, st = "FAIL", styleErr
		default:
			status, st = j.State, styleDim
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
	var pools []Pool
	var nDatasets, nSnaps int
	for i := range m.data.States {
		hs := &m.data.States[i]
		if hs.Host != host {
			continue
		}
		pools = hs.Pools
		nDatasets, nSnaps = len(hs.Datasets), len(hs.Snapshots)
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
	if len(pools) > 0 {
		nameW := 4
		for _, p := range pools {
			if len(p.Name) > nameW {
				nameW = len(p.Name)
			}
		}
		lines = append(lines, sectionLine(fmt.Sprintf("* %-*s  %6s %6s %6s  %5s %4s %6s  %s",
			nameW, "POOL", "SIZE", "ALLOC", "FREE", "FRAG", "CAP", "DEDUP", "HEALTH")))
		for _, p := range pools {
			p := p
			lines = append(lines, detLine{
				text: fmt.Sprintf("  %-*s  %6s %6s %6s  %4d%% %3d%% %6s  %s",
					nameW, p.Name, human(p.Size), human(p.Alloc), human(p.Free),
					p.Frag, p.Cap, p.Dedup, p.Health),
				open: func(*Model) *textModal { return poolModal(p) },
			})
		}
	}
	lines = append(lines, plainLine(fmt.Sprintf("%d dataset(s), %d snapshot(s) cached", nDatasets, nSnaps)))
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
	pp := *p
	lines := []detLine{{
		text: fmt.Sprintf("  %-9s frag %d%%  dedup %s  guid %s", p.Health, p.Frag, p.Dedup, orDash(p.GUID)),
		open: func(*Model) *textModal { return poolModal(pp) },
	}}
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
				open: func(*Model) *textModal { return datasetModal(&d) },
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

// hostModal: everything known about a host (access spec, refresh state,
// pools, cache counts).
func (m *Model) hostModal(host string) *textModal {
	var lines []string
	for _, h := range m.hosts {
		if h.Name != host {
			continue
		}
		lines = append(lines, "access    "+h.URL())
		lines = append(lines, "ssh user  "+h.EffUser())
		if h.NeedSudo() {
			lines = append(lines, "sudo      yes (implied by non-root user)")
		}
		if h.Port != 0 {
			lines = append(lines, fmt.Sprintf("port      %d", h.Port))
		}
		if h.SendUser != "" {
			lines = append(lines, "send_user "+h.SendUser)
		}
		if h.RecvUser != "" {
			lines = append(lines, "recv_user "+h.RecvUser)
		}
		if h.Pool != "" {
			lines = append(lines, "pool      "+h.Pool+" (from spec; not enforced yet)")
		}
	}
	for _, hi := range m.data.Hosts {
		if hi.Name != host {
			continue
		}
		when := hi.LastRefresh.Format("2006-01-02 15:04:05")
		if hi.Reachable {
			lines = append(lines, "refreshed "+when)
		} else {
			lines = append(lines, "UNREACHABLE at last refresh ("+when+"): "+hi.LastError)
		}
	}
	for i := range m.data.States {
		hs := &m.data.States[i]
		if hs.Host != host {
			continue
		}
		lines = append(lines, "")
		for _, p := range hs.Pools {
			pct := int64(0)
			if p.Size > 0 {
				pct = p.Alloc * 100 / p.Size
			}
			lines = append(lines, fmt.Sprintf("pool %-12s %-9s size %-8s alloc %s (%d%%) free %s",
				p.Name, p.Health, human(p.Size), human(p.Alloc), pct, human(p.Free)))
		}
		running := 0
		for _, j := range hs.Jobs {
			if j.State == "running" {
				running++
			}
		}
		lines = append(lines, fmt.Sprintf("%d dataset(s), %d snapshot(s) cached; %d job dir(s), %d running",
			len(hs.Datasets), len(hs.Snapshots), len(hs.Jobs), running))
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
	lines = append(lines, "", "properties ("+fmt.Sprint(len(props))+"):")
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	keyW := 0
	for _, k := range keys {
		if len(k) > keyW {
			keyW = len(k)
		}
	}
	for _, k := range keys {
		l := fmt.Sprintf("  %-*s  %s", keyW, k, props[k])
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
	return &textModal{title: "pool " + p.Host + ":" + p.Name, lines: propsDump(lines, p.Props, p.PropSrc)}
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
