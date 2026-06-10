// Package derive turns raw per-host inventory into replication groups and
// per-replica states. Everything here is a pure function of the inventory —
// see DESIGN.md "Source detection: derived from data, not declared".
package main

import (
	"sort"
	"strings"
)

// replicaKey identifies a dataset across the whole inventory.
func replicaKey(host, name string) string { return host + ":" + name }

// entry pairs a dataset with its snapshots (sorted by Creation asc).
type entry struct {
	ds    *Dataset
	snaps []*Snapshot
}

// Groups computes replication groups (connected components over shared
// snapshot GUIDs) with display names and per-replica states.
func Groups(states []HostState) []*Group {
	entries := map[string]*entry{}
	var keys []string
	for i := range states {
		st := &states[i]
		for j := range st.Datasets {
			d := &st.Datasets[j]
			k := replicaKey(d.Host, d.Name)
			entries[k] = &entry{ds: d}
			keys = append(keys, k)
		}
		for j := range st.Snapshots {
			s := &st.Snapshots[j]
			if e := entries[replicaKey(s.Host, s.Dataset)]; e != nil {
				e.snaps = append(e.snaps, s)
			}
		}
	}
	sort.Strings(keys)
	for _, e := range entries {
		sort.Slice(e.snaps, func(a, b int) bool {
			if e.snaps[a].Creation != e.snaps[b].Creation {
				return e.snaps[a].Creation < e.snaps[b].Creation
			}
			return e.snaps[a].CreateTXG < e.snaps[b].CreateTXG
		})
	}

	// Union-find over replica keys, connected by shared snapshot GUIDs.
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" {
			parent[x] = x
		}
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b string) { parent[find(a)] = find(b) }
	byGUID := map[string]string{} // snapshot guid -> first replica key seen
	for _, k := range keys {
		for _, s := range entries[k].snaps {
			if s.GUID == "" {
				continue
			}
			if first, ok := byGUID[s.GUID]; ok {
				union(first, k)
			} else {
				byGUID[s.GUID] = k
			}
		}
		// A clone shares no snapshots with its origin until snapshotted
		// itself; group it via the origin property (same host by definition).
		if o := entries[k].ds.Origin; o != "" {
			if ds, _, ok := strings.Cut(o, "@"); ok {
				ok2 := replicaKey(entries[k].ds.Host, ds)
				if _, exists := entries[ok2]; exists {
					union(ok2, k)
				}
			}
		}
	}

	components := map[string][]string{}
	for _, k := range keys {
		root := find(k)
		components[root] = append(components[root], k)
	}

	var groups []*Group
	for _, members := range components {
		sort.Strings(members)
		g := &Group{}
		var names [][]string // path components sans pool, per member
		for _, k := range members {
			e := entries[k]
			r := &Replica{Dataset: e.ds, Written: e.ds.Written}
			if n := len(e.snaps); n > 0 {
				r.Latest = e.snaps[n-1]
			}
			g.Members = append(g.Members, r)
			names = append(names, splitNameInPool(e.ds))
			if r.Latest != nil && r.Latest.Creation > g.LatestCreation {
				g.LatestCreation = r.Latest.Creation
				g.LatestName = r.Latest.Name
			}
		}
		// Clones get odd names; don't let them shrink the common suffix.
		// Boring leading components (snapshot-less container datasets like
		// "bak/", "srv/") are trimmed before the suffix computation, so they
		// end up in cell prefixes instead of the displayed group name.
		var sufNames [][]string
		for i, k := range members {
			if entries[k].ds.Origin == "" {
				sufNames = append(sufNames, trimBoring(entries, entries[k].ds, names[i]))
			}
		}
		if len(sufNames) == 0 {
			sufNames = names
		}
		suffix := commonSuffix(sufNames)
		g.DisplayName = strings.Join(suffix, "/")
		if g.DisplayName == "" {
			// No common suffix (or root datasets): fall back to full names per cell.
			g.DisplayName = entries[members[0]].ds.Name
		}
		for i := range members {
			if hasSuffix(names[i], suffix) {
				rest := names[i][:len(names[i])-len(suffix)]
				g.Members[i].CellPrefix = joinPrefix(rest)
			} else {
				// Doesn't share the suffix (a clone): show its full in-pool name.
				g.Members[i].CellPrefix = strings.Join(names[i], "/")
			}
		}
		classify(g, entries, members)
		g.Hidden = g.LatestCreation == 0 // no snapshots anywhere in the group
		groups = append(groups, g)
	}
	sort.Slice(groups, func(a, b int) bool {
		if groups[a].DisplayName != groups[b].DisplayName {
			return groups[a].DisplayName < groups[b].DisplayName
		}
		// Same display name (unrelated same-named groups): break the tie by
		// first member so the order is stable across runs.
		da, db := groups[a].Members[0].Dataset, groups[b].Members[0].Dataset
		return da.Host+":"+da.Name < db.Host+":"+db.Name
	})
	return groups
}

