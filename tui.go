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
	styleHeader   = lipgloss.NewStyle().Bold(true).Underline(true)
	styleName     = lipgloss.NewStyle().Bold(true)
	styleAhead    = lipgloss.NewStyle().Bold(true)
	styleBehind   = lipgloss.NewStyle().Foreground(lipgloss.Color("3")) // yellow
	styleDiverged = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // red
	styleResume   = lipgloss.NewStyle().Foreground(lipgloss.Color("6")) // cyan
	styleClone    = lipgloss.NewStyle().Foreground(lipgloss.Color("8")) // dim
	styleInSync   = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	styleDim      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleCursor   = lipgloss.NewStyle().Reverse(true)
	styleStatus   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleErr      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

func stateStyle(s ReplicaState) lipgloss.Style {
	switch s {
	case StateAhead:
		return styleAhead
	case StateBehind:
		return styleBehind
	case StateDiverged:
		return styleDiverged
	case StateResumable:
		return styleResume
	case StateClone:
		return styleClone
	default:
		return styleInSync
	}
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

	showHidden bool
	showHelp   bool
	refreshing bool
	status     string
	statusErr  bool

	dryRun bool
	dialog *snapDialog // snapshot dialog when non-nil
	modal  *textModal  // scrollable centered text modal (dry-run details, job logs)
}

