package main

import (
	"testing"
)

// fixture builders -----------------------------------------------------------

type fx struct {
	states map[string]*HostState
}

func newFx() *fx { return &fx{states: map[string]*HostState{}} }

func (f *fx) host(name string) *HostState {
	if st, ok := f.states[name]; ok {
		return st
	}
	st := &HostState{Host: name}
	f.states[name] = st
	return st
}

func (f *fx) ds(host, name string, written int64, origin string) {
	st := f.host(host)
	st.Datasets = append(st.Datasets, Dataset{
		Host: host, Name: name, Type: "filesystem", GUID: host + ":" + name,
		Written: written, Origin: origin,
	})
}

// snap adds a snapshot; guid is the cross-host identity, creation orders them.
func (f *fx) snap(host, dataset, name, guid string, creation int64) {
	st := f.host(host)
	st.Snapshots = append(st.Snapshots, Snapshot{
		Host: host, Dataset: dataset, Name: name, GUID: guid, Creation: creation,
	})
}

func (f *fx) groups() []*Group {
	var states []HostState
	for _, st := range f.states {
		states = append(states, *st)
	}
	return Groups(states)
}

func findGroup(t *testing.T, groups []*Group, name string) *Group {
	t.Helper()
	for _, g := range groups {
		if g.DisplayName == name {
			return g
		}
	}
	t.Fatalf("group %q not found; have %v", name, names(groups))
	return nil
}

func names(groups []*Group) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g.DisplayName)
	}
	return out
}

func member(t *testing.T, g *Group, host, dataset string) *Replica {
	t.Helper()
	for _, r := range g.Members {
		if r.Dataset.Host == host && r.Dataset.Name == dataset {
			return r
		}
	}
	t.Fatalf("member %s:%s not in group %q", host, dataset, g.DisplayName)
	return nil
}

// tests -----------------------------------------------------------------------

// The README's canonical setup: master on dev1:ssd, fresh backup on dev2:ssd,
// stale backup on dev2:tank. Master has unsnapshotted writes.
func TestTypicalMasterAndBackups(t *testing.T) {
	f := newFx()
	f.ds("dev1", "ssd/srv/git", 4096, "")
	f.snap("dev1", "ssd/srv/git", "20260501", "g1", 100)
	f.snap("dev1", "ssd/srv/git", "20260601", "g2", 200)
	f.ds("dev2", "ssd/bak/git", 0, "")
	f.snap("dev2", "ssd/bak/git", "20260501", "g1", 100)
	f.snap("dev2", "ssd/bak/git", "20260601", "g2", 200)
	f.ds("dev2", "tank/bak/git", 0, "")
	f.snap("dev2", "tank/bak/git", "20260501", "g1", 100)

	groups := f.groups()
	if len(groups) != 1 {
		t.Fatalf("want 1 group, got %v", names(groups))
	}
	g := findGroup(t, groups, "git")
	if g.LatestName != "20260601" {
		t.Errorf("latest = %q", g.LatestName)
	}
	if g.Diverged {
		t.Error("group should not be diverged")
	}
	if g.Hidden {
		t.Error("group with snapshots must not be hidden")
	}

	// master and the fresh backup BOTH hold the newest snapshot (20260601);
	// the master differs only by unsnapshotted writes → "modified", and the
	// fresh backup is fully replicated → "synced" (NOT behind, despite the
	// master's pending writes). Only the tank backup, missing 20260601, is stale.
	m := member(t, g, "dev1", "ssd/srv/git")
	if m.State != StateModified || m.CellPrefix != "srv/" {
		t.Errorf("master: state=%s prefix=%q", m.State, m.CellPrefix)
	}
	fresh := member(t, g, "dev2", "ssd/bak/git")
	if fresh.State != StateInSync {
		t.Errorf("fresh backup: state=%s (want synced)", fresh.State)
	}
	if fresh.CellPrefix != "bak/" {
		t.Errorf("fresh backup prefix=%q", fresh.CellPrefix)
	}
	stale := member(t, g, "dev2", "tank/bak/git")
	if stale.State != StateBehind || stale.Latest.Name != "20260501" {
		t.Errorf("stale backup: state=%s latest=%v", stale.State, stale.Latest)
	}
}

