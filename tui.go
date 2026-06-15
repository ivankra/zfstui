// Package tui is the interactive interface: a scrollable groups × host:pool
// matrix with a details pane. Read-only in M3 — actions (snap/sync/prune)
// arrive with M4. See DESIGN.md "TUI".
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	styleHeader     = lipgloss.NewStyle().Bold(true).Underline(true)
	styleName       = lipgloss.NewStyle().Bold(true)
	styleNormal     = lipgloss.NewStyle()
	styleNewSnaps   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))   // green: ahead (snapshots peers lack)
	styleWritten    = lipgloss.NewStyle().Foreground(lipgloss.Color("226")) // bright yellow: modified (uncommitted writes)
	styleDiverged   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))   // red
	styleCyan       = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))   // cyan
	styleClone      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))   // dim
	styleDim        = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleCursor     = lipgloss.NewStyle().Reverse(true)
	stylePaneTitle  = lipgloss.NewStyle().Foreground(lipgloss.Color("7")) // plain white
	stylePaneActive = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("8"))
	rowGuideBG      = lipgloss.Color("238")
	styleRowGuide   = lipgloss.NewStyle().Background(rowGuideBG)
	styleSection    = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))  // bright white: details headers/high-prio
	styleDetRow     = lipgloss.NewStyle().Foreground(lipgloss.Color("248")) // mid gray: details data rows
	styleStatus     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleStatusMsg  = lipgloss.NewStyle().Foreground(lipgloss.Color("14")) // light cyan: status/action messages
	styleErr        = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// cellStyle: white = synced, gray = stale (missing the newest snapshot),
// green = ahead (sole holder of the newest snapshot), yellow = modified
// (holds it but has uncommitted writes); red/cyan/dim flag diverged/
// resumable/clone. Uncommitted writes alone never make a peer "stale".
func cellStyle(r *Replica) lipgloss.Style {
	switch r.State {
	case StateDiverged:
		return styleDiverged
	case StateResumable:
		return styleCyan
	case StateClone:
		return styleClone
	case StateBehind:
		return styleDetRow // stale: gray
	case StateModified:
		return styleWritten // yellow: uncommitted local writes
	case StateAhead:
		return styleNewSnaps // green: holds snapshots peers lack
	}
	return styleNormal // synced
}

// cell is one (group, column) position; usually 0 or 1 replicas, rarely more.
type cell struct {
	replicas []*Replica
}

func (c cell) text() string {
	parts := make([]string, 0, len(c.replicas))
	for _, r := range c.replicas {
		parts = append(parts, CellText(r))
	}
	return strings.Join(parts, " ")
}

type Model struct {
	st    *Store
	hosts []Host // the active inventory (resolved from cli specs / sqlite)

	data    *Data
	cols    []string             // "host:pool"
	rows    []*Group             // currently listed (hidden filtered unless showHidden)
	cells   map[int]map[int]cell // row -> col -> cell
	unreach map[string]bool      // hostname -> unreachable at last refresh
	hiddenN int

	curRow, curCol int // curCol 0 = name column
	rowOff, colOff int
	width, height  int

	focus  int // 0 = Datasets pane, 1 = Details pane
	detOff int // Details scroll offset
	detCur int // Details row cursor (selectable lines)

	showHidden      bool
	showHelp        bool
	refreshing      bool
	refreshingHosts map[string]bool // host names currently being refreshed
	status          string
	statusErr       bool
	spin            int  // spinner frame counter
	ticking         bool // spinner/poll ticker active

	dryRun  bool
	dialog  *snapDialog // snapshot dialog when non-nil
	syncDlg *syncDialog // sync dialog when non-nil
	windows []*window   // stack of interactive object windows (Esc pops)
}

// snapDialog is the state of the snapshot dialog (TUI `s`): a modal form
// with a text input, a checkbox and buttons; Tab/↑↓ cycle focus. The Create
// button acts immediately (in -dry-run it opens the details modal instead).
type snapDialog struct {
	host      Host
	dataset   string
	input     textinput.Model
	recursive bool
	focus     int // formFocus* constants
	errMsg    string
}

const (
	formFocusName = iota
	formFocusRecursive
	formFocusOK
	formFocusCancel
	formFocusCount
)

func New(st *Store, hosts []Host, dryRun bool) (*Model, error) {
	m := &Model{st: st, hosts: hosts, dryRun: dryRun, refreshingHosts: map[string]bool{}}
	if err := m.reload(); err != nil {
		return nil, err
	}
	if len(m.data.States) == 0 {
		m.status = "cache is empty — press R to refresh"
	}
	return m, nil
}

// reload re-derives everything from the cache, preserving cursor best-effort.
func (m *Model) reload() error {
	data, err := loadData(m.st, hostNames(m.hosts))
	if err != nil {
		return err
	}
	m.data = data
	m.unreach = map[string]bool{}
	for _, h := range data.Hosts {
		if !h.Reachable {
			m.unreach[h.Name] = true
		}
	}

	// Header rows have negative curRow — there is no group there.
	var curName string
	if m.curRow >= 0 && m.curRow < len(m.rows) {
		curName = m.rows[m.curRow].DisplayName
	}

	colSet := map[string]bool{}
	m.rows = m.rows[:0]
	m.hiddenN = 0
	for _, g := range data.Groups {
		if g.Hidden {
			m.hiddenN++
			if !m.showHidden {
				continue
			}
		}
		m.rows = append(m.rows, g)
		for _, r := range g.Members {
			colSet[r.Dataset.Host+":"+r.Dataset.Pool()] = true
		}
	}
	m.cols = m.cols[:0]
	for c := range colSet {
		m.cols = append(m.cols, c)
	}
	sort.Strings(m.cols)
	colIdx := map[string]int{}
	for i, c := range m.cols {
		colIdx[c] = i
	}
	m.cells = map[int]map[int]cell{}
	for ri, g := range m.rows {
		m.cells[ri] = map[int]cell{}
		for _, r := range g.Members {
			ci := colIdx[r.Dataset.Host+":"+r.Dataset.Pool()]
			c := m.cells[ri][ci]
			c.replicas = append(c.replicas, r)
			m.cells[ri][ci] = c
		}
	}

	// Restore cursor onto the same group if possible; a header-row cursor
	// (negative) stays where it is.
	if m.curRow < 0 {
		m.clampCursor()
		return nil
	}
	m.curRow = 0
	for i, g := range m.rows {
		if g.DisplayName == curName {
			m.curRow = i
			break
		}
	}
	m.clampCursor()
	m.fixFocus()
	return nil
}

// fixFocus resets details-pane focus to main when there is nothing to show.
func (m *Model) fixFocus() {
	if m.focus == 1 {
		if segs, _ := m.detailsContent(); len(segs) == 0 {
			m.focus = 0
		}
	}
}

// clampCursor keeps the cursor in range. Row -1 is the pool header row,
// row -2 the host header row (data columns only — the name column has
// neither a pool nor a host).
func (m *Model) clampCursor() {
	if m.curRow >= len(m.rows) {
		m.curRow = len(m.rows) - 1
	}
	if m.curRow < -2 {
		m.curRow = -2
	}
	if m.curRow < 0 && len(m.cols) == 0 {
		m.curRow = 0
	}
	if m.curCol > len(m.cols) {
		m.curCol = len(m.cols)
	}
	if m.curCol < 0 {
		m.curCol = 0
	}
	if m.curRow < 0 && m.curCol == 0 {
		m.curCol = 1
	}
}

func (m *Model) hostOfCol(ci int) string {
	return strings.SplitN(m.cols[ci], ":", 2)[0]
}

func (m *Model) poolByCol(ci int) (*HostState, *Pool) {
	if ci < 0 || ci >= len(m.cols) {
		return nil, nil
	}
	parts := strings.SplitN(m.cols[ci], ":", 2)
	for i := range m.data.States {
		hs := &m.data.States[i]
		if hs.Host != parts[0] {
			continue
		}
		for j := range hs.Pools {
			if hs.Pools[j].Name == parts[1] {
				return hs, &hs.Pools[j]
			}
		}
	}
	return nil, nil
}

