package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// syncFixture builds a group with one source and a zoo of destinations.
func syncFixture(t *testing.T) (*Replica, *Group, map[string][]Snapshot) {
	t.Helper()
	snapsBy := map[string][]Snapshot{}
	mk := func(host, name string, written int64, origin, token string, snaps ...Snapshot) *Replica {
		r := &Replica{Dataset: &Dataset{
			Host: host, Name: name, Type: "filesystem",
			Written: written, Origin: origin, ResumeToken: token,
			Encryption: "aes-256-gcm",
		}, Written: written}
		snapsBy[host+":"+name] = snaps
		return r
	}
	sn := func(host, ds, name, guid string, creation int64) Snapshot {
		return Snapshot{Host: host, Dataset: ds, Name: name, GUID: guid, Creation: creation}
	}

	src := mk("ws", "tank/srv/git", 0, "", "",
		sn("ws", "tank/srv/git", "20260501", "g1", 100),
		sn("ws", "tank/srv/git", "20260601", "g2", 200),
		sn("ws", "tank/srv/git", "20260611", "g3", 300))

	behind := mk("cml", "root/bak/git", 0, "", "",
		sn("cml", "root/bak/git", "20260501", "g1", 100))
	// Same data, different snapshot NAME on the destination: base must be
	// resolved via GUID to the source-side name.
	renamed := mk("nas", "tank/bak/git-old", 0, "", "",
		sn("nas", "tank/bak/git-old", "before-rename", "g2", 200))
	uptodate := mk("hetzner", "tank/bak/git", 0, "", "",
		sn("hetzner", "tank/bak/git", "20260611", "g3", 300))
	diverged := mk("dev", "tank/bak/git", 0, "", "",
		sn("dev", "tank/bak/git", "20260501", "g1", 100),
		sn("dev", "tank/bak/git", "own", "gX", 250))
	dirty := mk("dirty", "tank/bak/git", 4096, "", "",
		sn("dirty", "tank/bak/git", "20260601", "g2", 200))
	resum := mk("ws", "hdd/bak/git", 0, "", "1-abcdef0123-4567",
		sn("ws", "hdd/bak/git", "20260501", "g1", 100))
	clone := mk("ws", "tank/srv/git-exp", 100, "tank/srv/git@20260501", "")
	nosnaps := mk("empty", "tank/bak/git", 0, "", "")

	g := &Group{Members: []*Replica{src, behind, renamed, uptodate, diverged, dirty, resum, clone, nosnaps}}
	return src, g, snapsBy
}

func planFor(t *testing.T, plans []syncPlan, host string) syncPlan {
	t.Helper()
	for _, p := range plans {
		if p.Dest.Dataset.Host == host {
			return p
		}
	}
	t.Fatalf("no plan for host %q", host)
	return syncPlan{}
}

func TestPlanSync(t *testing.T) {
	src, g, snapsBy := syncFixture(t)
	plans, err := planSync(src, g, snapsBy)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 8 {
		t.Fatalf("got %d plans", len(plans))
	}

	p := planFor(t, plans, "cml") // behind
	if p.Kind != planIncremental || p.BaseName != "20260501" || p.TargetName != "20260611" {
		t.Errorf("behind: %+v", p)
	}

	p = planFor(t, plans, "nas") // renamed base snapshot
	if p.Kind != planIncremental || p.BaseName != "20260601" {
		t.Errorf("renamed dest must resolve base via GUID to the source-side name: %+v", p)
	}

	p = planFor(t, plans, "hetzner")
	if p.Kind != planUpToDate {
		t.Errorf("uptodate: %+v", p)
	}

	p = planFor(t, plans, "dev")
	if p.Kind != planIncompatible || !strings.Contains(p.Reason, "diverged") {
		t.Errorf("diverged: %+v", p)
	}

	p = planFor(t, plans, "dirty")
	if p.Kind != planIncompatible || !strings.Contains(p.Reason, "local writes") {
		t.Errorf("dirty: %+v", p)
	}

	resumePlans := 0
	clonePlans := 0
	emptyPlans := 0
	for _, p := range plans {
		switch {
		case p.Kind == planResume:
			resumePlans++
		case p.Dest.Dataset.Origin != "":
			clonePlans++
			if p.Kind != planIncompatible {
				t.Errorf("clone: %+v", p)
			}
		case p.Dest.Dataset.Host == "empty":
			emptyPlans++
			if p.Kind != planIncompatible || !strings.Contains(p.Reason, "no snapshots") {
				t.Errorf("empty: %+v", p)
			}
		}
	}
	if resumePlans != 1 || clonePlans != 1 || emptyPlans != 1 {
		t.Errorf("resume=%d clone=%d empty=%d", resumePlans, clonePlans, emptyPlans)
	}
}

func TestPlanSyncNoSourceSnapshots(t *testing.T) {
	src, g, snapsBy := syncFixture(t)
	delete(snapsBy, "ws:tank/srv/git")
	if _, err := planSync(src, g, snapsBy); err == nil {
		t.Fatal("want error for snapshot-less source")
	}
}

