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