func poolProblem(hs *HostState, p *Pool) bool {
	if p == nil {
		return false
	}
	health := strings.ToUpper(p.Health)
	if health != "" && health != "ONLINE" {
		return true
	}
	status := p.Props["_zpool-status"]
	for _, ln := range strings.Split(status, "\n") {
		t := strings.TrimSpace(ln)
		if s, ok := strings.CutPrefix(t, "errors:"); ok {
			errs := strings.TrimSpace(s)
			if errs != "" && errs != "No known data errors" {
				return true
			}
			break
		}
	}
	if hs != nil {
		for _, v := range hs.VDevs {
			if v.Pool != p.Name {
				continue
			}
			if v.State != "" && v.State != "ONLINE" {
				return true
			}
			if v.RErr > 0 || v.WErr > 0 || v.CErr > 0 {
				return true
			}
		}
	}
	return false
}

// poolScrubbing reports whether the pool has an active scrub or resilver.
func poolScrubbing(p *Pool) bool {
	if p == nil {
		return false
	}
	for _, ln := range strings.Split(p.Props["_zpool-status"], "\n") {
		t := strings.TrimSpace(ln)
		if s, ok := strings.CutPrefix(t, "scan:"); ok {
			return strings.Contains(s, "in progress")
		}
	}
	return false
}

// beginRefresh marks the given hosts as refreshing, ensures the spinner ticker
// is running, and returns the combined refresh + tick command.
func (m *Model) beginRefresh(hosts []Host) tea.Cmd {
	m.refreshing = true
	for _, h := range hosts {
		m.refreshingHosts[h.Name] = true
	}
	cmds := []tea.Cmd{m.refreshCmd(hosts)}
	if tc := m.ensureTicker(); tc != nil {
		cmds = append(cmds, tc)
	}
	return tea.Batch(cmds...)
}

// hostSpan returns the [start, end) column range sharing ci's host.
func (m *Model) hostSpan(ci int) (int, int) {
	host := m.hostOfCol(ci)
	start, end := ci, ci+1
	for start > 0 && m.hostOfCol(start-1) == host {
		start--
	}
	for end < len(m.cols) && m.hostOfCol(end) == host {
		end++
	}
	return start, end
}

// moveHost moves the cursor to the adjacent host on the host header row.
func (m *Model) moveHost(dir int) {
	if m.curCol < 1 || m.curCol > len(m.cols) {
		return
	}
	start, end := m.hostSpan(m.curCol - 1)
	if dir < 0 && start > 0 {
		s, _ := m.hostSpan(start - 1)
		m.curCol = s + 1
	} else if dir > 0 && end < len(m.cols) {
		m.curCol = end + 1
	}
}

// selected returns the group and (if a data column is selected) the cell.
func (m *Model) selected() (*Group, *cell) {
	if m.curRow < 0 || m.curRow >= len(m.rows) {
		return nil, nil
	}
	g := m.rows[m.curRow]
	if m.curCol == 0 {
		return g, nil
	}
	c, ok := m.cells[m.curRow][m.curCol-1]
	if !ok {
		return g, nil
	}
	return g, &c
}

// --- messages ---

type refreshDoneMsg struct {
	results []RefreshResult
	err     error
}

func (m *Model) refreshCmd(hosts []Host) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		results := Refresh(ctx, newSSH(), m.st, hosts)
		return refreshDoneMsg{results: results}
	}
}

func (m *Model) Init() tea.Cmd { return m.ensureTicker() }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		return m.onTick()

	case jobsPollMsg:
		return m.onJobsPoll(msg)

	case refreshDoneMsg:
		m.refreshing = false
		for _, r := range msg.results {
			delete(m.refreshingHosts, r.Host)
		}
		ok, fail := 0, []string{}
		for _, r := range msg.results {
			if r.Err == nil {
				ok++
			} else {
				fail = append(fail, r.Host)
			}
		}
		if err := m.reload(); err != nil {
			m.status, m.statusErr = err.Error(), true
			return m, nil
		}
		if len(fail) > 0 {
			m.status = fmt.Sprintf("refreshed %d host(s); unreachable: %s", ok, strings.Join(fail, ", "))
			m.statusErr = true
		} else {
			m.status = fmt.Sprintf("refreshed %d host(s) at %s", ok, time.Now().Format("15:04:05"))
			m.statusErr = false
		}
		return m, m.ensureTicker()

	case snapDoneMsg:
		return m.onSnapDone(msg)

	case syncDoneMsg:
		return m.onSyncDone(msg)

	case tea.KeyMsg:
		if len(m.windows) > 0 {
			return m.updateWindow(msg)
		}
		if m.showHelp {
			m.showHelp = false
			return m, nil
		}
		if m.dialog != nil {
			return m.updateDialog(msg)
		}
		if m.syncDlg != nil {
			return m.updateSyncDialog(msg)
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "?":
			m.showHelp = true
		case "tab", "shift+tab":
			segs, _ := m.detailsContent()
			if len(segs) > 0 {
				m.focus = 1 - m.focus
				if m.focus == 1 {
					m.detSnap()
				}
			}
		case "up", "k":
			if m.focus == 1 {
				m.detMove(-1)
			} else {
				m.curRow--
				m.detOff, m.detCur = 0, 0
			}
		case "down", "j":
			if m.focus == 1 {
				m.detMove(1)
			} else {
				m.curRow++
				m.detOff, m.detCur = 0, 0
			}
		case "enter":
			if m.focus == 1 {
				m.detOpen()
				return m, nil
			}
			// Main table: Enter opens the detail window for the selection.
			switch {
			case m.curRow == -2 && m.curCol >= 1 && m.curCol <= len(m.cols):
				m.pushView(m.hostView(m.hostOfCol(m.curCol - 1)))
			case m.curRow == -1 && m.curCol >= 1 && m.curCol <= len(m.cols):
				parts := strings.SplitN(m.cols[m.curCol-1], ":", 2)
				m.pushView(m.poolView(parts[0], parts[1]))
			default:
				if _, c := m.selected(); c != nil && len(c.replicas) == 1 {
					m.pushView(m.datasetView(c.replicas[0].Dataset.Host, c.replicas[0].Dataset.Name))
				} else {
					// group row / multi-replica cell: drill into Details.
					m.focus = 1
					m.detSnap()
				}
			}
		case "left":
			if m.focus == 1 {
				break
			}
			if m.curRow == -2 {
				m.moveHost(-1)
			} else {
				m.curCol--
			}
			m.detOff, m.detCur = 0, 0
		case "right":
			if m.focus == 1 {
				break
			}
			if m.curRow == -2 {
				m.moveHost(1)
			} else {
				m.curCol++
			}
			m.detOff, m.detCur = 0, 0
		case "pgup":
			if m.focus == 1 {
				m.detOff -= detailsBodyH
				if m.detOff < 0 {
					m.detOff = 0
				}
				m.detCur = m.detOff
				m.detSnap()
			} else {
				m.curRow -= max(m.tableBodyH()-3, 1)
				m.detOff, m.detCur = 0, 0
			}
		case "pgdown":
			if m.focus == 1 {
				m.detOff += detailsBodyH
				m.detCur = m.detOff
				m.detSnap()
			} else {
				m.curRow += max(m.tableBodyH()-3, 1)
				m.detOff, m.detCur = 0, 0
			}
		case "h": // toggle hidden groups, not vim-left: arrows move horizontally
			m.showHidden = !m.showHidden
			if err := m.reload(); err != nil {
				m.status, m.statusErr = err.Error(), true
			}
		case "g", "home":
			if m.focus == 1 {
				m.detOff, m.detCur = 0, 0
				m.detSnap()
			} else {
				m.curRow = 0
			}
		case "G", "end":
			if m.focus == 1 {
				m.detCur = 1 << 30
				m.detSnap()
				m.detOff = m.detCur - detailsBodyH + 1
			} else {
				m.curRow = len(m.rows) - 1
			}
		case "s":
			m.openSnapshotDialog()
		case "y", "Y":
			m.openSyncDialog()
		case "r":
			return m.startRefresh(false)
		case "R":
			return m.startRefresh(true)
		}
		m.clampCursor()
		m.scrollToCursor()
		m.fixFocus()
	}
	return m, nil
}