func TestSyncJobRemote(t *testing.T) {
	src, g, snapsBy := syncFixture(t)
	plans, _ := planSync(src, g, snapsBy)
	p := planFor(t, plans, "cml")

	spec, err := syncJob(Host{Name: "ws"}, Host{Name: "cml", User: "ivan"}, src, p, true)
	if err != nil {
		t.Fatal(err)
	}
	if !spec.WaitStart {
		t.Error("remote sync must use WaitStart (agent window)")
	}
	cmd := spec.Command
	for _, want := range []string{
		`zfs send -w -I 'tank/srv/git@20260501' 'tank/srv/git@20260611'`, // root src: no sudo
		`$DSSH "sudo -n zfs recv -s -u 'root/bak/git'"`,                  // ivan dest: sudo
		"ControlMaster=auto", "ControlPersist=600", "ivan@cml",
		"touch ssh-ok", "mbuffer", `$DSSH -O exit`,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "-F") {
		t.Error("recv -F must never appear")
	}
	assertScriptAllowlisted(t, spec.script())

	// Generated script must be valid bash.
	f := filepath.Join(t.TempDir(), "job.sh")
	if err := os.WriteFile(f, []byte(spec.script()), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", f).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}

	// Bootstrap must wait for ssh-ok.
	if !strings.Contains(spec.bootstrap(), "ssh-ok") {
		t.Errorf("bootstrap without start-wait: %q", spec.bootstrap())
	}
}

func TestSyncJobLocal(t *testing.T) {
	src, g, snapsBy := syncFixture(t)
	plans, _ := planSync(src, g, snapsBy)
	p := planFor(t, plans, "ws") // the resumable replica is on the same host

	spec, err := syncJob(Host{Name: "ws"}, Host{Name: "ws"}, src, p, true)
	if err != nil {
		t.Fatal(err)
	}
	if spec.WaitStart {
		t.Error("local sync needs no start-wait")
	}
	cmd := spec.Command
	if strings.Contains(cmd, "ssh") {
		t.Errorf("local sync must not use ssh:\n%s", cmd)
	}
	// Resume token path.
	if !strings.Contains(cmd, `zfs send -t '1-abcdef0123-4567'`) ||
		!strings.Contains(cmd, `zfs recv -s -u 'hdd/bak/git'`) {
		t.Errorf("resume pipeline:\n%s", cmd)
	}
	assertScriptAllowlisted(t, spec.script())
}

func TestSyncJobIntermediateFlag(t *testing.T) {
	src, g, snapsBy := syncFixture(t)
	plans, _ := planSync(src, g, snapsBy)
	p := planFor(t, plans, "cml")
	spec, err := syncJob(Host{Name: "ws"}, Host{Name: "cml"}, src, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spec.Command, "zfs send -w -i ") {
		t.Errorf("want -i:\n%s", spec.Command)
	}
}

func TestSyncJobRefusesUnselectable(t *testing.T) {
	src, g, snapsBy := syncFixture(t)
	plans, _ := planSync(src, g, snapsBy)
	p := planFor(t, plans, "dev") // diverged
	if _, err := syncJob(Host{Name: "ws"}, Host{Name: "dev"}, src, p, true); err == nil {
		t.Fatal("must refuse to build a job for an incompatible plan")
	}
}

func TestSyncJobFullSend(t *testing.T) {
	src, _, snapsBy := syncFixture(t)
	p, err := customDestPlan(src, snapsBy, "newhost", "backup/git")
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != planFull || p.TargetName != "20260611" {
		t.Fatalf("plan: %+v", p)
	}
	spec, err := syncJob(Host{Name: "ws"}, Host{Name: "newhost"}, src, p, true)
	if err != nil {
		t.Fatal(err)
	}
	cmd := spec.Command
	// Encrypted source: raw full send, recv plain (-o not needed/allowed).
	if !strings.Contains(cmd, `zfs send -w 'tank/srv/git@20260611'`) ||
		!strings.Contains(cmd, `zfs recv -s -u 'backup/git'`) {
		t.Errorf("full send pipeline:\n%s", cmd)
	}
	if strings.Contains(cmd, "encryption=off") {
		t.Errorf("raw full send must not force encryption=off:\n%s", cmd)
	}
	if strings.Contains(cmd, "send -w -I") || strings.Contains(cmd, "send -w -i") {
		t.Errorf("full send must not be incremental:\n%s", cmd)
	}
	if !strings.Contains(cmd, "> expected") || !strings.Contains(cmd, "2>>progress") {
		t.Errorf("missing progress instrumentation:\n%s", cmd)
	}
	assertScriptAllowlisted(t, spec.script())
}

func TestSyncJobUnencryptedSourceUsesLc(t *testing.T) {
	src, g, snapsBy := syncFixture(t)
	src.Dataset.Encryption = "off" // unencrypted source
	plans, _ := planSync(src, g, snapsBy)
	p := planFor(t, plans, "cml")
	spec, err := syncJob(Host{Name: "ws"}, Host{Name: "cml"}, src, p, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spec.Command, "zfs send -L -c -I ") {
		t.Errorf("unencrypted source must use -L -c:\n%s", spec.Command)
	}
	if strings.Contains(spec.Command, "send -w") {
		t.Errorf("-w must not appear for unencrypted source:\n%s", spec.Command)
	}
	assertScriptAllowlisted(t, spec.script())
}

func TestSyncJobFullSendUnencryptedForcesPlain(t *testing.T) {
	src, _, snapsBy := syncFixture(t)
	src.Dataset.Encryption = "off"
	p, err := customDestPlan(src, snapsBy, "ws", "ws-tank/bak/ws-boot")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := syncJob(Host{Name: "ws"}, Host{Name: "ws"}, src, p, true)
	if err != nil {
		t.Fatal(err)
	}
	// The field bug: encrypted dest parent demanded its key for the new
	// child. Unencrypted source => create the child unencrypted.
	if !strings.Contains(spec.Command, `zfs recv -s -u -o encryption=off 'ws-tank/bak/ws-boot'`) {
		t.Errorf("missing -o encryption=off:\n%s", spec.Command)
	}
	assertScriptAllowlisted(t, spec.script())
}

func TestCustomDestPlanRejectsBadDataset(t *testing.T) {
	src, _, snapsBy := syncFixture(t)
	if _, err := customDestPlan(src, snapsBy, "h", "tank/$(boom)"); err == nil {
		t.Fatal("bad dataset accepted")
	}
}
