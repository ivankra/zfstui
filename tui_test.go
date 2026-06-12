package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func dialogTestModel(t *testing.T) *Model {
	t.Helper()
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	err = st.PutHostState(HostState{
		Host: "dev1",
		Datasets: []Dataset{
			{Host: "dev1", Name: "ssd/srv/git", Type: "filesystem", GUID: "d1", Written: 4096},
		},
		Snapshots: []Snapshot{
			{Host: "dev1", Dataset: "ssd/srv/git", Name: "20260601", GUID: "s1", Creation: 100},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(st, []Host{{Name: "dev1", User: "ivan"}}, true /* dry-run */)
	if err != nil {
		t.Fatal(err)
	}
	m.width, m.height = 100, 30
	return m
}

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEscape}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func TestSnapshotDialogDryRunFlow(t *testing.T) {
	m := dialogTestModel(t)

	// Name column: single replica isn't "ahead" of anyone — must refuse.
	m.curRow, m.curCol = 0, 0
	m.openSnapshotDialog()
	if m.dialog != nil {
		t.Fatal("dialog should not open from name column without a sole ahead replica")
	}

	// On the cell: dialog opens with today's date prefilled.
	m.curCol = 1
	m.openSnapshotDialog()
	if m.dialog == nil {
		t.Fatal("dialog should open on a replica cell")
	}
	if got := m.dialog.input.Value(); got != time.Now().Format("20060102") {
		t.Errorf("default name = %q", got)
	}
	if m.dialog.host.User != "ivan" {
		t.Errorf("host access not picked up: %+v", m.dialog.host)
	}

	// Type a custom name (clear first), toggle recursive, continue.
	m.Update(key("ctrl+u"))
	for _, r := range "pre-upgrade" {
		m.Update(key(string(r)))
	}
	m.Update(key("tab")) // focus checkbox
	m.Update(key(" "))   // toggle recursive on
	if !m.dialog.recursive {
		t.Fatal("space should toggle the recursive checkbox")
	}
	// The form previews the exact command before Create is pressed.
	if !strings.Contains(m.View(), `sudo -n zfs snapshot -r 'ssd/srv/git@pre-upgrade'`) {
		t.Errorf("form should preview the command:\n%s", m.View())
	}

	// Create in dry-run mode: dialog closes, scrollable details modal opens.
	m.Update(key("enter"))
	if m.dialog != nil {
		t.Error("dialog should close")
	}
	if m.modal == nil {
		t.Fatal("dry-run details modal should open")
	}
	content := strings.Join(m.modal.lines, "\n")
	if !strings.Contains(content, `sudo -n zfs snapshot -r 'ssd/srv/git@pre-upgrade'`) ||
		!strings.Contains(content, "/tmp/zfstui-job-") ||
		!strings.Contains(content, "#!/usr/bin/env bash") {
		t.Errorf("modal content:\n%s", content)
	}
	if !strings.Contains(m.modal.title, "DRY RUN") {
		t.Errorf("modal title: %q", m.modal.title)
	}

	// Scrollable: j moves down, g back to top; Esc closes.
	m.Update(key("j"))
	m.Update(key("j"))
	if m.modal.off != 2 {
		t.Errorf("scroll off = %d", m.modal.off)
	}
	m.Update(key("g"))
	if m.modal.off != 0 {
		t.Errorf("scroll home off = %d", m.modal.off)
	}
	m.Update(key("esc"))
	if m.modal != nil {
		t.Error("Esc should close the modal")
	}
}

func TestSnapshotDialogRejectsDuplicateAndBadNames(t *testing.T) {
	m := dialogTestModel(t)
	m.curRow, m.curCol = 0, 1
	m.openSnapshotDialog()

	// Existing snapshot name.
	m.Update(key("ctrl+u"))
	for _, r := range "20260601" {
		m.Update(key(string(r)))
	}
	m.Update(key("enter"))
	if m.dialog == nil || !strings.Contains(m.dialog.errMsg, "already exists") {
		t.Errorf("dup not rejected: %+v", m.dialog)
	}

	// Invalid name.
	m.Update(key("ctrl+u"))
	m.Update(key("enter"))
	if m.dialog == nil || m.dialog.errMsg == "" {
		t.Errorf("empty name not rejected: %+v", m.dialog)
	}
	if m.modal != nil {
		t.Error("no modal must open on validation failure")
	}

	// Esc closes.
	m.Update(key("esc"))
	if m.dialog != nil {
		t.Error("Esc should close the dialog")
	}
}

func syncTestModel(t *testing.T) *Model {
	t.Helper()
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sn := func(host, ds, name, guid string, c int64) Snapshot {
		return Snapshot{Host: host, Dataset: ds, Name: name, GUID: guid, Creation: c}
	}
	put := func(host, ds string, written int64, snaps ...Snapshot) {
		err := st.PutHostState(HostState{
			Host:      host,
			Datasets:  []Dataset{{Host: host, Name: ds, Type: "filesystem", GUID: host + ds, Written: written}},
			Snapshots: snaps,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	put("ws", "tank/srv/git", 0,
		sn("ws", "tank/srv/git", "20260501", "g1", 100),
		sn("ws", "tank/srv/git", "20260611", "g2", 200))
	put("cml", "root/bak/git", 0,
		sn("cml", "root/bak/git", "20260501", "g1", 100))
	put("hetzner", "tank/bak/git", 0,
		sn("hetzner", "tank/bak/git", "20260611", "g2", 200))
	put("dev", "tank/bak/git", 0,
		sn("dev", "tank/bak/git", "20260501", "g1", 100),
		sn("dev", "tank/bak/git", "own", "gX", 150))

	hosts := []Host{{Name: "ws"}, {Name: "cml", User: "ivan"}, {Name: "hetzner"}, {Name: "dev"}}
	m, err := New(st, hosts, true /* dry-run */)
	if err != nil {
		t.Fatal(err)
	}
	m.width, m.height = 110, 34
	return m
}

func (m *Model) moveCursorToCol(t *testing.T, col string) {
	t.Helper()
	for i, c := range m.cols {
		if c == col {
			m.curRow, m.curCol = 0, i+1
			return
		}
	}
	t.Fatalf("column %q not found in %v", col, m.cols)
}

func TestSyncDialogFlow(t *testing.T) {
	m := syncTestModel(t)
	m.moveCursorToCol(t, "ws:tank")
	m.openSyncDialog()
	if m.syncDlg == nil {
		t.Fatal("sync dialog should open")
	}
	d := m.syncDlg

	// Only the behind destination is selectable and pre-checked; up-to-date
	// and diverged are listed but not selectable.
	if len(d.selectable) != 1 {
		t.Fatalf("selectable: %v (plans %+v)", d.selectable, d.plans)
	}
	sel := d.plans[d.selectable[0]]
	if sel.Dest.Dataset.Host != "cml" || !d.checked[d.selectable[0]] {
		t.Errorf("pre-checked selection: %+v", sel)
	}
	view := m.View()
	for _, want := range []string{"up to date", "diverged", "@20260501 ⇒ @20260611", "Sync (dry run)"} {
		if !strings.Contains(view, want) {
			t.Errorf("dialog view missing %q", want)
		}
	}

	// Submit (dry run): modal with the pipeline; agent-forward note present.
	m.Update(key("enter"))
	if m.syncDlg != nil || m.modal == nil {
		t.Fatal("expected details modal after dry-run submit")
	}
	content := strings.Join(m.modal.lines, "\n")
	for _, want := range []string{
		`zfs send -L -c -I 'tank/srv/git@20260501' 'tank/srv/git@20260611'`,
		`$DSSH "sudo -n zfs recv -s -u 'root/bak/git'"`, // ivan@cml => sudo
		"ssh -A", "ivan@cml", "#!/usr/bin/env bash",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("modal missing %q:\n%s", want, content)
		}
	}
}

func TestSyncDialogUnselectAll(t *testing.T) {
	m := syncTestModel(t)
	m.moveCursorToCol(t, "ws:tank")
	m.openSyncDialog()
	m.Update(key(" ")) // focus starts on the only checkbox: uncheck it
	m.Update(key("enter"))
	if m.syncDlg == nil || m.syncDlg.errMsg == "" {
		t.Fatal("submitting with nothing selected must show an error")
	}
	m.Update(key("esc"))
	if m.syncDlg != nil {
		t.Error("Esc should close")
	}
}

func TestSyncDialogFromUpToDateSource(t *testing.T) {
	m := syncTestModel(t)
	// Source = hetzner (in sync with ws): everything is up-to-date,
	// diverged or otherwise unselectable; dialog still opens, just with
	// nothing pre-checked.
	m.moveCursorToCol(t, "hetzner:tank")
	m.openSyncDialog()
	if m.syncDlg == nil {
		t.Fatal("dialog should open")
	}
	if len(m.syncDlg.selectable) != 0 {
		t.Errorf("nothing should be selectable: %+v", m.syncDlg.plans)
	}
}

func TestSyncDialogCustomDestination(t *testing.T) {
	m := syncTestModel(t)
	m.moveCursorToCol(t, "ws:tank")
	m.openSyncDialog()
	d := m.syncDlg

	// Move focus to the editbox (after the one checkbox) and type a custom
	// destination; j/k must type, not navigate, while in the editbox.
	m.Update(key("tab"))
	if d.focus != len(d.selectable) {
		t.Fatalf("focus = %d", d.focus)
	}
	for _, r := range "kiwi:tank/bak/git" {
		m.Update(key(string(r)))
	}
	if d.custom.Value() != "kiwi:tank/bak/git" {
		t.Fatalf("custom = %q", d.custom.Value())
	}
	m.Update(key("enter"))
	if m.modal == nil {
		t.Fatalf("expected dry-run modal, err=%q", d.errMsg)
	}
	content := strings.Join(m.modal.lines, "\n")
	for _, want := range []string{
		"kiwi:tank/bak/git (full send)",
		`zfs send -L -c 'tank/srv/git@20260611'`,    // full, not incremental
		`zfs send -L -c -I 'tank/srv/git@20260501'`, // the checked group member too
	} {
		if !strings.Contains(content, want) {
			t.Errorf("modal missing %q:\n%s", want, content)
		}
	}
}

func TestSyncDialogCustomRejectsExisting(t *testing.T) {
	m := syncTestModel(t)
	m.moveCursorToCol(t, "ws:tank")
	m.openSyncDialog()
	m.Update(key("tab"))
	for _, r := range "dev:tank/bak/git" { // exists (the diverged replica)
		m.Update(key(string(r)))
	}
	m.Update(key("enter"))
	if m.modal != nil || m.syncDlg == nil ||
		!strings.Contains(m.syncDlg.errMsg, "already exists") {
		t.Fatalf("existing dataset not rejected: %+v", m.syncDlg)
	}
}

func TestSyncDialogSingleReplicaGroup(t *testing.T) {
	m := dialogTestModel(t) // one host, one replica
	m.curRow, m.curCol = 0, 1
	m.openSyncDialog()
	if m.syncDlg == nil {
		t.Fatal("dialog must open even with no other group members")
	}
	if len(m.syncDlg.selectable) != 0 {
		t.Fatalf("selectable: %v", m.syncDlg.selectable)
	}
	// Editbox is focused from the start; type and submit (dry run).
	for _, r := range "dev2:ssd/bak/git" {
		m.Update(key(string(r)))
	}
	m.Update(key("enter"))
	if m.modal == nil {
		t.Fatalf("expected dry-run modal, err=%q", m.syncDlg.errMsg)
	}
	if !strings.Contains(strings.Join(m.modal.lines, "\n"), "full send") {
		t.Error("modal should describe a full send")
	}
}

func TestJobProgressInTUI(t *testing.T) {
	m := syncTestModel(t)
	// Inject a running sync job on ws targeting cml's replica.
	for i := range m.data.States {
		if m.data.States[i].Host == "ws" {
			m.data.States[i].Jobs = []JobInfo{{
				Host: "ws", ID: "20260611T100000Z-aa11", Kind: "sync", State: "running",
				Meta:     map[string]string{"src": "ws:tank/srv/git", "dest": "cml:root/bak/git"},
				Progress: 500, Expected: 1000,
			}}
		}
	}
	m.data.JobsByReplica = indexJobs(m.data.States)

	// Receiving cell shows a spinner.
	m.moveCursorToCol(t, "cml:root")
	view := m.View()
	if !strings.Contains(view, spinnerFrames[0]) {
		t.Error("table should show a spinner on the receiving cell")
	}
	// Details show the job row with its percentage.
	if !strings.Contains(view, "50%") || !strings.Contains(view, "20260611T100000Z-aa11") {
		t.Errorf("details should show the running job row:\n%s", view)
	}
	// Enter on the job row (details focused) opens the job modal with full progress.
	m.Update(key("tab")) // focus details; snaps to first selectable row (the job)
	m.Update(key("enter"))
	if m.modal == nil {
		t.Fatal("Enter should open the job modal")
	}
	if !strings.Contains(strings.Join(m.modal.lines, "\n"), "500B / 1000B (50%)") {
		t.Errorf("job modal missing progress:\n%s", strings.Join(m.modal.lines, "\n"))
	}
	m.Update(key("esc"))
	m.Update(key("tab")) // back to table focus
	// Ticker machinery engages.
	if hosts := m.runningJobHosts(); len(hosts) != 1 || hosts[0] != "ws" {
		t.Errorf("runningJobHosts: %v", hosts)
	}
	if cmd := m.ensureTicker(); cmd == nil || !m.ticking {
		t.Error("ticker should start with running jobs")
	}

	// Poll transition running→done triggers refresh of affected hosts.
	_, cmd := m.onJobsPoll(jobsPollMsg{host: "ws", jobs: []JobInfo{{
		Host: "ws", ID: "20260611T100000Z-aa11", Kind: "sync", State: "done",
		Meta: map[string]string{"src": "ws:tank/srv/git", "dest": "cml:root/bak/git"},
	}}})
	if cmd == nil || !m.refreshing {
		t.Error("job completion should trigger a refresh")
	}
}

// Field panic (occasional): refresh completing while the cursor is on a
// header row (curRow -1/-2) indexed m.rows[-1] in reload.
func TestReloadWithHeaderCursor(t *testing.T) {
	m := syncTestModel(t)
	for _, row := range []int{-1, -2} {
		m.curRow, m.curCol = row, 1
		m.clampCursor()
		if err := m.reload(); err != nil {
			t.Fatal(err)
		}
		if m.curRow != row {
			t.Errorf("header cursor not preserved: %d -> %d", row, m.curRow)
		}
		_ = m.View() // and render fine
	}
}

func TestEnterOpensDetailModals(t *testing.T) {
	m := syncTestModel(t)
	// Cell with one replica → dataset modal.
	m.moveCursorToCol(t, "ws:tank")
	m.Update(key("enter"))
	if m.modal == nil || !strings.Contains(m.modal.title, "ws:tank/srv/git") {
		t.Fatalf("dataset modal expected, got %+v", m.modal)
	}
	m.Update(key("esc"))
	// Host header row → host modal.
	m.curRow = -2
	m.clampCursor()
	m.Update(key("enter"))
	if m.modal == nil || !strings.Contains(m.modal.title, "host ") {
		t.Fatalf("host modal expected, got %+v", m.modal)
	}
	if !strings.Contains(strings.Join(m.modal.lines, "\n"), "ssh:") {
		t.Errorf("host modal content:\n%s", strings.Join(m.modal.lines, "\n"))
	}
	m.Update(key("esc"))
	// Group row → focus jump (no single modal).
	m.curRow, m.curCol = 0, 0
	m.Update(key("enter"))
	if m.modal != nil || m.focus != 1 {
		t.Fatalf("group row should jump to details: modal=%v focus=%d", m.modal, m.focus)
	}
}

func TestDiskDataFlow(t *testing.T) {
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hs := HostState{
		Host: "sta",
		Pools: []Pool{{Host: "sta", Name: "sta-tank", Size: 100, Alloc: 40, Free: 60, Health: "ONLINE",
			Props: map[string]string{"size": "100", "allocated": "40", "free": "60", "health": "ONLINE",
				"_zpool-status": "  pool: sta-tank\n state: ONLINE\n  scan: scrub repaired 0B in 11:11:17 with 0 errors\nerrors: No known data errors"}}},
		Datasets:  []Dataset{{Host: "sta", Name: "sta-tank/bak/git", Type: "filesystem", GUID: "d1"}},
		Snapshots: []Snapshot{{Host: "sta", Dataset: "sta-tank/bak/git", Name: "s", GUID: "g1", Creation: 1}},
		VDevs: []VDev{
			{Host: "sta", Pool: "sta-tank", Name: "root-0", State: "ONLINE"},
			{Host: "sta", Pool: "sta-tank", Name: "raidz1-0", Parent: "root-0", State: "ONLINE"},
			{Host: "sta", Pool: "sta-tank", Name: "HG10-2JNX", Parent: "raidz1-0", DiskID: "serial:E4GG2JNX", State: "ONLINE", CErr: 3, Size: 1 << 40},
		},
		Disks: []Disk{{Host: "sta", ID: "serial:E4GG2JNX", Dev: "/dev/sda", KName: "sda",
			Model: "HGST HUH721010ALE600", Serial: "E4GG2JNX", Size: 10 << 40,
			Transport: "sata", Rotational: true, Smart: "SMART overall-health: PASSED"}},
	}
	if err := st.PutHostState(hs); err != nil {
		t.Fatal(err)
	}
	m, err := New(st, []Host{{Name: "sta"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	m.width, m.height = 110, 30

	// Pool view: vdev tree with error highlighted disk + scan line.
	m.curRow, m.curCol = -1, 1
	m.clampCursor()
	view := m.View()
	for _, want := range []string{"VDEV", "DISK", "HG10-2JNX", "/dev/sda", "* scrub repaired"} {
		if !strings.Contains(view, want) {
			t.Errorf("pool view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "raidz1-0") {
		t.Errorf("pool view should show leaf vdev rows only:\n%s", view)
	}
	// Enter on pool header → pool modal with verbatim status.
	m.Update(key("enter"))
	if m.modal == nil || !strings.Contains(strings.Join(m.modal.lines, "\n"), "scrub repaired 0B in 11:11:17") {
		t.Fatalf("pool modal verbatim missing: %+v", m.modal)
	}
	m.Update(key("esc"))

	// Host modal: ssh+refresh summary and disk inventory (SMART opens on disk).
	m.curRow = -2
	m.clampCursor()
	m.Update(key("enter"))
	content := strings.Join(m.modal.lines, "\n")
	for _, want := range []string{"ssh: root@sta", "job(s) running", "disk /dev/sda", "E4GG2JNX", "sata hdd"} {
		if !strings.Contains(content, want) {
			t.Errorf("host modal missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "zpool status") || strings.Contains(content, "smartctl -a") {
		t.Error("host modal should not include pool or SMART verbatims")
	}
	m.Update(key("esc"))

	// Vdev row Enter → vdev modal (navigate into details on pool view).
	m.curRow = -1
	m.clampCursor()
	m.Update(key("tab"))
	m.Update(key("j")) // pool line -> first vdev row
	m.Update(key("enter"))
	if m.modal == nil || !strings.Contains(m.modal.title, "vdev sta-tank/") {
		t.Fatalf("vdev modal expected: %+v", m.modal)
	}
	m.Update(key("esc"))

	// Host details disk row Enter -> disk modal with SMART.
	m.curRow = -2
	m.clampCursor()
	m.focus = 1
	m.detCur, m.detOff = 0, 0
	m.detSnap()
	for i := 0; i < 6 && (m.modal == nil || !strings.Contains(m.modal.title, "disk ")); i++ {
		m.Update(key("j"))
		m.Update(key("enter"))
		if m.modal == nil || !strings.Contains(m.modal.title, "disk ") {
			m.Update(key("esc"))
		}
	}
	if m.modal == nil || !strings.Contains(strings.Join(m.modal.lines, "\n"), "smartctl -a /dev/sda") {
		t.Fatalf("disk modal expected: %+v", m.modal)
	}
}

func TestMainTableStyling(t *testing.T) {
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hs1 := HostState{
		Host:  "src",
		Pools: []Pool{{Host: "src", Name: "tank", Health: "ONLINE"}},
		Datasets: []Dataset{
			{Host: "src", Name: "tank/git", Type: "filesystem", GUID: "src-git"},
		},
		Snapshots: []Snapshot{
			{Host: "src", Dataset: "tank/git", Name: "old", GUID: "g1", Creation: 1},
			{Host: "src", Dataset: "tank/git", Name: "new", GUID: "g2", Creation: 2},
		},
	}
	hs2 := HostState{
		Host: "dst",
		Pools: []Pool{{Host: "dst", Name: "tank", Health: "ONLINE",
			Props: map[string]string{"health": "ONLINE", "_zpool-status": "errors: No known data errors"}}},
		Datasets: []Dataset{
			{Host: "dst", Name: "tank/git", Type: "filesystem", GUID: "dst-git"},
		},
		Snapshots: []Snapshot{
			{Host: "dst", Dataset: "tank/git", Name: "old", GUID: "g1", Creation: 1},
		},
		VDevs: []VDev{{Host: "dst", Pool: "tank", Name: "sda1", State: "ONLINE", CErr: 1}},
	}
	if err := st.PutHostState(hs1); err != nil {
		t.Fatal(err)
	}
	if err := st.PutHostState(hs2); err != nil {
		t.Fatal(err)
	}
	m, err := New(st, []Host{{Name: "src"}, {Name: "dst"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	m.width, m.height = 100, 24
	view := m.View()
	if strings.Contains(view, "Datasets") {
		t.Fatalf("main pane title should be gone:\n%s", view)
	}
	if !strings.Contains(view, styleDetRow.Render("old")) {
		t.Fatalf("behind replica should render gray:\n%s", view)
	}
	if !strings.Contains(view, styleErr.Render("tank")) {
		t.Fatalf("pool header with vdev errors should render red:\n%s", view)
	}
	if !poolProblem(nil, &Pool{Health: "DEGRADED"}) {
		t.Fatal("degraded pool should be treated as a problem")
	}
}

func TestTitleFillStyles(t *testing.T) {
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := &Model{st: st, width: 80}
	title := m.paneTitle([]string{"Details"}, true)
	if !strings.Contains(title, stylePaneActive.Render("  Details  ")) {
		t.Fatalf("active pane title should use filled style: %q", title)
	}
	if strings.Contains(title, "[ Details ]") {
		t.Fatalf("active pane title should drop brackets: %q", title)
	}
	box := dialogBox("Dialog", strings.Repeat("body ", 20))
	if !strings.Contains(box, styleDialogTitle.Render(" Dialog ")) {
		t.Fatalf("dialog title should use filled style: %q", box)
	}
}

func TestPropsDumpLayout(t *testing.T) {
	lines := propsDump([]string{"host      cml"}, map[string]string{
		"guid":   "123",
		"health": "ONLINE",
	}, map[string]string{"guid": "default"})
	var foundHeader, foundGuid bool
	for _, ln := range lines {
		if strings.HasPrefix(ln, "── properties ") {
			foundHeader = true
		}
		if strings.HasPrefix(ln, "guid") {
			foundGuid = true
			if strings.HasPrefix(ln, "  ") {
				t.Fatalf("property rows should not be indented: %q", ln)
			}
		}
	}
	if !foundHeader {
		t.Fatalf("missing properties header: %v", lines)
	}
	if !foundGuid {
		t.Fatalf("missing guid row: %v", lines)
	}
}