// startRefresh kicks off discovery for the selected host (all=false) or all hosts.
func (m *Model) startRefresh(all bool) (tea.Model, tea.Cmd) {
	if m.refreshing {
		m.status = "refresh already running"
		return m, nil
	}
	hosts := m.hosts
	if !all && m.curCol > 0 && m.curCol <= len(m.cols) {
		hostName := strings.SplitN(m.cols[m.curCol-1], ":", 2)[0]
		for _, h := range m.hosts {
			if h.Name == hostName {
				hosts = []Host{h}
				break
			}
		}
	}
	names := make([]string, len(hosts))
	for i, h := range hosts {
		names[i] = h.Name
	}
	m.status, m.statusErr = "refreshing "+strings.Join(names, ", ")+"…", false
	return m, m.beginRefresh(hosts)
}

// --- view ---

const (
	maxColW      = 28
	detailsBodyH = 8 // detail lines visible (scrollable with Tab focus)
)

// tableBodyH is the table pane height including its three header lines
// (host, pool, fill-bar).
func (m *Model) tableBodyH() int {
	h := m.height - 1 - detailsBodyH - 1 // details title + footer
	if h < 5 {
		h = 5
	}
	return h
}

func (m *Model) View() string {
	if m.width == 0 {
		return "loading…"
	}
	if m.showHelp {
		return m.helpView()
	}

	var b strings.Builder
	b.WriteString(m.tableView(m.tableBodyH()))
	detSegs, _ := m.detailsContent()
	b.WriteString(m.paneTitle(detSegs, m.focus == 1))
	b.WriteString(m.detailsBody())
	b.WriteString(m.footerView())
	if m.dialog != nil {
		// Modal: composite the dialog box over the normal view.
		return overlayCenter(b.String(), m.dialogView(), m.width, m.height)
	}
	if m.syncDlg != nil {
		return overlayCenter(b.String(), m.syncDialogView(), m.width, m.height)
	}
	if len(m.windows) > 0 {
		return overlayCenter(b.String(), m.windowView(), m.width, m.height)
	}
	return b.String()
}

// updateWindow handles keys for the top interactive object window: a row
// cursor over selectable lines, Enter to drill into a sub-window (push), Esc
// to pop, plus vertical scroll and horizontal pan for wide content.
func (m *Model) updateWindow(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	w := m.topWindow()
	switch msg.String() {
	case "esc", "q":
		m.popWindow()
		return m, nil
	case "enter":
		_, lines := w.content(m)
		if w.cur >= 0 && w.cur < len(lines) && lines[w.cur].open != nil {
			m.pushView(lines[w.cur].open())
		} else {
			m.popWindow()
		}
		return m, nil
	case "up", "k":
		w.move(m, -1)
	case "down", "j":
		w.move(m, 1)
	case "pgup":
		w.page(m, -1)
	case "pgdown", " ":
		w.page(m, 1)
	case "left", "h":
		w.xoff -= 10
	case "right", "l":
		w.xoff += 10
	case "g", "home":
		w.off, w.xoff = 0, 0
		_, lines := w.content(m)
		if sel := detSelectable(lines); len(sel) > 0 {
			w.cur = sel[0]
		} else {
			w.cur = 0
		}
	case "G", "end":
		_, lines := w.content(m)
		if sel := detSelectable(lines); len(sel) > 0 {
			w.cur = sel[len(sel)-1]
		}
		w.off = len(lines)
		w.clampOff(len(lines), m.windowBodyH())
	}
	return m, nil
}

// overlayCenter splices the box into the middle of base, ANSI-aware, so the
// underlying view stays visible around a modal dialog.
func overlayCenter(base, box string, w, h int) string {
	boxLines := strings.Split(strings.TrimRight(box, "\n"), "\n")
	bw := 0
	for _, l := range boxLines {
		if x := ansi.StringWidth(l); x > bw {
			bw = x
		}
	}
	x := (w - bw) / 2
	if x < 0 {
		x = 0
	}
	y := (h - len(boxLines)) / 2
	if y < 0 {
		y = 0
	}
	baseLines := strings.Split(base, "\n")
	for len(baseLines) < y+len(boxLines) {
		baseLines = append(baseLines, "")
	}
	const reset = "\x1b[0m"
	for i, bl := range boxLines {
		orig := baseLines[y+i]
		left := ansi.Truncate(orig, x, "")
		pad := strings.Repeat(" ", max(x-ansi.StringWidth(left), 0))
		right := ansi.TruncateLeft(orig, x+bw, "")
		baseLines[y+i] = left + reset + pad + bl + reset + right
	}
	return strings.Join(baseLines, "\n")
}

// paneTitle renders segmented titles: ──[ seg ]──[ seg ]──…; the active
// pane's first segment is filled with the border color.
func (m *Model) paneTitle(segs []string, active bool) string {
	var b strings.Builder
	used := 0
	write := func(t string, st lipgloss.Style) {
		b.WriteString(st.Render(t))
		used += lipgloss.Width(t)
	}
	write("──", styleDim)
	for i, seg := range segs {
		st := stylePaneTitle
		if active && i == 0 {
			st = stylePaneActive
		}
		if active && i == 0 {
			write("  "+seg+"  ", st)
		} else {
			write("[", styleDim)
			write(" "+seg+" ", st)
			write("]", styleDim)
		}
		write("──", styleDim)
	}
	if fill := m.width - used; fill > 0 {
		b.WriteString(styleDim.Render(strings.Repeat("─", fill)))
	}
	return b.String() + "\n"
}

// cellTextLive is the table cell text plus a spinner when a job touching
// the replica is running (receiving or sending).
func (m *Model) cellTextLive(c cell) string {
	txt := c.text()
	for _, r := range c.replicas {
		if m.runningJobFor(r) != nil {
			txt += spinnerFrames[m.spin%len(spinnerFrames)]
			break
		}
	}
	return txt
}

func (m *Model) colWidths() (nameW, sizeW int, colWs []int) {
	// sizeW = max display width of the right-aligned size suffix (ASCII only).
	for _, g := range m.rows {
		if s := humanShort(groupSize(g)); len(s) > sizeW {
			sizeW = len(s)
		}
	}
	nameW = len("datasets")
	for _, g := range m.rows {
		needed := len(g.DisplayName) + 1 + sizeW
		if needed > nameW {
			nameW = needed
		}
	}
	if nameW < 8 {
		nameW = 8
	}
	if nameW > maxColW {
		nameW = maxColW
	}
	colWs = make([]int, len(m.cols))
	for i, c := range m.cols {
		poolLabel := strings.SplitN(c, ":", 2)[1] // pool name (host spans above)
		if _, p := m.poolByCol(i); poolScrubbing(p) {
			poolLabel += " 🧹"
		}
		w := lipgloss.Width(poolLabel)
		for ri := range m.rows {
			if cl, ok := m.cells[ri][i]; ok {
				if t := lipgloss.Width(m.cellTextLive(cl)); t > w {
					w = t
				}
			}
		}
		if w > maxColW {
			w = maxColW
		}
		colWs[i] = w
	}
	return nameW, sizeW, colWs
}

// visibleCols returns the column index range [colOff, end) that fits the width.
// Inter-group separators (" │ ") are 3 wide; intra-group ("  ") are 2 wide.
func (m *Model) visibleCols(nameW int, colWs []int) int {
	avail := m.width - nameW
	end := m.colOff
	for end < len(colWs) {
		// The separator before this column: 3 for first-in-group, 2 otherwise.
		sepW := 3
		if end > m.colOff && end > 0 && m.hostOfCol(end) == m.hostOfCol(end-1) {
			sepW = 1
		}
		if avail < colWs[end]+sepW {
			break
		}
		avail -= colWs[end] + sepW
		end++
	}
	if end == m.colOff && m.colOff < len(colWs) {
		end = m.colOff + 1 // always show at least one column
	}
	return end
}

func (m *Model) scrollToCursor() {
	// Vertical.
	tableRows := m.tableBodyH() - 3 // minus the three header lines
	if tableRows < 3 {
		tableRows = 3
	}
	if m.curRow < m.rowOff {
		m.rowOff = max(m.curRow, 0) // header row needs no scrolling
	}
	if m.curRow >= m.rowOff+tableRows {
		m.rowOff = m.curRow - tableRows + 1
	}
	// Horizontal: ensure cursor's data column visible.
	if m.curCol == 0 {
		m.colOff = 0
		return
	}
	ci := m.curCol - 1
	if ci < m.colOff {
		m.colOff = ci
	}
	nameW, _, colWs := m.colWidths()
	for m.visibleCols(nameW, colWs) <= ci && m.colOff < ci {
		m.colOff++
	}
}

