package main

import (
	"regexp"
	"strings"
	"testing"
)

// The data-safety allowlist (DESIGN.md "Data safety (hard rules)"): every
// mutating command line the tool can emit must match one of these shapes.
// Note what is absent: recv -F, rollback, destroy of datasets, destroy -r/-R.
var allowlistRes = []*regexp.Regexp{
	regexp.MustCompile(`^(sudo -n )?zfs snapshot( -r)? '[^';|&$<>\\]+'$`),
	regexp.MustCompile(`^(sudo -n )?zfs send (-w|-L -c) -[iI] '[^';|&$<>\\]+@[^';|&$<>\\]+' '[^';|&$<>\\]+@[^';|&$<>\\]+'$`),
	regexp.MustCompile(`^(sudo -n )?zfs send (-w|-L -c) '[^';|&$<>\\]+@[^';|&$<>\\]+'$`), // full send (new dest)
	regexp.MustCompile(`^(sudo -n )?zfs send -t \$TOKEN$`),                               // TOKEN='…' set by resumeTokenAssignRe
	// -nP sizing twins are no-op/read-only (never generate stream data):
	regexp.MustCompile(`^(sudo -n )?zfs send -nP (-w |-L -c )?(-[iI] )?'[^']+'( '[^']+')?$`),
	regexp.MustCompile(`^(sudo -n )?zfs send -nP -t \$TOKEN$`),
	regexp.MustCompile(`^(sudo -n )?zfs recv -s -u( -o encryption=off)? '[^';|&$<>\\]+'$`),
	regexp.MustCompile(`^(sudo -n )?zfs receive -A '[^';|&$<>\\]+'$`),
	regexp.MustCompile(`^(sudo -n )?zfs destroy '[^'@;|&$<>\\]+@[^';|&$<>\\]+'$`),
}

// resumeTokenAssignRe is the only shape a TOKEN= line (the operand of the
// `zfs send -t $TOKEN` shapes above) may take in scripts/manual commands.
var resumeTokenAssignRe = regexp.MustCompile(`^TOKEN='[A-Za-z0-9-]+'$`)