// With no writes and equal snapshots everywhere, everyone is in sync and no
// replica is "ahead" — master is moot.
func TestAllInSync(t *testing.T) {
	f := newFx()
	f.ds("dev1", "ssd/srv/git", 0, "")
	f.snap("dev1", "ssd/srv/git", "s", "g1", 100)
	f.ds("dev2", "ssd/bak/git", 0, "")
	f.snap("dev2", "ssd/bak/git", "s", "g1", 100)

	g := findGroup(t, f.groups(), "git")
	for _, r := range g.Members {
		if r.State != StateInSync {
			t.Errorf("%s:%s state=%s", r.Dataset.Host, r.Dataset.Name, r.State)
		}
	}
}

// Both sides have data past the common base => diverged, both flagged.
func TestDivergence(t *testing.T) {
	f := newFx()
	f.ds("dev1", "ssd/srv/git", 0, "")
	f.snap("dev1", "ssd/srv/git", "base", "g1", 100)
	f.snap("dev1", "ssd/srv/git", "mine", "g2", 200)
	f.ds("dev2", "ssd/bak/git", 0, "")
	f.snap("dev2", "ssd/bak/git", "base", "g1", 100)
	f.snap("dev2", "ssd/bak/git", "theirs", "g3", 150)

	g := findGroup(t, f.groups(), "git")
	if !g.Diverged {
		t.Fatal("group should be diverged")
	}
	for _, r := range g.Members {
		if r.State != StateDiverged {
			t.Errorf("%s state=%s", r.Dataset.Name, r.State)
		}
	}
}

// Uncommitted writes are NOT divergence: two replicas sharing the same latest
// snapshot, each with local writes (e.g. atime on mounted copies), are both
// "modified" — not diverged. Divergence requires independent SNAPSHOTS past a
// common base. (Old behavior flagged this as false divergence; see the atime
// caveat in DESIGN.md — fixed by the snapshot/writes split.)
func TestWritesAreModifiedNotDiverged(t *testing.T) {
	f := newFx()
	f.ds("dev1", "ssd/srv/git", 4096, "")
	f.snap("dev1", "ssd/srv/git", "base", "g1", 100)
	f.ds("dev2", "ssd/bak/git", 512, "")
	f.snap("dev2", "ssd/bak/git", "base", "g1", 100)

	g := findGroup(t, f.groups(), "git")
	if g.Diverged {
		t.Fatal("uncommitted writes must not be treated as divergence")
	}
	for _, r := range g.Members {
		if r.State != StateModified {
			t.Errorf("%s state=%s (want modified)", r.Dataset.Name, r.State)
		}
	}
}

// Same dataset name on two hosts but no shared snapshots: separate groups
// (the rpool/cache case).
func TestSameNameUnrelated(t *testing.T) {
	f := newFx()
	f.ds("dev1", "rpool/ROOT", 0, "")
	f.snap("dev1", "rpool/ROOT", "s", "a1", 100)
	f.ds("dev2", "rpool/ROOT", 0, "")
	f.snap("dev2", "rpool/ROOT", "s", "b1", 100)

	groups := f.groups()
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %v", names(groups))
	}
}

// Datasets with no snapshots at all are singleton groups.
func TestNoSnapshots(t *testing.T) {
	f := newFx()
	f.ds("dev1", "ssd/scratch", 0, "")
	groups := f.groups()
	if len(groups) != 1 || len(groups[0].Members) != 1 {
		t.Fatalf("want singleton group, got %v", names(groups))
	}
	if groups[0].Members[0].State != StateInSync {
		t.Errorf("state=%s", groups[0].Members[0].State)
	}
	if !groups[0].Hidden {
		t.Error("snapshot-less group should be hidden by default")
	}
}