// pad truncates/pads s to exactly w display cells (rune-safe).
func pad(s string, w int) string {
	if sw := lipgloss.Width(s); sw <= w {
		return s + strings.Repeat(" ", w-sw)
	}
	runes := []rune(s)
	out := make([]rune, 0, w)
	width := 0
	for _, r := range runes {
		if width+1 > w-1 {
			break
		}
		out = append(out, r)
		width++
	}
	return string(out) + "…"
}

// humanShort formats n with 2-3 significant digits: "1.5T", "37.2T", "239M".
// >= 100 of the unit: integer (XXX); otherwise one decimal (XX.X or X.X).
func humanShort(n int64) string {
	if n <= 0 {
		return ""
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	if v >= 100 || v == float64(int(v)) {
		return fmt.Sprintf("%d%c", int(v), "KMGTPE"[exp])
	}
	return fmt.Sprintf("%.1f%c", v, "KMGTPE"[exp])
}

// groupRefer returns the largest latest-snapshot Refer across group members.
func groupRefer(g *Group) int64 {
	var best int64
	for _, r := range g.Members {
		if r.Latest != nil && r.Latest.Refer > best {
			best = r.Latest.Refer
		}
	}
	return best
}

// groupSize returns the name-column size for a group: groupRefer (latest
// snapshot Refer) when snapshots exist; max Dataset.Used otherwise.
// Written is not used — it's bytes-since-last-snapshot and is meaningless
// when no snapshot has ever been taken.
func groupSize(g *Group) int64 {
	if r := groupRefer(g); r > 0 {
		return r
	}
	var best int64
	for _, m := range g.Members {
		if m.Dataset.Used > best {
			best = m.Dataset.Used
		}
	}
	return best
}

// centerStr centers s in a field of w display columns, padding with spaces.
func centerStr(s string, w int) string {
	sw := lipgloss.Width(s)
	if sw >= w {
		return pad(s, w)
	}
	left := (w - sw) / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", w-sw-left)
}

func (m *Model) tableView(h int) string {
	nameW, sizeW, colWs := m.colWidths()
	end := m.visibleCols(nameW, colWs)

	hostOf := func(col string) string { return strings.SplitN(col, ":", 2)[0] }
	poolOf := func(col string) string { return strings.SplitN(col, ":", 2)[1] }

	// Total pool size for the host (e.g. "1.8T"); the per-pool fill bars below
	// the pool headers carry the utilisation now, so no percentage here.
	hostInfo := func(host string) string {
		var size int64
		for i := range m.data.States {
			if m.data.States[i].Host != host {
				continue
			}
			for _, p := range m.data.States[i].Pools {
				size += p.Size
			}
		}
		if size == 0 {
			return ""
		}
		return humanShort(size)
	}

	// isColHL: the exact pool-header cell the cursor column sits under gets the
	// active-bg highlight. Only in the pool header row, only on data-row cursors.
	isColHL := func(ci int) bool {
		return m.focus == 0 && m.curRow >= 0 && m.curCol > 0 && ci == m.curCol-1
	}
	// isNameHL: same logic for the name column header ("group" / empty row).
	isNameHL := m.focus == 0 && m.curRow >= 0 && m.curCol == 0

	// isNewGroup: true when column ci starts a new host group (or is first visible).
	isNewGroup := func(ci int) bool {
		return ci == m.colOff || hostOf(m.cols[ci]) != hostOf(m.cols[ci-1])
	}

	// Separator strings. Inter-group: " │ " (3 cols). Intra-group: " " (1 col).
	dimPipe := styleDim.Render("│")
	dimPipeBG := styleDim.Background(rowGuideBG).Render("│")

	writeSep := func(b *strings.Builder, inter, onRow bool) {
		if inter {
			if onRow {
				b.WriteString(styleRowGuide.Render(" ") + dimPipeBG + styleRowGuide.Render(" "))
			} else {
				b.WriteString(" " + dimPipe + " ")
			}
		} else {
			if onRow {
				b.WriteString(styleRowGuide.Render(" "))
			} else {
				b.WriteString(" ")
			}
		}
	}

	var b strings.Builder

	// ── Host header row (curRow == -2) ──────────────────────────────────────
	groupHdr := centerStr("datasets", nameW)
	if isNameHL {
		b.WriteString(stylePaneActive.Render(groupHdr))
	} else {
		b.WriteString(styleName.Render(groupHdr))
	}
	for ci := m.colOff; ci < end; {
		host := hostOf(m.cols[ci])
		// Compute visible span width (intra-group gaps only, no leading sep).
		span := 0
		cj := ci
		for cj < end && hostOf(m.cols[cj]) == host {
			if cj > ci {
				span += 1 // intra-group gap
			}
			span += colWs[cj]
			cj++
		}
		b.WriteString(" " + dimPipe + " ") // inter-group separator
		label := host
		if info := hostInfo(host); info != "" &&
			lipgloss.Width(host)+1+lipgloss.Width(info) <= span {
			label = host + " " + info
		}
		if m.refreshingHosts[host] {
			sp := " " + spinnerFrames[m.spin%len(spinnerFrames)]
			if lipgloss.Width(label)+lipgloss.Width(sp) <= span {
				label += sp
			}
		}
		hdr := centerStr(label, span)
		switch {
		case m.curRow == -2 && m.curCol-1 >= ci && m.curCol-1 < cj:
			b.WriteString(styleCursor.Render(hdr))
		case m.unreach[host]:
			b.WriteString(styleDim.Render(hdr))
		default:
			b.WriteString(styleName.Render(hdr)) // bold, no underline
		}
		ci = cj
	}
	b.WriteString("\n")

	// ── Pool header row (curRow == -1) ──────────────────────────────────────
	b.WriteString(pad("", nameW))
	for ci := m.colOff; ci < end; ci++ {
		writeSep(&b, isNewGroup(ci), false)
		poolLabel := poolOf(m.cols[ci])
		if _, p := m.poolByCol(ci); poolScrubbing(p) {
			poolLabel += " 🧹"
		}
		hdr := pad(poolLabel, colWs[ci])
		switch {
		case m.curRow == -1 && ci == m.curCol-1:
			b.WriteString(styleCursor.Render(hdr))
		case isColHL(ci):
			b.WriteString(stylePaneActive.Render(hdr))
		case m.unreach[hostOf(m.cols[ci])]:
			b.WriteString(styleDim.Render(hdr))
		case poolProblem(m.poolByCol(ci)):
			b.WriteString(styleErr.Render(hdr))
		default:
			b.WriteString(styleName.Render(hdr))
		}
	}
	if m.colOff > 0 {
		b.WriteString(" ◀")
	}
	if end < len(m.cols) {
		b.WriteString(" ▶")
	}
	b.WriteString("\n")

	// ── Pool fill-bar row (visual only, not focusable) ───────────────────────
	b.WriteString(pad("", nameW))
	for ci := m.colOff; ci < end; ci++ {
		writeSep(&b, isNewGroup(ci), false)
		bar := ""
		if _, p := m.poolByCol(ci); p != nil && p.Size > 0 {
			bar = fillBar(p.Alloc*100/p.Size, colWs[ci])
		}
		b.WriteString(styleDim.Render(pad(bar, colWs[ci])))
	}
	b.WriteString("\n")

	// ── Data rows ────────────────────────────────────────────────────────────
	rowsShown := 0
	for ri := m.rowOff; ri < len(m.rows) && rowsShown < h-3; ri++ {
		g := m.rows[ri]
		onRow := ri == m.curRow

		// Name cell: name left-aligned, size right-aligned within nameW.
		sizeStr := humanShort(groupSize(g))
		namePart := pad(g.DisplayName, nameW-sizeW)
		sizePart := fmt.Sprintf("%*s", sizeW, sizeStr) // right-align ASCII size
		name := namePart + sizePart

		nstyle := styleName
		if g.Hidden {
			nstyle = styleDim
		}
		if onRow && m.curCol == 0 {
			b.WriteString(styleCursor.Render(name))
		} else {
			if onRow {
				nstyle = nstyle.Background(rowGuideBG)
			}
			b.WriteString(nstyle.Render(name))
		}

		for ci := m.colOff; ci < end; ci++ {
			writeSep(&b, isNewGroup(ci), onRow)
			c, ok := m.cells[ri][ci]
			txt := ""
			st := styleNormal
			if ok {
				txt = m.cellTextLive(c)
				if len(c.replicas) == 1 {
					st = cellStyle(c.replicas[0])
				}
			}
			if m.unreach[strings.SplitN(m.cols[ci], ":", 2)[0]] {
				st = styleDim
			}
			if onRow {
				st = st.Background(rowGuideBG)
			}
			cellStr := pad(txt, colWs[ci])
			if ri == m.curRow && ci == m.curCol-1 {
				b.WriteString(styleCursor.Render(cellStr))
			} else {
				b.WriteString(st.Render(cellStr))
			}
		}
		b.WriteString("\n")
		rowsShown++
	}
	for ; rowsShown < h-3; rowsShown++ {
		b.WriteString("\n")
	}
	return b.String()
}

// fillBar renders a pct-full progress bar of w cells: ▰ filled, ▱ empty.
func fillBar(pct int64, w int) string {
	if w <= 0 {
		return ""
	}
	filled := int((pct*int64(w) + 50) / 100) // round to nearest cell
	if filled > w {
		filled = w
	}
	if filled < 0 {
		filled = 0
	}
	return strings.Repeat("▰", filled) + strings.Repeat("▱", w-filled)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func replicaBrief(r *Replica) string {
	if r.Latest == nil {
		return "no snapshots"
	}
	s := "@" + r.Latest.Name
	if r.Latest.Refer > 0 {
		s += " (" + humanShort(r.Latest.Refer) + ")"
	}
	if r.Written > 0 {
		s += " +" + humanShort(r.Written) + " written"
	}
	return s
}

func snapList(snaps []Snapshot, n int) string {
	if len(snaps) == 0 {
		return "(none)"
	}
	start := len(snaps) - n
	if start < 0 {
		start = 0
	}
	parts := []string{}
	for i := len(snaps) - 1; i >= start; i-- {
		parts = append(parts, snaps[i].Name)
	}
	if start > 0 {
		parts = append(parts, fmt.Sprintf("(+%d older)", start))
	}
	return strings.Join(parts, "  ")
}

func (m *Model) footerView() string {
	keys := "q quit · ? help · ←↓↑→ move · Tab focus · s snap · y sync · r/R refresh · h hidden"
	if m.dryRun {
		keys = styleDiverged.Render("DRY-RUN") + " · " + keys
	}
	if m.hiddenN > 0 {
		mark := "+"
		if m.showHidden {
			mark = "-"
		}
		keys += fmt.Sprintf("(%s%d)", mark, m.hiddenN)
	}
	statusStr := m.status
	if m.refreshing {
		statusStr = "⟳ " + statusStr
	}
	gap := m.width - lipgloss.Width(keys) - lipgloss.Width(statusStr) - 1
	if gap < 1 {
		gap = 1
	}
	var line string
	if m.statusErr {
		line = styleErr.Render(statusStr)
	} else {
		line = styleStatusMsg.Render(statusStr)
	}
	line += strings.Repeat(" ", gap) + styleStatus.Render(keys)
	return line
}

func (m *Model) helpView() string {
	help := `zfstui — keys

  ←↓↑→ / jk     move cursor (name column selects the whole group; ↑ past the
                 first row selects a pool, ↑ again a host — details follow)
  Tab            switch focus to Details: ↑↓/jk move between rows (snapshots,
                 jobs, pools, datasets, replicas); Enter opens a modal with
                 everything known about the selected object
  s              snapshot the selected dataset (dialog: name, recursive)
  y              sync: incrementally send the selected dataset to other
                 replicas in its group (skips up-to-date and incompatible)
  g / G          first / last row
  r              refresh the selected cell's host (all hosts if on name column)
  R              refresh all hosts
  h              toggle showing hidden groups (currently ` + fmt.Sprint(m.hiddenN) + ` hidden:
                 groups with no snapshots anywhere, usually system/plumbing datasets)
  q              quit
  ?              close this help

cell markers:  ! diverged  ~ resumable  % clone
states/colors: white synced · green ahead (sole holder of newest snapshot) ·
               yellow modified (holds newest snapshot, has uncommitted writes) ·
               gray stale (missing newest snapshot) · red diverged ·
               cyan resumable · dim clone
column dim = host unreachable at last refresh

(prune actions arrive in the next milestone)

press any key to close`
	return help
}

// hostNames extracts the identity names of the active hosts.
func hostNames(hosts []Host) []string {
	names := make([]string, len(hosts))
	for i, h := range hosts {
		names[i] = h.Name
	}
	return names
}

// Run starts the TUI.
func runTUI(st *Store, hosts []Host, dryRun bool) error {
	m, err := New(st, hosts, dryRun)
	if err != nil {
		return err
	}
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

// --- snapshot dialog ---

// openSnapshotDialog picks the snapshot target from the cursor: a specific
// cell's replica, or — on the name column — the group's sole ahead replica.
func (m *Model) openSnapshotDialog() {
	g, c := m.selected()
	if m.curRow < 0 || g == nil {
		m.status, m.statusErr = "select a dataset to snapshot", true
		return
	}
	var r *Replica
	switch {
	case c != nil && len(c.replicas) > 0:
		r = c.replicas[0]
	case m.curCol == 0:
		var ahead []*Replica
		for _, mem := range g.Members {
			if mem.State == StateAhead {
				ahead = append(ahead, mem)
			}
		}
		if len(ahead) != 1 {
			m.status, m.statusErr = "no single ahead replica — select a specific cell to snapshot", true
			return
		}
		r = ahead[0]
	default:
		m.status, m.statusErr = "empty cell — nothing to snapshot here", true
		return
	}
	var host Host
	found := false
	for _, h := range m.hosts {
		if h.Name == r.Dataset.Host {
			host, found = h, true
			break
		}
	}
	if !found {
		host = Host{Name: r.Dataset.Host}
	}
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 64
	in.Width = 28
	in.TextStyle = lipgloss.NewStyle().Underline(true)
	in.SetValue(time.Now().Format("20060102"))
	in.CursorEnd()
	in.Focus()
	m.dialog = &snapDialog{host: host, dataset: r.Dataset.Name, input: in}
}

func (m *Model) updateDialog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	d := m.dialog
	switch msg.String() {
	case "esc", "ctrl+c":
		m.dialog = nil
		return m, nil
	case "tab", "down":
		d.setFocus((d.focus + 1) % formFocusCount)
		return m, nil
	case "shift+tab", "up":
		d.setFocus((d.focus + formFocusCount - 1) % formFocusCount)
		return m, nil
	case "left":
		if d.focus == formFocusCancel {
			d.setFocus(formFocusOK)
			return m, nil
		}
	case "right":
		if d.focus == formFocusOK {
			d.setFocus(formFocusCancel)
			return m, nil
		}
	case " ":
		switch d.focus {
		case formFocusRecursive:
			d.recursive = !d.recursive
			return m, nil
		case formFocusOK:
			return m, m.submitDialog()
		case formFocusCancel:
			m.dialog = nil
			return m, nil
		}
	case "enter":
		if d.focus == formFocusCancel {
			m.dialog = nil
			return m, nil
		}
		return m, m.submitDialog() // Enter anywhere else = default action
	}
	if d.focus == formFocusName {
		var cmd tea.Cmd
		d.input, cmd = d.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (d *snapDialog) setFocus(f int) {
	d.focus = f
	if f == formFocusName {
		d.input.Focus()
	} else {
		d.input.Blur()
	}
}

// submitDialog validates the form and acts: in -dry-run it opens the
// details modal; otherwise it launches the snapshot job. The form itself is
// the confirmation — the exact command is shown before pressing Create.
func (m *Model) submitDialog() tea.Cmd {
	d := m.dialog
	name := strings.TrimSpace(d.input.Value())
	for _, sn := range m.data.SnapsByReplica[d.host.Name+":"+d.dataset] {
		if sn.Name == name {
			d.errMsg = fmt.Sprintf("snapshot @%s already exists on this replica", name)
			return nil
		}
	}
	spec, err := snapshotJob(d.host, d.dataset, name, d.recursive)
	if err != nil {
		d.errMsg = err.Error()
		return nil
	}
	m.dialog = nil
	if m.dryRun {
		m.pushView(dryRunView(spec))
		m.status, m.statusErr = "[dry-run] nothing was executed", false
		return nil
	}
	m.refreshing = true
	m.status, m.statusErr = fmt.Sprintf("creating %s@%s on %s (job %s)…",
		d.dataset, name, d.host.Name, spec.ID), false
	return runSnapshotCmd(d.host, spec)
}

// --- dialog rendering ---

var (
	styleDialogBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("6")).
			Padding(1, 2)
	styleDialogBorder = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	styleDialogTitle  = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("6"))
	styleFieldLabel   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleButton       = lipgloss.NewStyle().Padding(0, 1).Foreground(lipgloss.Color("7"))
	styleButtonFocus  = lipgloss.NewStyle().Padding(0, 1).Reverse(true).Bold(true)
	styleFocusMark    = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
)

// dialogBox renders content in a rounded border with the title in the top
// border line, window-style: ╭─ Title ────╮.
func dialogBox(title, content string) string {
	box := styleDialogBox.Render(content)
	lines := strings.Split(box, "\n")
	if title == "" || len(lines) == 0 {
		return box
	}
	w := lipgloss.Width(lines[0])
	maxTitle := w - 6
	if lipgloss.Width(title) > maxTitle && maxTitle > 1 {
		title = pad(title, maxTitle)
	}
	fill := w - 5 - lipgloss.Width(title)
	if fill < 0 {
		fill = 0
	}
	lines[0] = styleDialogBorder.Render("╭─") + styleDialogTitle.Render(" "+title+" ") +
		styleDialogBorder.Render(strings.Repeat("─", fill)+"╮")
	return strings.Join(lines, "\n")
}

func button(label string, focused bool) string {
	if focused {
		return styleButtonFocus.Render("▶ " + label + " ◀")
	}
	return styleButton.Render("  " + label + "  ")
}

func checkbox(label string, checked, focused bool) string {
	box := "[ ]"
	if checked {
		box = "[x]"
	}
	s := box + " " + label
	if focused {
		return styleFocusMark.Render("› ") + styleCursor.Render(s)
	}
	return "  " + s
}

func (m *Model) dialogView() string {
	d := m.dialog
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", styleFieldLabel.Render("host    "), d.host.Name+"  ("+d.host.URL()+")")
	fmt.Fprintf(&b, "%s %s\n\n", styleFieldLabel.Render("dataset "), d.dataset)
	mark := "  "
	if d.focus == formFocusName {
		mark = styleFocusMark.Render("› ")
	}
	fmt.Fprintf(&b, "%s%s %s\n\n", mark, styleFieldLabel.Render("name"), d.input.View())
	b.WriteString(checkbox("recursive (-r)", d.recursive, d.focus == formFocusRecursive) + "\n")
	if cmd, err := buildSnapshotCmd(d.host.NeedSudo(), d.dataset,
		strings.TrimSpace(d.input.Value()), d.recursive); err == nil {
		b.WriteString("\n" + styleFieldLabel.Render("will run ") + styleName.Render(cmd) + "\n")
	}
	if d.errMsg != "" {
		b.WriteString("\n" + styleErr.Render("⚠ "+d.errMsg) + "\n")
	}
	create := "Create"
	if m.dryRun {
		create = "Create (dry run)"
	}
	b.WriteString("\n" + button(create, d.focus == formFocusOK) + "   " +
		button("Cancel", d.focus == formFocusCancel) + "\n")
	b.WriteString("\n" + styleDim.Render("Tab/↑↓ move · Space toggle · Enter ok · Esc cancel"))
	return dialogBox("Create snapshot", b.String())
}

func dryRunView(spec jobSpec) *objView {
	lines := []string{
		"on host " + spec.Host + ", zfstui would launch a detached job:",
		"",
		"  job dir:  " + spec.Dir(),
		"  command:  " + spec.Command,
		"",
		"mechanics: job.sh is piped over ssh and started with setsid,",
		"writing meta.json, status and log into the job dir; the TUI then",
		"polls status and refreshes the host. Full job.sh:",
		"",
	}
	for _, l := range strings.Split(strings.TrimRight(spec.script(), "\n"), "\n") {
		lines = append(lines, "  "+l)
	}
	return textView("DRY RUN — nothing was executed", lines)
}

// --- snapshot execution ---

type snapDoneMsg struct {
	spec    jobSpec
	host    Host
	state   string // done | failed | running (still going after our wait)
	logTail string
	err     error // launch/transport error
}

func runSnapshotCmd(h Host, spec jobSpec) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		r := newSSH()
		if err := launchJob(ctx, r, h, spec); err != nil {
			return snapDoneMsg{spec: spec, host: h, err: err}
		}
		state, logTail, err := waitJob(ctx, r, h, spec.ID, 20*time.Second)
		return snapDoneMsg{spec: spec, host: h, state: state, logTail: logTail, err: err}
	}
}

