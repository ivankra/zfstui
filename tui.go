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
	styleNewSnaps   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))   // green: unsynced snapshots
	styleWritten    = lipgloss.NewStyle().Foreground(lipgloss.Color("226")) // bright yellow: unsnapshotted writes (+)
	styleDiverged   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))   // red
	styleResume     = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))   // cyan
	styleClone      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))   // dim
	styleDim        = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleCursor     = lipgloss.NewStyle().Reverse(true)
	stylePaneTitle  = lipgloss.NewStyle().Foreground(lipgloss.Color("7")) // plain white
	stylePaneActive = lipgloss.NewStyle().Foreground(lipgloss.Color("7")).Underline(true)
	rowGuideBG      = lipgloss.Color("238")
	styleRowGuide   = lipgloss.NewStyle().Background(rowGuideBG)
	styleSection    = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))  // bright white: details headers/high-prio
	styleDetRow     = lipgloss.NewStyle().Foreground(lipgloss.Color("248")) // mid gray: details data rows
	styleStatus     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleStatusMsg  = lipgloss.NewStyle().Foreground(lipgloss.Color("14")) // light cyan: status/action messages
	styleErr        = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// cellStyle: white = normal (in sync or merely behind — the @names speak),
// green = has snapshots not yet synced elsewhere, orange = unsnapshotted
// local writes ("+"); red/cyan/dim keep flagging diverged/resumable/clone.
func cellStyle(r *Replica) lipgloss.Style {
	switch r.State {
	case StateDiverged:
		return styleDiverged
	case StateResumable:
		return styleResume
	case StateClone:
		return styleClone
	}
	if r.Written > 0 {
		return styleWritten
	}
	if r.State == StateAhead {
		return styleNewSnaps
	}
	return styleNormal
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

	showHidden bool
	showHelp   bool
	refreshing bool
	status     string
	statusErr  bool
	spin       int  // spinner frame counter
	ticking    bool // spinner/poll ticker active

	dryRun  bool
	dialog  *snapDialog // snapshot dialog when non-nil
	syncDlg *syncDialog // sync dialog when non-nil
	modal   *textModal  // scrollable centered text modal (dry-run details, job logs)
}