// A clone is annotated as such, not counted toward divergence.
func TestClone(t *testing.T) {
	f := newFx()
	f.ds("dev1", "ssd/srv/git", 0, "")
	f.snap("dev1", "ssd/srv/git", "base", "g1", 100)
	// A real clone owns no snapshots until snapshotted; it joins the group
	// via its origin property, not via shared GUIDs.
	f.ds("dev1", "ssd/srv/git-experiment", 9999, "ssd/srv/git@base")
	f.ds("dev2", "ssd/bak/git", 0, "")
	f.snap("dev2", "ssd/bak/git", "base", "g1", 100)

	groups := f.groups()
	if len(groups) != 1 {
		t.Fatalf("want 1 group, got %v", names(groups))
	}
	g := findGroup(t, groups, "git") // clone name must not pollute the display name
	c := member(t, g, "dev1", "ssd/srv/git-experiment")
	if c.State != StateClone {
		t.Errorf("clone state=%s", c.State)
	}
	if c.CellPrefix != "srv/git-experiment" {
		t.Errorf("clone cell prefix=%q", c.CellPrefix)
	}
	if g.Diverged {
		t.Error("clone writes must not mark the group diverged")
	}
}

// Replica with an interrupted recv shows resumable.
func TestResumable(t *testing.T) {
	f := newFx()
	f.ds("dev1", "ssd/srv/git", 0, "")
	f.snap("dev1", "ssd/srv/git", "a", "g1", 100)
	f.snap("dev1", "ssd/srv/git", "b", "g2", 200)
	st := f.host("hetzner")
	st.Datasets = append(st.Datasets, Dataset{
		Host: "hetzner", Name: "tank/bak/git", Type: "filesystem",
		ResumeToken: "1-abc",
	})
	f.snap("hetzner", "tank/bak/git", "a", "g1", 100)

	g := findGroup(t, f.groups(), "git")
	r := member(t, g, "hetzner", "tank/bak/git")
	if r.State != StateResumable {
		t.Errorf("state=%s", r.State)
	}
}

// Display names: longest common path suffix; hierarchical children are
// separate groups; the parent dataset is its own group/row.
func TestHierarchicalAndDisplayNames(t *testing.T) {
	f := newFx()
	for _, child := range []string{"blobs", "blobs/01", "blobs/02"} {
		f.ds("dev2", "tank/srv/"+child, 0, "")
		f.snap("dev2", "tank/srv/"+child, "s", "g-"+child, 100)
		f.ds("hetzner", "tank/bak/"+child, 0, "")
		f.snap("hetzner", "tank/bak/"+child, "s", "g-"+child, 100)
	}
	groups := f.groups()
	if len(groups) != 3 {
		t.Fatalf("want 3 groups, got %v", names(groups))
	}
	// Sorted: blobs, blobs/01, blobs/02 — parent row above children.
	want := []string{"blobs", "blobs/01", "blobs/02"}
	for i, w := range want {
		if groups[i].DisplayName != w {
			t.Errorf("groups[%d] = %q, want %q", i, groups[i].DisplayName, w)
		}
	}
}