func (m *Model) onSnapDone(msg snapDoneMsg) (tea.Model, tea.Cmd) {
	m.refreshing = false
	switch {
	case msg.err != nil:
		m.status, m.statusErr = "snapshot job: "+msg.err.Error(), true
	case msg.state == "done":
		m.status, m.statusErr = fmt.Sprintf("snapshot created (job %s) — refreshing %s…",
			msg.spec.ID, msg.host.Name), false
		return m, m.beginRefresh([]Host{msg.host})
	case msg.state == "failed":
		m.status, m.statusErr = "snapshot FAILED", true
		lines := []string{
			"job:      " + msg.spec.ID + " on " + msg.host.Name,
			"command:  " + msg.spec.Command,
			"log tail:",
			"",
		}
		for _, l := range strings.Split(strings.TrimRight(msg.logTail, "\n"), "\n") {
			lines = append(lines, "  "+l)
		}
		m.pushView(textView("snapshot job failed", lines))
	default: // still running
		m.status, m.statusErr = fmt.Sprintf("job %s still running on %s — its result will show on next refresh",
			msg.spec.ID, msg.host.Name), false
	}
	return m, nil
}

// --- sync dialog (Y) ---

// syncDialog drives incremental replication from a source replica to one or
// more group members. Destinations that need syncing are pre-checked;
// up-to-date and incompatible ones are listed disabled, with the reason.
type syncDialog struct {
	src          *Replica
	srcHost      Host
	plans        []syncPlan
	selectable   []int // indices into plans that can be (de)selected
	checked      map[int]bool
	custom       textinput.Model // free-form extra destinations (full send)
	intermediate bool            // -I (default) vs -i
	focus        int             // 0..len(selectable)-1, custom, intermediate, OK, Cancel
	errMsg       string
}

