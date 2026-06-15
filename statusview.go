// Package view renders inventory as a plain-text table (the `status`
// command; the TUI has its own renderer).
package main

import (
	"fmt"
	"sort"
	"strings"
)

// Marker suffixes:
//
//   - unsnapshotted local writes   !  diverged   %  clone
//
// "Ahead" needs no marker: the per-cell @names already show who is newest.
// Resumable carries no marker either — it's signalled by the cyan cell color
// in the TUI and surfaced in the dataset detail.
func CellText(r *Replica) string {
	var b strings.Builder
	// Display drops the prefix's trailing slash: "bak@20250409", not
	// "bak/@20250409".
	b.WriteString(strings.TrimSuffix(r.CellPrefix, "/"))
	if r.Latest != nil {
		b.WriteString("@" + r.Latest.Name)
	} else if own := r.Dataset.OwnUsed(); r.State != StateClone && own > 0 {
		// Snapshot-less: show the dataset's OWN used size (excluding children,
		// which have their own rows) in parens. Written is just
		// bytes-since-last-snapshot metadata and unreliable with no snapshot.
		b.WriteString("(" + humanShort(own) + ")")
	}
	switch r.State {
	case StateDiverged:
		b.WriteString("!")
	case StateClone:
		b.WriteString("%")
	}
	// "+" dropped: cell color (styleWritten) already signals unsnapshotted
	// writes when Latest != nil.
	if b.Len() == 0 {
		// Replica exists but has no snapshots, writes, or distinguishing
		// prefix — still mark its presence.
		return "-"
	}
	return b.String()
}

// Status renders the groups × host:pool matrix.
func Status(groups []*Group) string {
	colSet := map[string]bool{}
	for _, g := range groups {
		for _, r := range g.Members {
			colSet[r.Dataset.Host+":"+r.Dataset.Pool()] = true
		}
	}
	cols := make([]string, 0, len(colSet))
	for c := range colSet {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	colIdx := map[string]int{}
	for i, c := range cols {
		colIdx[c] = i
	}

	rows := make([][]string, 0, len(groups)+1)
	header := append([]string{""}, cols...)
	rows = append(rows, header)
	for _, g := range groups {
		row := make([]string, len(cols)+1)
		row[0] = g.DisplayName
		for _, r := range g.Members {
			i := colIdx[r.Dataset.Host+":"+r.Dataset.Pool()] + 1
			cell := CellText(r)
			if row[i] != "" {
				row[i] += " " + cell // >1 replica of a group in one pool (rare)
			} else {
				row[i] = cell
			}
		}
		rows = append(rows, row)
	}

	widths := make([]int, len(cols)+1)
	for _, row := range rows {
		for i, c := range row {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	var b strings.Builder
	for _, row := range rows {
		var line strings.Builder
		for i, c := range row {
			if i > 0 {
				line.WriteString("  ")
			}
			fmt.Fprintf(&line, "%-*s", widths[i], c)
		}
		b.WriteString(strings.TrimRight(line.String(), " "))
		b.WriteString("\n")
	}
	return b.String()
}