// Leading components that are snapshot-less container datasets ("bak/",
// "srv/") are trimmed from display names — even when every replica shares
// them — and show up as cell prefixes instead. A parent that has snapshots
// of its own (replicated "blobs") is not trimmed.
func TestBoringPrefixTrimming(t *testing.T) {
	f := newFx()
	// Containers without snapshots on both hosts.
	f.ds("ws", "tank/bak", 100, "")
	f.ds("cml", "root/bak", 100, "")
	f.ds("ws", "tank/srv", 100, "")

	// Same "bak/" prefix on every replica: previously displayed "bak/anki".
	f.ds("ws", "tank/bak/anki", 0, "")
	f.snap("ws", "tank/bak/anki", "s", "g1", 100)
	f.ds("cml", "root/bak/anki", 0, "")
	f.snap("cml", "root/bak/anki", "s", "g1", 100)

	// Single replica behind a boring prefix.
	f.ds("ws", "tank/bak/music", 0, "")
	f.snap("ws", "tank/bak/music", "s", "g2", 100)

	// Parent with its own snapshots is meaningful: not trimmed.
	f.ds("ws", "tank/srv/blobs", 0, "")
	f.snap("ws", "tank/srv/blobs", "s", "g3", 100)
	f.ds("ws", "tank/srv/blobs/01", 0, "")
	f.snap("ws", "tank/srv/blobs/01", "s", "g4", 100)

	groups := f.groups()
	anki := findGroup(t, groups, "anki")
	if p := member(t, anki, "ws", "tank/bak/anki").CellPrefix; p != "bak/" {
		t.Errorf("anki ws cell prefix = %q", p)
	}
	if p := member(t, anki, "cml", "root/bak/anki").CellPrefix; p != "bak/" {
		t.Errorf("anki cml cell prefix = %q", p)
	}
	music := findGroup(t, groups, "music")
	if p := member(t, music, "ws", "tank/bak/music").CellPrefix; p != "bak/" {
		t.Errorf("music cell prefix = %q", p)
	}
	blobs01 := findGroup(t, groups, "blobs/01")
	if p := member(t, blobs01, "ws", "tank/srv/blobs/01").CellPrefix; p != "srv/" {
		t.Errorf("blobs/01 cell prefix = %q", p)
	}
	findGroup(t, groups, "blobs") // the parent row, "srv/" trimmed
}

// Transitive chain A-B-C where A and C share nothing directly: still one
// group; C just shows as behind, not diverged.
func TestTransitiveChain(t *testing.T) {
	f := newFx()
	f.ds("a", "p/srv/d", 0, "")
	f.snap("a", "p/srv/d", "s2", "g2", 200)
	f.snap("a", "p/srv/d", "s3", "g3", 300)
	f.ds("b", "p/bak/d", 0, "")
	f.snap("b", "p/bak/d", "s1", "g1", 100)
	f.snap("b", "p/bak/d", "s2", "g2", 200)
	f.ds("c", "p/bak/d", 0, "")
	f.snap("c", "p/bak/d", "s1", "g1", 100)

	groups := f.groups()
	if len(groups) != 1 {
		t.Fatalf("want 1 group, got %v", names(groups))
	}
	g := groups[0]
	if g.Diverged {
		t.Error("chain should not be diverged")
	}
	if s := member(t, g, "a", "p/srv/d").State; s != StateAhead {
		t.Errorf("a state=%s", s)
	}
	if s := member(t, g, "c", "p/bak/d").State; s != StateBehind {
		t.Errorf("c state=%s", s)
	}
}

// Field bug (2026-06-11): a freshly received replica carries the SAME
// snapshot GUID but a different (pool-local!) createtxg — typically larger,
// since it's the txg of the receive. That must not make it "ahead": equal
// GUIDs are equal snapshots, and createtxg must never be compared across
// replicas.
func TestReceivedReplicaNotAheadDespiteLargerTXG(t *testing.T) {
	f := newFx()
	f.ds("ws", "ws-boot/ws-boot", 0, "")
	st := f.host("ws")
	st.Snapshots = append(st.Snapshots,
		Snapshot{Host: "ws", Dataset: "ws-boot/ws-boot", Name: "20260611",
			GUID: "g1", Creation: 500, CreateTXG: 100},
		// the backup, received later: same guid/creation, huge local txg
	)
	f.ds("ws", "ws-tank/bak/ws-boot", 0, "")
	st.Snapshots = append(st.Snapshots,
		Snapshot{Host: "ws", Dataset: "ws-tank/bak/ws-boot", Name: "20260611",
			GUID: "g1", Creation: 500, CreateTXG: 999999},
	)

	g := findGroup(t, f.groups(), "ws-boot")
	for _, r := range g.Members {
		if r.State != StateInSync {
			t.Errorf("%s: state=%s, want in-sync", r.Dataset.Name, r.State)
		}
	}
}