func (d *syncDialog) focusCount() int { return len(d.selectable) + 4 }

func (d *syncDialog) setFocus(f int) {
	d.focus = f
	if f == len(d.selectable) { // the custom editbox
		d.custom.Focus()
	} else {
		d.custom.Blur()
	}
}

func (m *Model) hostByName(name string) Host {
	for _, h := range m.hosts {
		if h.Name == name {
			return h
		}
	}
	return Host{Name: name}
}

// openSyncDialog picks the sync source from the cursor (cell replica, or the
// group's sole ahead replica on the name column) and plans all destinations.
func (m *Model) openSyncDialog() {
	g, c := m.selected()
	if m.curRow < 0 || g == nil {
		m.status, m.statusErr = "select a dataset to sync from", true
		return
	}
	var src *Replica
	switch {
	case c != nil && len(c.replicas) > 0:
		src = c.replicas[0]
	case m.curCol == 0:
		var ahead []*Replica
		for _, mem := range g.Members {
			if mem.State == StateAhead {
				ahead = append(ahead, mem)
			}
		}
		if len(ahead) != 1 {
			m.status, m.statusErr = "no single ahead replica — select the source cell explicitly", true
			return
		}
		src = ahead[0]
	default:
		m.status, m.statusErr = "empty cell — nothing to sync from here", true
		return
	}
	plans, err := planSync(src, g, m.data.SnapsByReplica)
	if err != nil {
		m.status, m.statusErr = err.Error(), true
		return
	}
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 256
	in.Width = 44
	in.Placeholder = "host:pool/dataset, comma-separated"
	in.TextStyle = lipgloss.NewStyle().Underline(true)
	d := &syncDialog{
		src: src, srcHost: m.hostByName(src.Dataset.Host),
		plans: plans, checked: map[int]bool{}, custom: in, intermediate: true,
	}
	for i, p := range plans {
		if p.Selectable() {
			d.selectable = append(d.selectable, i)
			d.checked[i] = true // everything that needs syncing, on by default
		}
	}
	if len(d.selectable) == 0 {
		d.setFocus(len(d.selectable)) // nothing to check: focus the editbox
	}
	m.syncDlg = d
}