// textModal is a centered, scrollable read-only text box.
type textModal struct {
	title string
	lines []string
	off   int
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

	var curName string
	if m.curRow < len(m.rows) && len(m.rows) > 0 {
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

	// Restore cursor onto the same group if possible.
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

func (m *Model) Init() tea.Cmd { return nil }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

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
		return m, nil

	case snapDoneMsg:
		return m.onSnapDone(msg)

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
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "?":
			m.showHelp = true
		case "tab", "shift+tab":
			m.focus = 1 - m.focus
		case "up", "k":
			if m.focus == 1 {
				m.detOff--
			} else {
				m.curRow--
				m.detOff = 0
			}
		case "down", "j":
			if m.focus == 1 {
				m.detOff++
			} else {
				m.curRow++
				m.detOff = 0
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
			m.detOff = 0
		case "right":
			if m.focus == 1 {
				break
			}
			if m.curRow == -2 {
				m.moveHost(1)
			} else {
				m.curCol++
			}
			m.detOff = 0
		case "h": // toggle hidden groups, not vim-left: arrows move horizontally
			m.showHidden = !m.showHidden
			if err := m.reload(); err != nil {
				m.status, m.statusErr = err.Error(), true
			}
		case "g", "home":
			if m.focus == 1 {
				m.detOff = 0
			} else {
				m.curRow = 0
			}
		case "G", "end":
			if m.focus == 1 {
				m.detOff = 1 << 30 // clamped to bottom at render
			} else {
				m.curRow = len(m.rows) - 1
			}
		case "s":
			m.openSnapshotDialog()
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
	b.WriteString(m.paneTitle("Datasets", m.focus == 0))
	b.WriteString(m.tableView(m.tableBodyH()))
	b.WriteString(m.paneTitle("Details", m.focus == 1))
	b.WriteString(m.detailsBody())
	b.WriteString(m.footerView())
	if m.dialog != nil {
		// Modal: composite the dialog box over the normal view.
		return overlayCenter(b.String(), m.dialogView(), m.width, m.height)
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
	case "pgup":
		mo.off -= bodyH
	case "pgdown", " ":
		mo.off += bodyH
	case "g", "home":
		mo.off = 0
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
	b.WriteString(styleDialogTitle.Render(mo.title) + "\n\n")
	end := mo.off + bodyH
	if end > len(mo.lines) {
		end = len(mo.lines)
	}
	for i := mo.off; i < end; i++ {
		b.WriteString(pad(mo.lines[i], maxW) + "\n")
	}
	for i := end - mo.off; i < bodyH; i++ {
		b.WriteString("\n")
	}
	pos := ""
	if len(mo.lines) > bodyH {
		pos = fmt.Sprintf("  %d–%d/%d", mo.off+1, end, len(mo.lines))
	}
	b.WriteString("\n" + styleDim.Render("↑↓/jk scroll · PgUp/PgDn · Esc close"+pos))
	return styleDialogBox.Render(b.String())
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

// paneTitle renders "── Name ───…" with the name highlighted when focused.
func (m *Model) paneTitle(name string, active bool) string {
	label := " " + name + " "
	fill := m.width - 2 - len(label)
	if fill < 0 {
		fill = 0
	}
	styled := styleDim.Render(label)
	if active {
		styled = styleCursor.Render(label)
	}
	return styleDim.Render("──") + styled + styleDim.Render(strings.Repeat("─", fill)) + "\n"
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
				if t := len(cl.text()); t > w {
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
		hdr := pad(host, span-2)
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
		name := pad(g.DisplayName, nameW)
		nstyle := styleName
		if g.Hidden {
			nstyle = styleDim
		}
		if ri == m.curRow && m.curCol == 0 {
			b.WriteString(styleCursor.Render(name))
		} else {
			b.WriteString(nstyle.Render(name))
		}
		for ci := m.colOff; ci < end; ci++ {
			b.WriteString("  ")
			c, ok := m.cells[ri][ci]
			txt := ""
			st := styleInSync
			if ok {
				txt = c.text()
				if len(c.replicas) == 1 {
					st = stateStyle(c.replicas[0].State)
				}
			}
			cellStr := pad(txt, colWs[ci])
			if ri == m.curRow && ci == m.curCol-1 {
				b.WriteString(styleCursor.Render(cellStr))
			} else if m.unreach[strings.SplitN(m.cols[ci], ":", 2)[0]] {
				b.WriteString(styleDim.Render(cellStr))
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

// detailLines builds the full (unscrolled) Details pane content.
func (m *Model) detailLines() []string {
	var lines []string
	g, c := m.selected()
	switch {
	case m.curRow == -2:
		lines = m.hostDetails()
	case m.curRow == -1:
		lines = m.poolDetails()
	case g == nil:
		lines = append(lines, "nothing selected")
	case m.curCol == 0:
		// Name column: group-level summary, columns aligned.
		lines = append(lines, fmt.Sprintf("group %s — %d replica(s), latest @%s",
			g.DisplayName, len(g.Members), orDash(g.LatestName)))
		nameW := 0
		for _, r := range g.Members {
			if l := len(r.Dataset.Host + ":" + r.Dataset.Name); l > nameW {
				nameW = l
			}
		}
		for _, r := range g.Members {
			lines = append(lines, fmt.Sprintf("  %-*s  %-9s  %s",
				nameW, r.Dataset.Host+":"+r.Dataset.Name, r.State, replicaBrief(r)))
		}
	case c == nil || len(c.replicas) == 0:
		// Empty cell: this group has no replica in this host:pool.
	default:
		for _, r := range c.replicas {
			d := r.Dataset
			enc := d.Encryption
			if enc == "" {
				enc = "off"
			}
			lines = append(lines,
				fmt.Sprintf("%s:%s  [%s]", d.Host, d.Name, r.State),
				fmt.Sprintf("  used %s  refer %s  written %s  encryption %s  mounted %v (%s)",
					human(d.Used), human(d.Refer), human(d.Written), enc, d.Mounted, orDash(d.Mountpoint)),
			)
			if d.Origin != "" {
				lines = append(lines, "  clone of "+d.Origin)
			}
			if d.ResumeToken != "" {
				lines = append(lines, "  interrupted recv (resume token present)")
			}
			snaps := m.data.SnapsByReplica[d.Host+":"+d.Name]
			lines = append(lines, "  snapshots: "+snapList(snaps, 6))
		}
	}
	return lines
}

// detailsBody renders detailsBodyH lines of detail content at m.detOff,
// with ↑/↓ markers when content is scrolled out of view.
func (m *Model) detailsBody() string {
	lines := m.detailLines()
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
			txt := pad(lines[idx], m.width-2)
			switch {
			case i == 0 && m.detOff > 0:
				txt += styleDim.Render(" ↑")
			case i == detailsBodyH-1 && idx < len(lines)-1:
				txt += styleDim.Render(" ↓")
			}
			b.WriteString(txt)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// hostDetails describes the host of the selected header column.
func (m *Model) hostDetails() []string {
	if m.curCol < 1 || m.curCol > len(m.cols) {
		return nil
	}
	host := m.hostOfCol(m.curCol - 1)
	var lines []string
	for _, h := range m.data.Hosts {
		if h.Name != host {
			continue
		}
		when := h.LastRefresh.Format("2006-01-02 15:04:05")
		if h.Reachable {
			lines = append(lines, fmt.Sprintf("host %s — refreshed %s", host, when))
		} else {
			lines = append(lines, fmt.Sprintf("host %s — UNREACHABLE at last refresh (%s): %s",
				host, when, h.LastError))
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "host "+host)
	}
	for _, h := range m.hosts {
		if h.Name == host {
			extra := ""
			if h.NeedSudo() {
				extra = "  (sudo)"
			}
			lines = append(lines, "  access "+h.URL()+extra)
		}
	}
	for i := range m.data.States {
		hs := &m.data.States[i]
		if hs.Host != host {
			continue
		}
		for _, p := range hs.Pools {
			pct := ""
			if p.Size > 0 {
				pct = fmt.Sprintf(" (%d%%)", p.Alloc*100/p.Size)
			}
			lines = append(lines, fmt.Sprintf("  pool %-12s %-9s size %-8s alloc %s%s free %s",
				p.Name, p.Health, human(p.Size), human(p.Alloc), pct, human(p.Free)))
		}
		lines = append(lines, fmt.Sprintf("  %d dataset(s), %d snapshot(s) cached",
			len(hs.Datasets), len(hs.Snapshots)))
	}
	return lines
}

// poolDetails describes the pool of the selected header column.
func (m *Model) poolDetails() []string {
	if m.curCol < 1 || m.curCol > len(m.cols) {
		return nil
	}
	parts := strings.SplitN(m.cols[m.curCol-1], ":", 2)
	host, pool := parts[0], parts[1]
	var p *Pool
	nDatasets, nSnaps := 0, 0
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
				nDatasets++
			}
		}
		for _, s := range hs.Snapshots {
			dsPool, _, _ := strings.Cut(s.Dataset, "/")
			if dsPool == pool {
				nSnaps++
			}
		}
	}
	if p == nil {
		return []string{fmt.Sprintf("pool %s:%s — not in cache", host, pool)}
	}
	usedPct := ""
	if p.Size > 0 {
		usedPct = fmt.Sprintf(" (%d%%)", p.Alloc*100/p.Size)
	}
	lines := []string{
		fmt.Sprintf("pool %s:%s — %s", host, pool, p.Health),
		fmt.Sprintf("  size %s  alloc %s%s  free %s  guid %s",
			human(p.Size), human(p.Alloc), usedPct, human(p.Free), orDash(p.GUID)),
		fmt.Sprintf("  %d dataset(s), %d snapshot(s) cached", nDatasets, nSnaps),
	}
	for _, h := range m.data.Hosts {
		if h.Name == host {
			when := h.LastRefresh.Format("2006-01-02 15:04:05")
			if !h.Reachable {
				lines = append(lines, styleErr.Render(
					fmt.Sprintf("  host unreachable at last refresh (%s): %s", when, h.LastError)))
			} else {
				lines = append(lines, fmt.Sprintf("  host refreshed %s", when))
			}
		}
	}
	return lines
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
	keys := "q quit · ? help · ←↓↑→ move · Tab focus · s snap · r/R refresh · h hidden"
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
		line += styleStatus.Render(right)
	}
	return line
}

func (m *Model) helpView() string {
	help := `zfstui — keys

  ←↓↑→ / jk     move cursor (name column selects the whole group; ↑ past the
                 first row selects a pool, ↑ again a host — details follow)
  Tab            switch focus between Datasets and Details (jk/↑↓ scroll there)
  s              snapshot the selected dataset (dialog: name, recursive, confirm)
  g / G          first / last row
  r              refresh the selected cell's host (all hosts if on name column)
  R              refresh all hosts
  h              toggle showing hidden groups (currently ` + fmt.Sprint(m.hiddenN) + ` hidden:
                 groups with no snapshots anywhere, usually system/plumbing datasets)
  q              quit
  ?              close this help

cell markers:  + unsnapshotted writes  ! diverged  ~ resumable  % clone
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
	styleDialogTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	styleFieldLabel  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleButton      = lipgloss.NewStyle().Padding(0, 1).Foreground(lipgloss.Color("7"))
	styleButtonFocus = lipgloss.NewStyle().Padding(0, 1).Reverse(true).Bold(true)
	styleFocusMark   = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
)

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
	b.WriteString(styleDialogTitle.Render("Create snapshot") + "\n\n")
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
	return styleDialogBox.Render(b.String())
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