// textModal is a centered, scrollable read-only text box (vertical and
// horizontal — job pipelines are wide).
type textModal struct {
	title string
	lines []string
	off   int // vertical scroll
	xOff  int // horizontal pan (display columns)
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
	m := &Model{st: st, hosts: hosts, dryRun: dryRun}
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
	return nil
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
		if m.modal != nil {
			m.updateModal(msg)
			return m, nil
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
			m.focus = 1 - m.focus
			if m.focus == 1 {
				m.detSnap()
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
			// Main table: Enter opens the detail modal for the selection.
			switch {
			case m.curRow == -2 && m.curCol >= 1 && m.curCol <= len(m.cols):
				m.modal = m.hostModal(m.hostOfCol(m.curCol - 1))
			case m.curRow == -1 && m.curCol >= 1 && m.curCol <= len(m.cols):
				parts := strings.SplitN(m.cols[m.curCol-1], ":", 2)
				m.modal = m.poolModalFor(parts[0], parts[1])
			default:
				if _, c := m.selected(); c != nil && len(c.replicas) == 1 {
					m.modal = datasetModal(c.replicas[0].Dataset)
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
	m.refreshing = true
	names := make([]string, len(hosts))
	for i, h := range hosts {
		names[i] = h.Name
	}
	m.status, m.statusErr = "refreshing "+strings.Join(names, ", ")+"…", false
	return m, m.refreshCmd(hosts)
}

// --- view ---

const (
	maxColW      = 28
	detailsBodyH = 8 // detail lines visible (scrollable with Tab focus)
)

// tableBodyH is the table pane height including its two header lines.
func (m *Model) tableBodyH() int {
	h := m.height - 1 - 1 - detailsBodyH - 1 // two pane titles, footer
	if h < 4 {
		h = 4
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
	b.WriteString(m.paneTitle([]string{"Datasets"}, m.focus == 0))
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
	if m.modal != nil {
		return overlayCenter(b.String(), m.modalView(), m.width, m.height)
	}
	return b.String()
}

// --- scrollable text modal ---

func (m *Model) modalBodyH() int {
	h := m.height - 8
	if h > 24 {
		h = 24
	}
	if h < 4 {
		h = 4
	}
	return h
}

func (m *Model) updateModal(msg tea.KeyMsg) {
	mo := m.modal
	bodyH := m.modalBodyH()
	maxOff := len(mo.lines) - bodyH
	if maxOff < 0 {
		maxOff = 0
	}
	switch msg.String() {
	case "up", "k":
		mo.off--
	case "down", "j":
		mo.off++
	case "left", "h":
		mo.xOff -= 10
	case "right", "l":
		mo.xOff += 10
	case "pgup":
		mo.off -= bodyH
	case "pgdown", " ":
		mo.off += bodyH
	case "g", "home":
		mo.off, mo.xOff = 0, 0
	case "G", "end":
		mo.off = maxOff
	case "esc", "q", "enter":
		m.modal = nil
		return
	}
	if mo.off > maxOff {
		mo.off = maxOff
	}
	if mo.off < 0 {
		mo.off = 0
	}
	maxLine := 0
	for _, l := range mo.lines {
		if w := lipgloss.Width(l); w > maxLine {
			maxLine = w
		}
	}
	if mo.xOff > maxLine-10 {
		mo.xOff = max(maxLine-10, 0)
	}
	if mo.xOff < 0 {
		mo.xOff = 0
	}
}

func (m *Model) modalView() string {
	mo := m.modal
	bodyH := m.modalBodyH()
	maxW := m.width - 12
	if maxW > 96 {
		maxW = 96
	}
	if maxW < 20 {
		maxW = 20
	}
	var b strings.Builder
	end := mo.off + bodyH
	if end > len(mo.lines) {
		end = len(mo.lines)
	}
	for i := mo.off; i < end; i++ {
		ln := mo.lines[i]
		if mo.xOff > 0 {
			r := []rune(ln)
			if mo.xOff < len(r) {
				ln = "…" + string(r[mo.xOff:])
			} else {
				ln = ""
			}
		}
		b.WriteString(pad(ln, maxW) + "\n")
	}
	for i := end - mo.off; i < bodyH; i++ {
		b.WriteString("\n")
	}
	pos := ""
	if len(mo.lines) > bodyH {
		pos = fmt.Sprintf("  %d–%d/%d", mo.off+1, end, len(mo.lines))
	}
	if mo.xOff > 0 {
		pos += fmt.Sprintf("  →%d", mo.xOff)
	}
	b.WriteString("\n" + styleDim.Render("↑↓←→/hjkl scroll · PgUp/PgDn · Esc close"+pos))
	return dialogBox(mo.title, b.String())
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
// pane's first segment gets ▶ ◀ cursor markers.
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
		write("[", styleDim)
		write(" ", stylePaneTitle)
		write(seg, st)
		write(" ", stylePaneTitle)
		write("]", styleDim)
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
			txt += " " + spinnerFrames[m.spin%len(spinnerFrames)]
			break
		}
	}
	return txt
}

func (m *Model) colWidths() (nameW int, colWs []int) {
	nameW = 8
	for _, g := range m.rows {
		if len(g.DisplayName) > nameW {
			nameW = len(g.DisplayName)
		}
	}
	if nameW > maxColW {
		nameW = maxColW
	}
	colWs = make([]int, len(m.cols))
	for i, c := range m.cols {
		w := len(strings.SplitN(c, ":", 2)[1]) // pool name (host spans above)
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
	return nameW, colWs
}

// visibleCols returns the column index range [colOff, end) that fits the width.
func (m *Model) visibleCols(nameW int, colWs []int) int {
	avail := m.width - nameW - 2
	end := m.colOff
	for end < len(colWs) && avail >= colWs[end]+2 {
		avail -= colWs[end] + 2
		end++
	}
	if end == m.colOff && m.colOff < len(colWs) {
		end = m.colOff + 1 // always show at least one column
	}
	return end
}

func (m *Model) scrollToCursor() {
	// Vertical.
	tableRows := m.tableBodyH() - 2 // minus the two header lines
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
	nameW, colWs := m.colWidths()
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

func (m *Model) tableView(h int) string {
	nameW, colWs := m.colWidths()
	end := m.visibleCols(nameW, colWs)

	hostOf := func(col string) string { return strings.SplitN(col, ":", 2)[0] }
	poolOf := func(col string) string { return strings.SplitN(col, ":", 2)[1] }

	// "(37T/53%)" capacity label for a host header, shown only when it fits.
	hostInfo := func(host string) string {
		var size, alloc int64
		for i := range m.data.States {
			if m.data.States[i].Host != host {
				continue
			}
			for _, p := range m.data.States[i].Pools {
				size += p.Size
				alloc += p.Alloc
			}
		}
		if size == 0 {
			return ""
		}
		return fmt.Sprintf("(%d%%/%s)", alloc*100/size, human(size))
	}

	var b strings.Builder
	// Header line 1: hosts, spanning their (consecutive, sorted) pool
	// columns. Selectable: cursor row -2 highlights the whole span.
	b.WriteString(pad("", nameW))
	for ci := m.colOff; ci < end; {
		host := hostOf(m.cols[ci])
		span := 0
		cj := ci
		for cj < end && hostOf(m.cols[cj]) == host {
			span += colWs[cj] + 2
			cj++
		}
		b.WriteString("  ")
		label := host
		if info := hostInfo(host); info != "" &&
			lipgloss.Width(host)+1+lipgloss.Width(info) <= span-2 {
			label = host + " " + info
		}
		hdr := pad(label, span-2)
		switch {
		case m.curRow == -2 && m.curCol-1 >= ci && m.curCol-1 < cj:
			b.WriteString(styleCursor.Render(hdr))
		case m.unreach[host]:
			b.WriteString(styleDim.Render(hdr))
		default:
			b.WriteString(styleHeader.Render(hdr))
		}
		ci = cj
	}
	b.WriteString("\n")

	// Header line 2: pool names (selectable: cursor row -1). Bold only —
	// underline is the host row's.
	b.WriteString(pad("", nameW))
	for ci := m.colOff; ci < end; ci++ {
		b.WriteString("  ")
		hdr := pad(poolOf(m.cols[ci]), colWs[ci])
		switch {
		case m.curRow == -1 && ci == m.curCol-1:
			b.WriteString(styleCursor.Render(hdr))
		case m.unreach[hostOf(m.cols[ci])]:
			b.WriteString(styleDim.Render(hdr))
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

	rowsShown := 0
	for ri := m.rowOff; ri < len(m.rows) && rowsShown < h-2; ri++ {
		g := m.rows[ri]
		// Row guide: the cursor's row gets a background tint so the group's
		// replicas are easy to pick out wherever the cursor is.
		onRow := ri == m.curRow
		sep := "  "
		if onRow {
			sep = styleRowGuide.Render("  ")
		}
		name := pad(g.DisplayName, nameW)
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
			b.WriteString(sep)
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
	for ; rowsShown < h-2; rowsShown++ {
		b.WriteString("\n")
	}
	return b.String()
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTPE"[exp])
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
	if r.Written > 0 {
		s += fmt.Sprintf(" +%s written", human(r.Written))
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
	right := m.status
	if m.refreshing {
		right = "⟳ " + right
	}
	gap := m.width - lipgloss.Width(keys) - lipgloss.Width(right) - 1
	if gap < 1 {
		gap = 1
	}
	line := styleStatus.Render(keys) + strings.Repeat(" ", gap)
	if m.statusErr {
		line += styleErr.Render(right)
	} else {
		line += styleStatusMsg.Render(right)
	}
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

cell markers:  + unsnapshotted writes  ! diverged  ~ resumable  % clone
cell colors:   white normal · green unsynced snapshots · orange unsnapshotted
               writes (+) · red diverged · cyan resumable · dim clone
column dim = host unreachable at last refresh

(snapshot/sync/prune actions arrive in the next milestone)

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
		m.modal = dryRunModal(spec)
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
			Padding(1, 3)
	styleDialogBorder = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	styleDialogTitle  = lipgloss.NewStyle().Foreground(lipgloss.Color("7")) // plain white
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
	lines[0] = styleDialogBorder.Render("╭─ ") + styleDialogTitle.Render(title) +
		styleDialogBorder.Render(" "+strings.Repeat("─", fill)+"╮")
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

func dryRunModal(spec jobSpec) *textModal {
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
	return &textModal{title: "DRY RUN — nothing was executed", lines: lines}
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
		m.refreshing = true
		return m, m.refreshCmd([]Host{msg.host})
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
		m.modal = &textModal{title: "snapshot job failed", lines: lines}
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
		m.modal = &textModal{title: "DRY RUN — nothing was executed", lines: lines}
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
		m.modal = &textModal{title: "sync job failures", lines: failLines}
	}
	if done > 0 || running > 0 {
		m.refreshing = true
		return m, m.refreshCmd(msg.affected)
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

// ensureTicker starts the spinner/poll ticker when there is something live.
func (m *Model) ensureTicker() tea.Cmd {
	if m.ticking || len(m.runningJobHosts()) == 0 {
		return nil
	}
	m.ticking = true
	return tickCmd()
}

func (m *Model) onTick() (tea.Model, tea.Cmd) {
	hosts := m.runningJobHosts()
	if len(hosts) == 0 {
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
		m.refreshing = true
		return m, m.refreshCmd(hosts)
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

// jobProgressLine renders "1.2G / 3.4G (35%)" as far as known.
func jobProgressLine(j *JobInfo) string {
	switch {
	case j.Progress > 0 && j.Expected > 0:
		pct := j.Progress * 100 / j.Expected
		if pct > 100 {
			pct = 100
		}
		return fmt.Sprintf("%s / %s (%d%%)", human(j.Progress), human(j.Expected), pct)
	case j.Progress > 0:
		return human(j.Progress) + " transferred"
	case j.Expected > 0:
		return "~" + human(j.Expected) + " to transfer"
	default:
		return "progress unknown (pv not installed on source?)"
	}
}

// jobDetailLines renders recent jobs touching a replica for the details pane.
func (m *Model) jobDetailLines(r *Replica) []string {
	jobs := m.data.JobsByReplica[replicaKeyOf(r)]
	var lines []string
	shown := 0
	for _, j := range jobs {
		if shown >= 3 {
			break
		}
		desc := j.Kind
		if j.Kind == "sync" {
			desc = j.Meta["src"] + " → " + j.Meta["dest"]
			if j.Meta["full"] == "true" {
				desc += " (full)"
			}
		} else if ds := j.Meta["dataset"]; ds != "" {
			desc += " " + ds + "@" + j.Meta["snapshot"]
		}
		switch j.State {
		case "running":
			lines = append(lines, fmt.Sprintf("  %s job %s: %s — %s",
				spinnerFrames[m.spin%len(spinnerFrames)], j.ID, desc, jobProgressLine(&j)))
		case "failed":
			lines = append(lines, styleErr.Render(fmt.Sprintf("  ✗ job %s: %s — FAILED (%s)", j.ID, desc, j.Status)))
			for _, cl := range strings.Split(j.Meta["command"], "\n") {
				if strings.Contains(cl, "zfs ") {
					lines = append(lines, styleDim.Render("      "+strings.TrimSpace(cl)))
				}
			}
		case "done":
			lines = append(lines, styleDim.Render(fmt.Sprintf("  ✓ job %s: %s — done", j.ID, desc)))
		default:
			lines = append(lines, styleDim.Render(fmt.Sprintf("  ? job %s: %s — %s", j.ID, desc, j.State)))
		}
		shown++
	}
	return lines
}