func (m *Model) updateSyncDialog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	d := m.syncDlg
	n := len(d.selectable)
	iCustom, iIntermediate, iOK, iCancel := n, n+1, n+2, n+3
	switch msg.String() {
	case "esc", "ctrl+c":
		m.syncDlg = nil
		return m, nil
	case "tab", "down":
		d.setFocus((d.focus + 1) % d.focusCount())
		return m, nil
	case "shift+tab", "up":
		d.setFocus((d.focus + d.focusCount() - 1) % d.focusCount())
		return m, nil
	case "enter":
		if d.focus == iCancel {
			m.syncDlg = nil
			return m, nil
		}
		return m, m.submitSyncDialog()
	}
	if d.focus == iCustom {
		// Everything else types into the editbox.
		var cmd tea.Cmd
		d.custom, cmd = d.custom.Update(msg)
		return m, cmd
	}
	switch msg.String() {
	case "j":
		d.setFocus((d.focus + 1) % d.focusCount())
	case "k":
		d.setFocus((d.focus + d.focusCount() - 1) % d.focusCount())
	case "left":
		if d.focus == iCancel {
			d.setFocus(iOK)
		}
	case "right":
		if d.focus == iOK {
			d.setFocus(iCancel)
		}
	case " ":
		switch {
		case d.focus < n:
			pi := d.selectable[d.focus]
			d.checked[pi] = !d.checked[pi]
		case d.focus == iIntermediate:
			d.intermediate = !d.intermediate
		case d.focus == iOK:
			return m, m.submitSyncDialog()
		case d.focus == iCancel:
			m.syncDlg = nil
		}
	}
	return m, nil
}

// datasetExists reports whether host:dataset is present in the cache.
func (m *Model) datasetExists(host, ds string) bool {
	for i := range m.data.States {
		if m.data.States[i].Host != host {
			continue
		}
		for _, d := range m.data.States[i].Datasets {
			if d.Name == ds {
				return true
			}
		}
	}
	return false
}

// submitSyncDialog builds one job per checked destination and launches them
// (or shows the dry-run details modal).
func (m *Model) submitSyncDialog() tea.Cmd {
	d := m.syncDlg
	type launch struct {
		spec  jobSpec
		label string
	}
	var launches []launch
	for _, pi := range d.selectable {
		if !d.checked[pi] {
			continue
		}
		p := d.plans[pi]
		destHost := m.hostByName(p.Dest.Dataset.Host)
		spec, err := syncJob(d.srcHost, destHost, d.src, p, d.intermediate)
		if err != nil {
			d.errMsg = err.Error()
			return nil
		}
		launches = append(launches, launch{spec: spec, label: p.DestLabel()})
	}
	// Custom destinations from the editbox: full sends to new datasets.
	for _, ent := range strings.Split(d.custom.Value(), ",") {
		ent = strings.TrimSpace(ent)
		if ent == "" {
			continue
		}
		hostName, ds, ok := strings.Cut(ent, ":")
		if !ok || hostName == "" || ds == "" {
			d.errMsg = fmt.Sprintf("custom destination %q: want host:pool/dataset", ent)
			return nil
		}
		if m.datasetExists(hostName, ds) {
			d.errMsg = fmt.Sprintf("%s already exists — full send only targets new datasets", ent)
			return nil
		}
		p, err := customDestPlan(d.src, m.data.SnapsByReplica, hostName, ds)
		if err != nil {
			d.errMsg = err.Error()
			return nil
		}
		spec, err := syncJob(d.srcHost, m.hostByName(hostName), d.src, p, d.intermediate)
		if err != nil {
			d.errMsg = err.Error()
			return nil
		}
		launches = append(launches, launch{spec: spec, label: ent + " (full send)"})
	}
	if len(launches) == 0 {
		d.errMsg = "no destinations selected"
		return nil
	}
	srcLabel := d.srcHost.Name + ":" + d.src.Dataset.Name
	if m.dryRun {
		lines := []string{"source: " + srcLabel, ""}
		for _, l := range launches {
			lines = append(lines,
				"→ "+l.label+"   (job dir "+l.spec.Dir()+" on "+d.srcHost.Name+", launched with ssh -A)",
				"")
			for _, cl := range strings.Split(strings.TrimRight(l.spec.Command, "\n"), "\n") {
				lines = append(lines, "    "+cl)
			}
			lines = append(lines, "")
		}
		lines = append(lines, "full job.sh of the first job:", "")
		for _, cl := range strings.Split(strings.TrimRight(launches[0].spec.script(), "\n"), "\n") {
			lines = append(lines, "  "+cl)
		}
		m.syncDlg = nil
		m.pushView(textView("DRY RUN — nothing was executed", lines))
		m.status, m.statusErr = "[dry-run] nothing was executed", false
		return nil
	}
	m.syncDlg = nil
	m.refreshing = true
	m.status, m.statusErr = fmt.Sprintf("syncing %s → %d destination(s)…", srcLabel, len(launches)), false
	srcHost := d.srcHost
	hostSet := map[string]Host{srcHost.Name: srcHost}
	specs := make([]jobSpec, len(launches))
	labels := make([]string, len(launches))
	for i, l := range launches {
		specs[i], labels[i] = l.spec, l.label
		dh := m.hostByName(strings.SplitN(l.label, ":", 2)[0])
		hostSet[dh.Name] = dh
	}
	var affected []Host
	for _, h := range hostSet {
		affected = append(affected, h)
	}
	return runSyncJobs(srcHost, specs, labels, affected)
}

type syncResult struct {
	label, id, state, logTail string
	command                   string
	err                       error
}

type syncDoneMsg struct {
	results  []syncResult
	affected []Host
}

// runSyncJobs launches all jobs (agent forwarded for src→dest auth), then
// polls briefly so fast failures surface immediately; long transfers keep
// running detached.
func runSyncJobs(srcHost Host, specs []jobSpec, labels []string, affected []Host) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		r := newSSH()
		results := make([]syncResult, len(specs))
		for i, spec := range specs {
			results[i] = syncResult{label: labels[i], id: spec.ID, state: "running", command: spec.Command}
			// -A forwards the agent; ControlPath=none forces a FRESH
			// connection — a reused ControlMaster would silently drop -A.
			if err := launchJob(ctx, r, srcHost, spec, "-A", "-o", "ControlPath=none"); err != nil {
				results[i].err = err
				results[i].state = "launch-failed"
			}
		}
		deadline := time.Now().Add(12 * time.Second)
		for i := range results {
			if results[i].state != "running" {
				continue
			}
			remain := time.Until(deadline)
			if remain < 2*time.Second {
				remain = 2 * time.Second
			}
			if st, logTail, err := waitJob(ctx, r, srcHost, results[i].id, remain); err == nil {
				results[i].state, results[i].logTail = st, logTail
			}
		}
		return syncDoneMsg{results: results, affected: affected}
	}
}

func (m *Model) onSyncDone(msg syncDoneMsg) (tea.Model, tea.Cmd) {
	m.refreshing = false
	done, running, failed := 0, 0, 0
	var failLines []string
	for _, r := range msg.results {
		switch r.state {
		case "done":
			done++
		case "running":
			running++
		default:
			failed++
			failLines = append(failLines, "✗ "+r.label+" (job "+r.id+")")
			if r.err != nil {
				failLines = append(failLines, "    "+r.err.Error())
			}
			failLines = append(failLines, "  command:")
			for _, cl := range strings.Split(strings.TrimRight(r.command, "\n"), "\n") {
				failLines = append(failLines, "    "+cl)
			}
			failLines = append(failLines, "  log:")
			for _, l := range strings.Split(strings.TrimRight(r.logTail, "\n"), "\n") {
				if l != "" {
					failLines = append(failLines, "    "+l)
				}
			}
			failLines = append(failLines, "")
		}
	}
	parts := []string{}
	if done > 0 {
		parts = append(parts, fmt.Sprintf("%d done", done))
	}
	if running > 0 {
		parts = append(parts, fmt.Sprintf("%d still running (will show on refresh)", running))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d FAILED", failed))
	}
	m.status = "sync: " + strings.Join(parts, ", ")
	m.statusErr = failed > 0
	if failed > 0 {
		m.pushView(textView("sync job failures", failLines))
	}
	if done > 0 || running > 0 {
		return m, m.beginRefresh(msg.affected)
	}
	return m, m.ensureTicker()
}

