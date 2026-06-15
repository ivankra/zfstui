// The Details pane: a compact, embedded rendering of the current selection's
// object view (see view.go). It shares its content builders with the full
// interactive windows — the pane just asks for full=false (no section
// headers, property dumps or verbatim blocks) and renders 8 lines with a row
// cursor. Selectable rows (snapshots, jobs, pools, datasets, replicas, disks,
// vdevs) push a full window on Enter.
package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// currentView returns the objView for the main-table selection (nil if none).
func (m *Model) currentView() *objView {
	g, c := m.selected()
	switch {
	case m.curRow == -2 && m.curCol >= 1 && m.curCol <= len(m.cols):
		return m.hostView(m.hostOfCol(m.curCol - 1))
	case m.curRow == -1 && m.curCol >= 1 && m.curCol <= len(m.cols):
		parts := strings.SplitN(m.cols[m.curCol-1], ":", 2)
		return m.poolView(parts[0], parts[1])
	case g != nil && m.curCol == 0:
		return m.groupView(g)
	case g != nil && c != nil && len(c.replicas) == 1:
		return m.datasetView(c.replicas[0].Dataset.Host, c.replicas[0].Dataset.Name)
	case g != nil && c != nil && len(c.replicas) > 1:
		return m.cellView(c.replicas)
	}
	return nil
}

// detailsContent builds the compact pane content for the current selection.
func (m *Model) detailsContent() ([]string, []viewLine) {
	v := m.currentView()
	if v == nil {
		return nil, nil
	}
	return v.build(m, false)
}

// detMove moves the details cursor among selectable rows (or plain-scrolls
// when nothing is selectable), keeping it visible.
func (m *Model) detMove(dir int) {
	_, lines := m.detailsContent()
	sel := detSelectable(lines)
	m.detCur, m.detOff = scrollMove(lines, sel, m.detCur, m.detOff, dir, detailsBodyH)
	if m.detOff < 0 {
		m.detOff = 0
	}
}

// detOpen pushes the modal window for the selected row.
func (m *Model) detOpen() {
	_, lines := m.detailsContent()
	if m.detCur >= 0 && m.detCur < len(lines) && lines[m.detCur].open != nil {
		m.pushView(lines[m.detCur].open())
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