// trimBoring drops leading name components whose ancestor dataset (on the
// same host) exists but has no snapshots at all — pure containers like a
// bare "bak"/"srv", the same plumbing the hidden rule classifies. Trimming
// stops at the first ancestor that has snapshots (a real replicated parent,
// e.g. "blobs" of "blobs/01"), and always keeps the last component.
func trimBoring(entries map[string]*entry, d *Dataset, comps []string) []string {
	path := d.Pool()
	for len(comps) > 1 {
		path += "/" + comps[0]
		e := entries[replicaKey(d.Host, path)]
		if e == nil || len(e.snaps) > 0 {
			break
		}
		comps = comps[1:]
	}
	return comps
}

func splitNameInPool(d *Dataset) []string {
	nip := d.NameInPool()
	if nip == "" {
		return nil
	}
	return strings.Split(nip, "/")
}

func joinPrefix(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "/") + "/"
}

// hasSuffix reports whether path-component slice n ends with suffix.
func hasSuffix(n, suffix []string) bool {
	if len(suffix) > len(n) {
		return false
	}
	for i := 0; i < len(suffix); i++ {
		if n[len(n)-len(suffix)+i] != suffix[i] {
			return false
		}
	}
	return true
}

// commonSuffix returns the longest common suffix of path-component slices.
func commonSuffix(names [][]string) []string {
	if len(names) == 0 {
		return nil
	}
	shortest := len(names[0])
	for _, n := range names {
		if len(n) < shortest {
			shortest = len(n)
		}
	}
	k := 0
	for k < shortest {
		c := names[0][len(names[0])-1-k]
		for _, n := range names[1:] {
			if n[len(n)-1-k] != c {
				goto done
			}
		}
		k++
	}
done:
	return names[0][len(names[0])-k:]
}

// classify assigns each member its ReplicaState via pairwise comparison.
//
// For a pair (A, B): the base is their newest shared snapshot GUID. A is
// "ahead of" B if A has snapshots newer than the base or unsnapshotted writes
// (written > 0). Both ahead => the pair has diverged. Pairs with no shared
// snapshot are incomparable and skipped (connectivity may be transitive).
func classify(g *Group, entries map[string]*entry, members []string) {
	n := len(members)
	ahead := make([]bool, n)  // ahead of at least one peer
	behind := make([]bool, n) // behind at least one peer
	diverged := make([]bool, n)
	guidSets := make([]map[string]bool, n)
	for i, k := range members {
		guidSets[i] = map[string]bool{}
		for _, s := range entries[k].snaps {
			guidSets[i][s.GUID] = true
		}
	}
	hasNewerThan := func(i int, baseCreation int64, baseTXG int64) bool {
		e := entries[members[i]]
		if len(e.snaps) == 0 {
			return false
		}
		last := e.snaps[len(e.snaps)-1]
		return last.Creation > baseCreation ||
			(last.Creation == baseCreation && last.CreateTXG > baseTXG)
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			// Newest shared snapshot, scanning i's snaps newest-first.
			ei := entries[members[i]]
			var base *Snapshot
			for s := len(ei.snaps) - 1; s >= 0; s-- {
				if guidSets[j][ei.snaps[s].GUID] {
					base = ei.snaps[s]
					break
				}
			}
			if base == nil {
				continue // incomparable pair
			}
			iAhead := hasNewerThan(i, base.Creation, base.CreateTXG) || ei.ds.Written > 0
			jAhead := hasNewerThan(j, base.Creation, base.CreateTXG) || entries[members[j]].ds.Written > 0
			switch {
			case iAhead && jAhead:
				diverged[i], diverged[j] = true, true
			case iAhead:
				ahead[i], behind[j] = true, true
			case jAhead:
				ahead[j], behind[i] = true, true
			}
		}
	}
	for i, r := range g.Members {
		switch {
		case r.Dataset.Origin != "":
			r.State = StateClone
		case r.Dataset.ResumeToken != "":
			r.State = StateResumable
		case diverged[i]:
			r.State = StateDiverged
			g.Diverged = true
		case ahead[i] && !behind[i]:
			r.State = StateAhead
		case behind[i]:
			r.State = StateBehind
		default:
			r.State = StateInSync
		}
	}
}