func (m *Model) syncDialogView() string {
	d := m.syncDlg
	var b strings.Builder
	srcSnaps := m.data.SnapsByReplica[d.src.Dataset.Host+":"+d.src.Dataset.Name]
	latest := ""
	if len(srcSnaps) > 0 {
		latest = "  @" + srcSnaps[len(srcSnaps)-1].Name
	}
	fmt.Fprintf(&b, "%s %s%s\n", styleFieldLabel.Render("source  "),
		d.srcHost.Name+":"+d.src.Dataset.Name, latest)
	if d.src.Dataset.Encrypted() {
		b.WriteString(styleFieldLabel.Render("        ") + " encrypted — raw send (-w)\n")
	}
	b.WriteString("\n" + styleFieldLabel.Render("destinations:") + "\n")
	sel := 0
	for i, p := range d.plans {
		if p.Selectable() {
			focused := d.focus < len(d.selectable) && d.selectable[d.focus] == i
			b.WriteString(checkbox(fmt.Sprintf("%-28s %s", p.DestLabel(), p.Reason),
				d.checked[i], focused) + "\n")
			sel++
		} else {
			reason := p.Reason
			st := styleDim
			if p.Kind == planIncompatible {
				st = styleDim.Foreground(lipgloss.Color("1"))
			}
			b.WriteString("      " + st.Render(fmt.Sprintf("%-28s %s", p.DestLabel(), reason)) + "\n")
		}
	}
	mark := "  "
	if d.focus == len(d.selectable) {
		mark = styleFocusMark.Render("› ")
	}
	b.WriteString("\n" + mark + styleFieldLabel.Render("new dest") + " " + d.custom.View() +
		styleDim.Render("  (full send)") + "\n")
	b.WriteString("\n" + checkbox("send intermediate snapshots (-I)", d.intermediate,
		d.focus == len(d.selectable)+1) + "\n")
	if d.errMsg != "" {
		b.WriteString("\n" + styleErr.Render("⚠ "+d.errMsg) + "\n")
	}
	sync := "Sync"
	if m.dryRun {
		sync = "Sync (dry run)"
	}
	b.WriteString("\n" + button(sync, d.focus == len(d.selectable)+2) + "   " +
		button("Cancel", d.focus == len(d.selectable)+3) + "\n")
	b.WriteString("\n" + styleDim.Render("Tab/↑↓ move · Space toggle · Enter ok · Esc cancel"))
	return dialogBox("Sync (incremental send)", b.String())
}

// --- live job progress (spinner + polling) ---

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type tickMsg time.Time

type jobsPollMsg struct {
	host string
	jobs []JobInfo
	err  error
}

func tickCmd() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// runningJobHosts returns hosts that have running jobs (poll targets).
func (m *Model) runningJobHosts() []string {
	seen := map[string]bool{}
	var hosts []string
	for i := range m.data.States {
		for _, j := range m.data.States[i].Jobs {
			if j.State == "running" && !seen[j.Host] {
				seen[j.Host] = true
				hosts = append(hosts, j.Host)
			}
		}
	}
	return hosts
}

// ensureTicker starts the spinner/poll ticker when there is something live
// (running jobs or an active refresh).
func (m *Model) ensureTicker() tea.Cmd {
	if m.ticking || (len(m.runningJobHosts()) == 0 && !m.refreshing) {
		return nil
	}
	m.ticking = true
	return tickCmd()
}

func (m *Model) onTick() (tea.Model, tea.Cmd) {
	hosts := m.runningJobHosts()
	if len(hosts) == 0 && !m.refreshing {
		m.ticking = false
		return m, nil
	}
	m.spin++
	cmds := []tea.Cmd{tickCmd()}
	if m.spin%20 == 0 { // every ~3s: poll job dirs on involved hosts
		for _, hn := range hosts {
			cmds = append(cmds, m.pollJobsCmd(m.hostByName(hn)))
		}
	}
	return m, tea.Batch(cmds...)
}

func (m *Model) pollJobsCmd(h Host) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out, err := newSSH().Run(ctx, h, jobsProbe)
		if err != nil {
			return jobsPollMsg{host: h.Name, err: err}
		}
		return jobsPollMsg{host: h.Name, jobs: ParseJobs(h.Name, string(out))}
	}
}

func (m *Model) onJobsPoll(msg jobsPollMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, nil // transient; next refresh will tell the truth
	}
	finished := map[string]bool{} // hosts affected by jobs that just ended
	for i := range m.data.States {
		hs := &m.data.States[i]
		if hs.Host != msg.host {
			continue
		}
		was := map[string]string{}
		for _, j := range hs.Jobs {
			was[j.ID] = j.State
		}
		for _, j := range msg.jobs {
			if was[j.ID] == "running" && j.State != "running" {
				for _, k := range jobReplicaKeys(j) {
					finished[strings.SplitN(k, ":", 2)[0]] = true
				}
				if j.State == "failed" {
					m.status, m.statusErr = fmt.Sprintf("job %s FAILED on %s — see details / job log", j.ID, j.Host), true
				}
			}
		}
		hs.Jobs = msg.jobs
	}
	m.data.JobsByReplica = indexJobs(m.data.States)
	if len(finished) > 0 {
		var hosts []Host
		for hn := range finished {
			hosts = append(hosts, m.hostByName(hn))
		}
		return m, m.beginRefresh(hosts)
	}
	return m, nil
}

// replicaKeyOf builds the jobs-index key for a replica.
func replicaKeyOf(r *Replica) string {
	return r.Dataset.Host + ":" + r.Dataset.Name
}

// runningJobFor returns a running job touching the replica, if any.
func (m *Model) runningJobFor(r *Replica) *JobInfo {
	for _, j := range m.data.JobsByReplica[replicaKeyOf(r)] {
		if j.State == "running" {
			return &j
		}
	}
	return nil
}

// jobStartTime parses the start timestamp from the status line "running <pid> <RFC3339>".
func jobStartTime(j *JobInfo) time.Time {
	if j.State != "running" {
		return time.Time{}
	}
	parts := strings.Fields(j.Status)
	if len(parts) >= 3 {
		if t, err := time.Parse(time.RFC3339, parts[2]); err == nil {
			return t
		}
	}
	return time.Time{}
}

// fmtUptime formats a long duration as "12d 3h", "3h 40m", or "40m".
func fmtUptime(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

// fmtDuration formats a duration down to the minute: "3h40m", "10m", "<1m".
// Seconds are omitted — ETA precision beyond a minute is false precision.
func fmtDuration(d time.Duration) string {
	m := int(d.Minutes())
	if m < 1 {
		return "<1m"
	}
	if h := m / 60; h > 0 {
		return fmt.Sprintf("%dh%dm", h, m%60)
	}
	return fmt.Sprintf("%dm", m)
}

// jobProgressLine renders progress, and for running jobs with elapsed time:
// speed and ETA. E.g. "1.2G / 3.4G (35%)  45.2M/s  ETA 0:48".
func jobProgressLine(j *JobInfo) string {
	var parts []string
	switch {
	case j.Progress > 0 && j.Expected > 0:
		pct := j.Progress * 100 / j.Expected
		if pct > 100 {
			pct = 100
		}
		parts = append(parts, fmt.Sprintf("%s/%s (%d%%)", humanShort(j.Progress), humanShort(j.Expected), pct))
	case j.Progress > 0:
		parts = append(parts, humanShort(j.Progress)+" transferred")
	case j.Expected > 0:
		parts = append(parts, "~"+humanShort(j.Expected)+" to transfer")
	default:
		return "progress unknown (pv not installed on source?)"
	}
	if j.Progress > 0 {
		start := jobStartTime(j)
		if !start.IsZero() {
			elapsed := time.Since(start)
			if elapsed >= 5*time.Second {
				speed := float64(j.Progress) / elapsed.Seconds()
				parts = append(parts, humanShort(int64(speed))+"/s")
				if j.Expected > 0 && j.Expected > j.Progress {
					eta := time.Duration(float64(j.Expected-j.Progress)/speed) * time.Second
					parts = append(parts, "ETA "+fmtDuration(eta))
				}
			}
		}
	}
	return strings.Join(parts, "  ")
}