func allowlisted(cmd string) bool {
	for _, re := range allowlistRes {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}

func TestSnapshotCmdAllowlist(t *testing.T) {
	for _, c := range []struct {
		sudo      bool
		dataset   string
		snap      string
		recursive bool
	}{
		{false, "tank/srv/git", "20260611", false},
		{true, "tank/srv/git", "20260611", true},
		{true, "tank/bak/blobs/01", "pre-upgrade_2", false},
		{false, "tank/data with space", "a:b.c-d", false},
	} {
		cmd, err := buildSnapshotCmd(c.sudo, c.dataset, c.snap, c.recursive)
		if err != nil {
			t.Errorf("%+v: %v", c, err)
			continue
		}
		if !allowlisted(cmd) {
			t.Errorf("command outside allowlist: %q", cmd)
		}
	}
}

func TestSnapshotCmdShape(t *testing.T) {
	cmd, err := buildSnapshotCmd(true, "tank/srv/git", "20260611", true)
	if err != nil {
		t.Fatal(err)
	}
	if cmd != `sudo -n zfs snapshot -r 'tank/srv/git@20260611'` {
		t.Errorf("got %q", cmd)
	}
	cmd, _ = buildSnapshotCmd(false, "tank/srv/git", "20260611", false)
	if cmd != `zfs snapshot 'tank/srv/git@20260611'` {
		t.Errorf("got %q", cmd)
	}
}

func TestSnapshotCmdRejectsBadInput(t *testing.T) {
	bad := []struct{ dataset, snap string }{
		{"tank/srv/git", ""},
		{"tank/srv/git", "with space"},
		{"tank/srv/git", "$(boom)"},
		{"tank/srv/git", "a;b"},
		{"tank/srv/git", "-leadingdash"},
		{"tank/srv/git", "name\nnewline"},
		{"tank;rm -rf /", "ok"},
		{"tank/`boom`", "ok"},
		{"tank//x", "ok"},
		{"", "ok"},
	}
	for _, c := range bad {
		if _, err := buildSnapshotCmd(false, c.dataset, c.snap, false); err == nil {
			t.Errorf("accepted bad input %+v", c)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if q := shellQuote(`it's`); q != `'it'\''s'` {
		t.Errorf("got %q", q)
	}
	if q := shellQuote("plain"); q != "'plain'" {
		t.Errorf("got %q", q)
	}
}

func TestQuotedPayloadNeverBreaksOut(t *testing.T) {
	// Even if validation were bypassed, quoting must contain the payload.
	q := shellQuote(`a'; rm -rf / #`)
	if strings.Contains(q, `'; rm`) && !strings.Contains(q, `'\''`) {
		t.Errorf("quote escape failed: %q", q)
	}
}

func TestSendRecvCmdShapes(t *testing.T) {
	send, err := buildSendCmd(true, true, true, "tank/srv/git", "20260501", "20260611")
	if err != nil {
		t.Fatal(err)
	}
	if send != `sudo -n zfs send -w -I 'tank/srv/git@20260501' 'tank/srv/git@20260611'` {
		t.Errorf("send = %q", send)
	}
	if !allowlisted(send) {
		t.Errorf("send not allowlisted: %q", send)
	}
	// Unencrypted source: -L -c, never -w (would imply -e: unreceivable
	// into encrypted destinations).
	send, _ = buildSendCmd(false, false, false, "tank/srv/git", "a", "b")
	if send != `zfs send -L -c -i 'tank/srv/git@a' 'tank/srv/git@b'` {
		t.Errorf("send -i = %q", send)
	}
	if !allowlisted(send) {
		t.Errorf("plain send not allowlisted: %q", send)
	}
	full, _ := buildFullSendCmd(false, false, "tank/x", "s1")
	if full != `zfs send -L -c 'tank/x@s1'` || !allowlisted(full) {
		t.Errorf("plain full = %q", full)
	}

	assign, resume, err := buildResumeSendCmd(false, "1-e604ea4bf-e0-789c63a2")
	if err != nil {
		t.Fatal(err)
	}
	if resume != `zfs send -t $TOKEN` || !allowlisted(resume) || !allowlisted(sizeVariant(resume)) {
		t.Errorf("resume = %q", resume)
	}
	if assign != `TOKEN='1-e604ea4bf-e0-789c63a2'` || !resumeTokenAssignRe.MatchString(assign) {
		t.Errorf("assign = %q", assign)
	}

	recv, err := buildRecvCmd(true, "tank/bak/git", false)
	if err != nil {
		t.Fatal(err)
	}
	if recv != `sudo -n zfs recv -s -u 'tank/bak/git'` || !allowlisted(recv) {
		t.Errorf("recv = %q", recv)
	}
	recv, _ = buildRecvCmd(false, "tank/bak/git", true)
	if recv != `zfs recv -s -u -o encryption=off 'tank/bak/git'` || !allowlisted(recv) {
		t.Errorf("plain recv = %q", recv)
	}
}

func TestSendRecvCmdRejectsBadInput(t *testing.T) {
	if _, err := buildSendCmd(false, true, true, "tank/x", "ok", "$(boom)"); err == nil {
		t.Error("bad target snap accepted")
	}
	if _, err := buildSendCmd(false, true, true, "tank/x", "a;b", "ok"); err == nil {
		t.Error("bad base snap accepted")
	}
	if _, _, err := buildResumeSendCmd(false, "short"); err == nil {
		t.Error("short token accepted")
	}
	if _, _, err := buildResumeSendCmd(false, "1-abc'; rm -rf /;'x"); err == nil {
		t.Error("evil token accepted")
	}
	if _, err := buildRecvCmd(false, "tank/`boom`", false); err == nil {
		t.Error("bad recv dataset accepted")
	}
}
